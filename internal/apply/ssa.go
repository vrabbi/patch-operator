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
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/render"
)

// ServerSideApplier applies each contribution under its own field manager and lets the API server
// do the merging and conflict detection.
//
// Three properties fall out of that, and they are why this is the default: conflict detection is
// the API server's job rather than reimplemented here; a conflict can be reported instead of
// forced; and revert is a single apply of an object claiming nothing, with no bookkeeping to keep
// in sync and no chance of a stale idea of the previous value.
type ServerSideApplier struct {
	Client client.Client
}

var _ Applier = &ServerSideApplier{}

func (a *ServerSideApplier) Mode() patchv1alpha1.ApplyMode {
	return patchv1alpha1.ApplyModeServerSideApply
}

func (a *ServerSideApplier) Apply(ctx context.Context, req Request) (*Result, error) {
	if req.Contribution == nil {
		return nil, fmt.Errorf("contribution is nil")
	}
	if req.FieldManager == "" {
		return nil, fmt.Errorf("field manager is empty")
	}

	existedBefore, err := a.exists(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	if !existedBefore && !req.AllowCreate {
		// An SSA apply would create the object. A patch-only contributor must not, so it waits.
		return nil, apierrors.NewNotFound(
			schemaGroupResource(req.Target), req.Target.Name)
	}

	// Seed the object from the base on a create, so a creating contributor's base and its
	// contribution land in one call rather than two. Under BaseReconcile=CreateOnly the base is
	// never re-asserted, so this only happens while the target is absent.
	obj := &unstructured.Unstructured{}
	if !existedBefore && req.Base != nil {
		merged, err := mergeBaseAndContribution(req.Base, req.Contribution.Object)
		if err != nil {
			return nil, err
		}
		obj.SetUnstructuredContent(merged)
	} else {
		obj.SetUnstructuredContent(render.DeepCopyMap(req.Contribution.Object))
	}

	conflict, err := a.patch(ctx, obj, req.FieldManager, req.ConflictPolicy)
	if err != nil {
		return nil, err
	}
	if conflict != nil {
		return &Result{Conflict: conflict, OwnedPaths: req.Contribution.Paths}, nil
	}

	return &Result{
		Created:    !existedBefore,
		Changed:    true,
		Live:       obj,
		OwnedPaths: req.Contribution.Paths,
	}, nil
}

// patch performs the apply, applying the conflict policy on rejection.
func (a *ServerSideApplier) patch(
	ctx context.Context,
	obj *unstructured.Unstructured,
	manager string,
	policy patchv1alpha1.ConflictPolicy,
) (*Conflict, error) {
	opts := []client.PatchOption{client.FieldOwner(manager)}
	if policy == patchv1alpha1.ConflictPolicyForce {
		opts = append(opts, client.ForceOwnership)
	}

	err := a.Client.Patch(ctx, obj, client.Apply, opts...)
	if err == nil {
		return nil, nil
	}
	if !IsFieldConflict(err) {
		return nil, err
	}

	managers := ConflictingManagers(err)
	c := &Conflict{
		Fields:   ConflictingFields(err),
		Managers: managers,
		AllOurs:  AllOurs(managers),
	}

	switch policy {
	case patchv1alpha1.ConflictPolicyPriority:
		// Force only against other patch-operator managers. Forcing a foreign controller out is a
		// flap, not a resolution: it writes its value straight back and the object oscillates.
		if !c.AllOurs {
			return c, nil
		}
		forced := append(opts, client.ForceOwnership)
		if err := a.Client.Patch(ctx, obj, client.Apply, forced...); err != nil {
			if IsFieldConflict(err) {
				return c, nil
			}
			return nil, err
		}
		return nil, nil

	case patchv1alpha1.ConflictPolicyForce:
		// Already forced above and still conflicting: report rather than loop.
		return c, nil

	default: // ConflictPolicyFail
		return c, nil
	}
}

// Revert releases every field this manager owns, by re-applying an object that claims nothing.
//
// This is the mechanism DESIGN.md 5.1 describes and internal/apply's SSA semantics tests verify
// against a real API server: the manager's entry is dropped and any field no other manager owns is
// deleted outright. There is no reverse patch to compute and no stored previous value that could
// have gone stale.
func (a *ServerSideApplier) Revert(ctx context.Context, req RevertRequest) error {
	if req.FieldManager == "" {
		return fmt.Errorf("field manager is empty")
	}

	obj := &unstructured.Unstructured{}
	obj.SetUnstructuredContent(render.Identity(req.Target))

	err := a.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(req.FieldManager))
	if err == nil {
		return nil
	}
	if apierrors.IsNotFound(err) {
		// The target is already gone, so there is nothing to withdraw. Releasing the contributor
		// is the correct outcome, not an error to retry.
		return nil
	}
	if IsFieldConflict(err) {
		// Claiming nothing cannot conflict with anyone. If the server says otherwise, forcing is
		// safe here because the apply asserts no values.
		return a.Client.Patch(ctx, obj, client.Apply,
			client.FieldOwner(req.FieldManager), client.ForceOwnership)
	}
	return err
}

func (a *ServerSideApplier) exists(ctx context.Context, target render.Target) (bool, error) {
	obj := targetObject(target)
	err := a.Client.Get(ctx, targetKey(target), obj)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

// mergeBaseAndContribution overlays a contribution onto a base seed. The contribution wins on
// every path it sets, so a base can never claw back a field a contributor declares.
func mergeBaseAndContribution(base, contribution map[string]any) (map[string]any, error) {
	out := render.DeepCopyMap(base)
	deepMerge(out, contribution)
	if len(out) == 0 {
		return nil, fmt.Errorf("merging base and contribution produced an empty object")
	}
	return out, nil
}

// deepMerge overlays src onto dst, recursing into maps and replacing everything else.
//
// Lists are replaced rather than merged: element-wise list merging needs a declared key, and at
// this point the base is only ever a create-time seed, so the contribution's list is the intended
// value.
func deepMerge(dst, src map[string]any) {
	for k, sv := range src {
		if sm, ok := sv.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
			dst[k] = render.DeepCopyMap(sm)
			continue
		}
		dst[k] = sv
	}
}
