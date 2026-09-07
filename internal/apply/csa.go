/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apply

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/render"
)

// ClientSideApplier merges in the operator and writes with an optimistic-concurrency Update.
//
// It exists because server-side apply's granularity is only as fine as the target's schema. A list
// without listType=map and listMapKey is atomic: one manager owns the whole list, so a second
// contributor cannot append to it. Ingress.spec.rules is exactly that, which makes this mode the
// one the primary use case needs rather than a legacy fallback.
//
// The cost is real bookkeeping. This applier tracks the paths a contributor owns and the values
// those paths held before it first touched them, so a revert restores a pre-existing setting
// rather than deleting the field — something SSA cannot express.
type ClientSideApplier struct {
	Client client.Client
}

var _ Applier = &ClientSideApplier{}

func (a *ClientSideApplier) Mode() patchv1alpha1.ApplyMode {
	return patchv1alpha1.ApplyModeClientSideApply
}

func (a *ClientSideApplier) Apply(ctx context.Context, req Request) (*Result, error) {
	if req.Contribution == nil {
		return nil, fmt.Errorf("contribution is nil")
	}

	live := targetObject(req.Target)
	err := a.Client.Get(ctx, targetKey(req.Target), live)
	switch {
	case apierrors.IsNotFound(err):
		if !req.AllowCreate {
			return nil, apierrors.NewNotFound(schemaGroupResource(req.Target), req.Target.Name)
		}
		return a.create(ctx, req)
	case err != nil:
		return nil, err
	}

	return a.update(ctx, req, live)
}

// create builds the target from the base plus this contribution and creates it.
//
// A concurrent creator makes this fail with AlreadyExists, which is an expected outcome rather
// than an error: the caller requeues and the next pass takes the update path.
func (a *ClientSideApplier) create(ctx context.Context, req Request) (*Result, error) {
	content := render.DeepCopyMap(req.Contribution.Object)
	if req.Base != nil {
		merged, err := mergeBaseAndContribution(req.Base, req.Contribution.Object)
		if err != nil {
			return nil, err
		}
		content = merged
	}

	obj := &unstructured.Unstructured{}
	obj.SetUnstructuredContent(content)
	obj.SetAPIVersion(req.Target.APIVersion)
	obj.SetKind(req.Target.Kind)
	obj.SetName(req.Target.Name)
	if req.Target.Namespace != "" {
		obj.SetNamespace(req.Target.Namespace)
	}

	if err := a.Client.Create(ctx, obj); err != nil {
		return nil, err
	}
	return &Result{
		Created:    true,
		Changed:    true,
		Live:       obj,
		OwnedPaths: req.Contribution.Paths,
		// Nothing pre-existed a target this contributor just created, so there is nothing to
		// restore on revert -- and the capture is finished, which the flag has to say explicitly
		// or the next apply would record this contributor's own values as the priors.
		PriorValues:         map[string]any{},
		PriorValuesCaptured: true,
	}, nil
}

