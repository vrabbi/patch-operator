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

package integration

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/scope"
)

// clusterCMPatch builds a ClusterResourcePatch contributing one ConfigMap key in a named namespace.
func clusterCMPatch(name, targetNS, targetName, key, value string) *patchv1alpha1.ClusterResourcePatch {
	return &patchv1alpha1.ClusterResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: patchv1alpha1.ResourcePatchSpec{
			Target: patchv1alpha1.TargetRef{
				Mode:       patchv1alpha1.TargetModeSingle,
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       targetName,
				Namespace:  targetNS,
			},
			Lifecycle: patchv1alpha1.LifecycleSpec{
				OnMissing: patchv1alpha1.OnMissingWait,
				OnRelease: patchv1alpha1.OnReleaseRevert,
			},
			Apply: patchv1alpha1.ApplySpec{
				Mode:           patchv1alpha1.ApplyModeServerSideApply,
				ConflictPolicy: patchv1alpha1.ConflictPolicyFail,
			},
			Priority: 100,
			Patch: patchv1alpha1.PatchSpec{
				Type:  patchv1alpha1.PatchTypeStrategicMerge,
				Value: raw(`{"data":{"` + key + `":"` + value + `"}}`),
			},
		},
	}
}

func clusterTrackerName(ns, targetName string) string {
	return scope.TrackerName(scope.TargetKey{
		APIVersion: "v1", Kind: "ConfigMap", Namespace: ns, Name: targetName,
	})
}

// A namespaced target gains a cluster-scoped contributor and must be promoted. This is Appendix
// A.4, and it is the case the scope split introduces.
//
// The properties that matter are what does NOT happen: the target is never written during the
// migration, and the state that makes revert and delete-safety work survives the copy.
func TestPromotionMigratesTrackerWithoutTouchingTarget(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)
	dumpOnFailure(t, ctx, ns)

	// Two namespaced contributors, one of them the creator.
	creator := cmPatch(ns, "creator", "shared", "key-a", "value-a", makeCreator("shared"))
	patcher := cmPatch(ns, "patcher", "shared", "key-b", "value-b", nil)
	if err := k8sClient.Create(ctx, creator); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(ctx, patcher); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "both namespaced contributions to land", func() bool {
		d := getConfigMap(t, ctx, ns, "shared")
		return d["key-a"] == "value-a" && d["key-b"] == "value-b"
	})

	// Capture the pre-promotion state so it can be compared afterwards.
	before := &patchv1alpha1.SharedResource{}
	waitFor(t, "the namespaced tracker to record both contributors and the creator", func() bool {
		if err := k8sClient.Get(ctx, trackerFor(ns, "shared"), before); err != nil {
			return false
		}
		return len(before.Status.Contributors) == 2 && before.Status.CreatorPatchRef != nil
	})
	creatorRefBefore := *before.Status.CreatorPatchRef
	createdByOperatorBefore := before.Status.CreatedByOperator

	cmBefore := getConfigMap(t, ctx, ns, "shared")

	// The platform team now contributes from outside the namespace.
	cluster := clusterCMPatch("platform-contribution", ns, "shared", "key-c", "value-c")
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}

	// A ClusterSharedResource must take over.
	csr := &patchv1alpha1.ClusterSharedResource{}
	waitFor(t, "a ClusterSharedResource to adopt the target", func() bool {
		err := k8sClient.Get(ctx, client.ObjectKey{Name: clusterTrackerName(ns, "shared")}, csr)
		return err == nil && len(csr.Status.Contributors) >= 2
	})

	// The namespaced tracker must be retired, not left as a second writer.
	waitFor(t, "the namespaced tracker to be retired", func() bool {
		sr := &patchv1alpha1.SharedResource{}
		err := k8sClient.Get(ctx, trackerFor(ns, "shared"), sr)
		return apierrors.IsNotFound(err)
	})

	// Exactly one tracker now owns this target.
	srList := &patchv1alpha1.SharedResourceList{}
	if err := k8sClient.List(ctx, srList, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(srList.Items) != 0 {
		t.Errorf("a namespaced tracker survived the promotion: %d remain", len(srList.Items))
	}

	// The state that makes revert and delete-safety work must have survived the copy. Losing
	// creatorPatchRef would silently retire the "only the creator may delete" guarantee at exactly
	// the moment the contributor set became cross-namespace.
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: clusterTrackerName(ns, "shared")}, csr); err != nil {
		t.Fatal(err)
	}
	if csr.Status.CreatorPatchRef == nil {
		t.Fatal("creatorPatchRef was lost in the promotion; delete-safety would no longer hold")
	}
	if !patchv1alpha1.SameContributor(*csr.Status.CreatorPatchRef, creatorRefBefore) {
		t.Errorf("creatorPatchRef changed across the promotion: %+v -> %+v",
			creatorRefBefore, *csr.Status.CreatorPatchRef)
	}
	if csr.Status.CreatedByOperator != createdByOperatorBefore {
		t.Errorf("createdByOperator changed across the promotion: %v -> %v",
			createdByOperatorBefore, csr.Status.CreatedByOperator)
	}
	// The successor must not itself be fenced: promotion is one-way.
	if csr.Status.PromotedTo != nil {
		t.Error("the successor is fenced; a ClusterSharedResource is never demoted")
	}

	// The existing contributions must be untouched by the migration itself.
	if d := getConfigMap(t, ctx, ns, "shared"); d["key-a"] != cmBefore["key-a"] || d["key-b"] != cmBefore["key-b"] {
		t.Errorf("the migration disturbed existing contributions: %#v -> %#v", cmBefore, d)
	}

	// And the new cluster-scoped contribution lands.
	waitFor(t, "the cluster-scoped contribution to land", func() bool {
		return getConfigMap(t, ctx, ns, "shared")["key-c"] == "value-c"
	})

	// The namespaced contributors must have re-registered with the successor, not been orphaned.
	for _, name := range []string{"creator", "patcher"} {
		waitFor(t, name+" to re-register with the ClusterSharedResource", func() bool {
			rp := &patchv1alpha1.ResourcePatch{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, rp); err != nil {
				return false
			}
			for _, ref := range rp.Status.SharedResourceRefs {
				if ref.Kind == patchv1alpha1.KindClusterSharedResource {
					return true
				}
			}
			return false
		})
	}
}

