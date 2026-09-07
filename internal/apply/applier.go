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

// Package apply writes one contributor's contribution to a target, and withdraws it again.
//
// Two implementations, because the target's schema decides which is correct:
//
//   - ServerSideApply is preferred. The API server merges, detects conflicts, and releases a
//     manager's fields when it re-applies without them, so revert costs nothing.
//   - ClientSideApply exists for targets SSA cannot express: a list without listType=map is
//     atomic, owned wholesale by one manager, so two contributors cannot both append to it.
//     Ingress.spec.rules is such a list, which makes this the mode the primary use case needs.
package apply

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/render"
)

// Request is one contributor's apply against one target.
type Request struct {
	// Target identifies the object.
	Target render.Target

	// Contribution is the rendered claim.
	Contribution *render.Contribution

	// FieldManager is this contributor's manager name.
	FieldManager string

	// ConflictPolicy decides what to do when another manager owns a claimed field.
	ConflictPolicy patchv1alpha1.ConflictPolicy

	// MergeKeys give list granularity under ClientSideApply.
	MergeKeys []patchv1alpha1.MergeKey

	// PreviousPaths are the paths this contributor owned on its last apply. Paths present here
	// but absent from Contribution.Paths are withdrawn, which is what makes a shrinking
	// contribution actually shrink rather than leaving orphans behind.
	PreviousPaths []render.Path

	// PriorValues are the values the target held at PreviousPaths before this contributor first
	// touched them. Carried forward across applies; used by revert to restore rather than delete.
	PriorValues map[string]any

	// Base seeds a create when the target is absent. Nil for a patch-only contributor.
	Base map[string]any

	// AllowCreate permits creating the target. Derived from lifecycle.onMissing=Create.
	AllowCreate bool
}

// RevertRequest withdraws one contributor's fields.
type RevertRequest struct {
	Target render.Target

	// FieldManager is the manager whose fields are released.
	FieldManager string

	// OwnedPaths are the paths to withdraw under ClientSideApply. Ignored by the SSA applier,
	// which lets the API server work out what the manager owned.
	OwnedPaths []render.Path

	// PriorValues are restored where present, rather than the path being deleted. A contributor
	// that overwrote a pre-existing setting must put it back.
	PriorValues map[string]any
}

// Conflict describes an apply rejected because another manager owns a claimed field.
type Conflict struct {
	// Fields are the contested paths.
	Fields []string
	// Managers are the current owners.
	Managers []string
	// AllOurs reports whether every owner is another patch-operator manager, which is the only
	// case conflictPolicy: Priority may force.
	AllOurs bool
}

func (c *Conflict) Error() string {
	return fmt.Sprintf("field conflict on %v, owned by %v", c.Fields, c.Managers)
}

// Result reports what an apply did.
type Result struct {
	// Created is true when this apply brought the target into existence.
	Created bool

	// Changed is false when the target already matched, so the caller can skip a status write.
	Changed bool

	// Live is the target after the apply.
	Live *unstructured.Unstructured

	// OwnedPaths are the paths this contributor now owns, for the tracker to record.
	OwnedPaths []render.Path

	// PriorValues are the values that existed at those paths before this contributor first
	// touched them, merged with any already known. Only populated under ClientSideApply.
	PriorValues map[string]any

	// Conflict is set, with no error returned, when the policy is to report rather than force.
	Conflict *Conflict
}

// Applier writes and withdraws one contributor's contribution.
//
// Implementations must be safe to call repeatedly: the tracker reconciles, so every apply is
// potentially a re-apply of an unchanged contribution.
type Applier interface {
	// Apply writes the contribution. A returned Result with a non-nil Conflict and a nil error
	// means "reported, not applied" — the caller surfaces it as a condition.
	Apply(ctx context.Context, req Request) (*Result, error)

	// Revert withdraws the contributor's fields. It must succeed when the target is already gone.
	Revert(ctx context.Context, req RevertRequest) error

	// Mode reports which apply mode this implementation provides.
	Mode() patchv1alpha1.ApplyMode
}

// For returns the applier for a mode.
func For(mode patchv1alpha1.ApplyMode, c client.Client) (Applier, error) {
	switch mode {
	case patchv1alpha1.ApplyModeServerSideApply, "":
		return &ServerSideApplier{Client: c}, nil
	case patchv1alpha1.ApplyModeClientSideApply:
		return &ClientSideApplier{Client: c}, nil
	default:
		return nil, fmt.Errorf("unsupported apply mode %q", mode)
	}
}

// FieldManagerFor derives a contributor's field manager name.
//
// The prefix is what lets a conflict be recognised as internal — arbitrable by the priority the
// user declared — rather than foreign, which must never be forced.
func FieldManagerFor(kind, namespace, name string) string {
	base := patchv1alpha1.FieldManagerPrefix
	if namespace != "" {
		base += namespace + "/"
	}
	base += name
	return truncateFieldManager(base)
}

// maxFieldManagerLength is the API server's limit on a field manager name.
const maxFieldManagerLength = 128

// truncateFieldManager keeps a manager name inside the API server's limit while keeping it
// distinct, by replacing the overflowing tail with a hash of the whole name.
func truncateFieldManager(s string) string {
	if len(s) <= maxFieldManagerLength {
		return s
	}
	h, err := render.HashObject(s)
	if err != nil {
		// HashObject only fails on unmarshalable input, which a string is not.
		h = "sha256:unknown"
	}
	suffix := "-" + h[len("sha256:"):][:16]
	return s[:maxFieldManagerLength-len(suffix)] + suffix
}

// targetObject returns an empty unstructured object addressing target.
func targetObject(target render.Target) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(target.APIVersion)
	obj.SetKind(target.Kind)
	obj.SetName(target.Name)
	if target.Namespace != "" {
		obj.SetNamespace(target.Namespace)
	}
	return obj
}

// targetKey is the lookup key for a target.
func targetKey(target render.Target) client.ObjectKey {
	return client.ObjectKey{Namespace: target.Namespace, Name: target.Name}
}