// update merges the contribution into the live object and writes it back under a resourceVersion
// precondition, so a concurrent writer causes a retryable conflict rather than a lost update.
func (a *ClientSideApplier) update(
	ctx context.Context,
	req Request,
	live *unstructured.Unstructured,
) (*Result, error) {
	desired := live.DeepCopy()

	// Capture what the target holds at each claimed path *before* touching it, and only ever
	// once. Re-capturing on a later apply would read back this contributor's own value and record
	// it as the prior, so revert would restore the contribution instead of withdrawing it -- a
	// silent no-op, the target keeping a field whose contributor is gone.
	//
	// "Already captured" cannot be inferred from a non-empty PriorValues: a contributor whose
	// paths pre-existed nowhere legitimately captures nothing. Hence the explicit flag.
	priors := map[string]any{}
	for k, v := range req.PriorValues {
		priors[k] = v
	}
	if !req.PriorValuesCaptured {
		for _, p := range req.Contribution.Paths {
			if _, known := priors[p.String()]; known {
				// A prior already recorded for this path is the true one. Replacing it with what
				// the target holds now would substitute a later value -- possibly this
				// contributor's own -- for the one that actually pre-existed.
				continue
			}
			existing, found := render.Get(live.Object, p)
			if !found {
				continue
			}
			// A live value identical to what this contributor is about to write is not a prior,
			// whoever put it there. This guard is not redundant with the flag above: the flag
			// lives in the tracker's status, so a status write that is lost or conflicts leaves a
			// second apply believing it is the first -- and it would then read back its own value
			// and record that as the prior. Revert would restore the contribution instead of
			// withdrawing it, which is a silent leak with no error anywhere. Comparing values
			// closes that regardless of write ordering.
			if want, ok := render.Get(req.Contribution.Object, p); ok && sameValue(existing, want) {
				continue
			}
			priors[p.String()] = render.DeepCopyValue(existing)
		}
	}

	// Withdraw paths this contributor owned last time but no longer claims. Without this, a
	// shrinking contribution would leave orphaned fields nobody owns and nobody can find.
	claimed := make(map[string]bool, len(req.Contribution.Paths))
	for _, p := range req.Contribution.Paths {
		claimed[p.String()] = true
	}
	for _, p := range req.PreviousPaths {
		if claimed[p.String()] {
			continue
		}
		restoreOrDelete(desired.Object, p, priors)
		delete(priors, p.String())
	}

	// Apply each claimed path. Going path by path rather than merging whole objects is what keeps
	// a contributor's write confined to what it actually declared.
	for _, p := range req.Contribution.Paths {
		value, found := render.Get(req.Contribution.Object, p)
		if !found {
			return nil, fmt.Errorf("internal: rendered path %q missing from the contribution", p)
		}
		if err := render.Set(desired.Object, p, render.DeepCopyValue(value)); err != nil {
			return nil, fmt.Errorf("setting %q: %w", p, err)
		}
	}

	if equalContent(live.Object, desired.Object) {
		return &Result{
			Changed:             false,
			Live:                live,
			OwnedPaths:          req.Contribution.Paths,
			PriorValues:         priors,
			PriorValuesCaptured: true,
		}, nil
	}

	// The resourceVersion carried over from the Get is the precondition: the API server rejects
	// the write if anyone else changed the object in the meantime.
	if err := a.Client.Update(ctx, desired); err != nil {
		return nil, err
	}

	return &Result{
		Changed:             true,
		Live:                desired,
		OwnedPaths:          req.Contribution.Paths,
		PriorValues:         priors,
		PriorValuesCaptured: true,
	}, nil
}

// Revert withdraws this contributor's paths, restoring prior values where they exist.
//
// Restoring rather than deleting is the whole reason this bookkeeping is kept: a contributor that
// overwrote a pre-existing annotation must put the original back, not remove the field.
func (a *ClientSideApplier) Revert(ctx context.Context, req RevertRequest) error {
	live := targetObject(req.Target)
	err := a.Client.Get(ctx, targetKey(req.Target), live)
	switch {
	case apierrors.IsNotFound(err):
		// Already gone: nothing to withdraw, and releasing the contributor is correct.
		return nil
	case err != nil:
		return err
	}

	desired := live.DeepCopy()
	for _, p := range req.OwnedPaths {
		restoreOrDelete(desired.Object, p, req.PriorValues)
	}

	if equalContent(live.Object, desired.Object) {
		return nil
	}
	return a.Client.Update(ctx, desired)
}

// sameValue compares two decoded JSON values by their canonical encoding.
//
// Not reflect.DeepEqual: the live object's numbers arrive as int64 through unstructured, while a
// rendered contribution's arrive as float64 from json.Unmarshal, so a port number equal in every
// meaningful sense compares unequal. Marshalling normalises both, and Go sorts map keys.
func sameValue(a, b any) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// restoreOrDelete puts back the prior value at p if one was recorded, otherwise removes the path.
func restoreOrDelete(obj map[string]any, p render.Path, priors map[string]any) {
	if prior, had := priors[p.String()]; had {
		// Set cannot fail for a path that was previously set successfully, but ignoring the error
		// would silently skip a restore, so fall back to deleting rather than leaving this
		// contributor's value in place.
		if err := render.Set(obj, p, render.DeepCopyValue(prior)); err == nil {
			return
		}
	}
	render.Delete(obj, p)
}

// equalContent compares two objects by their hashed encoding, so an unchanged apply can skip the
// write entirely. Skipping matters: the operator's own writes come back as watch events, and
// writing unconditionally would make that a hot loop.
func equalContent(a, b map[string]any) bool {
	ha, err := render.HashObject(a)
	if err != nil {
		return false
	}
	hb, err := render.HashObject(b)
	if err != nil {
		return false
	}
	return ha == hb
}

// schemaGroupResource builds a GroupResource for NotFound errors. The resource is derived from the
// kind rather than looked up, since this only feeds an error message.
func schemaGroupResource(target render.Target) schema.GroupResource {
	gv, err := schema.ParseGroupVersion(target.APIVersion)
	if err != nil {
		return schema.GroupResource{Resource: target.Kind}
	}
	return schema.GroupResource{Group: gv.Group, Resource: target.Kind}
}
