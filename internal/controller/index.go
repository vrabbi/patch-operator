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

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/scope"
)

// TargetKeyIndex is the field index that finds every contributor to a target.
//
// It spans both contributor kinds deliberately: after a promotion one tracker holds a mix of
// ResourcePatch and ClusterResourcePatch contributors, so a tracker has to be able to list both by
// the same key. Without the index this would be a cluster-wide scan on every reconcile.
const TargetKeyIndex = "spec.target.key"

// SetupIndexes registers the target-key index for both contributor kinds.
func SetupIndexes(ctx context.Context, mgr manager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		ctx, &patchv1alpha1.ResourcePatch{}, TargetKeyIndex,
		func(obj client.Object) []string {
			return targetKeysFor(obj.(*patchv1alpha1.ResourcePatch))
		},
	); err != nil {
		return fmt.Errorf("indexing ResourcePatch by target key: %w", err)
	}

	if err := mgr.GetFieldIndexer().IndexField(
		ctx, &patchv1alpha1.ClusterResourcePatch{}, TargetKeyIndex,
		func(obj client.Object) []string {
			return targetKeysFor(obj.(*patchv1alpha1.ClusterResourcePatch))
		},
	); err != nil {
		return fmt.Errorf("indexing ClusterResourcePatch by target key: %w", err)
	}

	return nil
}

// targetKeysFor returns the index values for a contributor.
//
// Single mode yields exactly one key, whether or not the object exists — a contributor waiting for
// its target still has to be found when that target appears.
//
// Selector mode yields nothing: the matched set is not knowable from the spec alone, so those
// contributors are resolved by listing at reconcile time instead. Indexing a stale resolution
// would be worse than not indexing, since the index would go quietly out of date as labels change.
func targetKeysFor(c patchv1alpha1.Contributor) []string {
	spec := c.GetPatchSpec()
	mode := spec.Target.Mode
	if mode == "" {
		mode = patchv1alpha1.TargetModeSingle
	}
	if mode != patchv1alpha1.TargetModeSingle || spec.Target.Name == "" {
		return nil
	}
	return []string{scope.TargetKeyFor(c).String()}
}