// Promotion is one-way. When the cluster-scoped contributor leaves, the tracker stays
// cluster-scoped: demoting would be a second migration with the same hazards for no benefit.
func TestPromotionIsNotReversed(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	namespaced := cmPatch(ns, "namespaced", "shared", "key-a", "value-a", makeCreator("shared"))
	if err := k8sClient.Create(ctx, namespaced); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the namespaced contribution to land", func() bool {
		return getConfigMap(t, ctx, ns, "shared")["key-a"] == "value-a"
	})

	cluster := clusterCMPatch("cluster-contribution", ns, "shared", "key-c", "value-c")
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "promotion to complete", func() bool {
		csr := &patchv1alpha1.ClusterSharedResource{}
		return k8sClient.Get(ctx, client.ObjectKey{Name: clusterTrackerName(ns, "shared")}, csr) == nil
	})

	// The cluster contributor departs.
	if err := k8sClient.Delete(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the cluster-scoped contribution to be withdrawn", func() bool {
		_, present := getConfigMap(t, ctx, ns, "shared")["key-c"]
		return !present
	})

	// The tracker stays cluster-scoped, and the namespaced contributor carries on against it.
	consistently(t, 3*time.Second, "the tracker to stay cluster-scoped", func() bool {
		csr := &patchv1alpha1.ClusterSharedResource{}
		return k8sClient.Get(ctx, client.ObjectKey{Name: clusterTrackerName(ns, "shared")}, csr) == nil
	})
	if d := getConfigMap(t, ctx, ns, "shared"); d["key-a"] != "value-a" {
		t.Errorf("the namespaced contribution was lost: %#v", d)
	}
}

// A cluster-scoped target is tracked by a ClusterSharedResource from the start, with no promotion
// involved.
func TestClusterScopedTargetUsesClusterTracker(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()

	target := "patch-operator-it-shared-role"
	c := &patchv1alpha1.ClusterResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-role-contribution"},
		Spec: patchv1alpha1.ResourcePatchSpec{
			Target: patchv1alpha1.TargetRef{
				Mode:       patchv1alpha1.TargetModeSingle,
				APIVersion: "rbac.authorization.k8s.io/v1",
				Kind:       "ClusterRole",
				Name:       target,
			},
			Lifecycle: patchv1alpha1.LifecycleSpec{
				OnMissing: patchv1alpha1.OnMissingCreate,
				OnRelease: patchv1alpha1.OnReleaseDelete,
			},
			Apply: patchv1alpha1.ApplySpec{Mode: patchv1alpha1.ApplyModeServerSideApply},
			Base: raw(`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole",` +
				`"metadata":{"name":"` + target + `"},"rules":[]}`),
			Patch: patchv1alpha1.PatchSpec{
				Value: raw(`{"metadata":{"labels":{"contributed":"yes"}}}`),
			},
		},
	}
	if err := k8sClient.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), c)
	})

	key := scope.TargetKey{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: target}
	waitFor(t, "a ClusterSharedResource to track the cluster-scoped target", func() bool {
		csr := &patchv1alpha1.ClusterSharedResource{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: scope.TrackerName(key)}, csr); err != nil {
			return false
		}
		// The tracker's targetRef must carry no namespace for a cluster-scoped target.
		return csr.Spec.TargetRef.Namespace == "" && len(csr.Status.Contributors) == 1
	})

	// No namespaced tracker anywhere for a cluster-scoped target.
	srList := &patchv1alpha1.SharedResourceList{}
	if err := k8sClient.List(ctx, srList); err != nil {
		t.Fatal(err)
	}
	for _, sr := range srList.Items {
		if sr.Spec.TargetRef.Kind == "ClusterRole" {
			t.Errorf("a namespaced tracker was created for a cluster-scoped target: %s/%s",
				sr.Namespace, sr.Name)
		}
	}
}
