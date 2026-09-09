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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/scope"
)

func raw(s string) *runtime.RawExtension { return &runtime.RawExtension{Raw: []byte(s)} }

func ptrTrue() *bool { b := true; return &b }

// cmPatch builds a ResourcePatch contributing one ConfigMap key in its own namespace.
func cmPatch(ns, name, targetName, key, value string, mutate func(*patchv1alpha1.ResourcePatch)) *patchv1alpha1.ResourcePatch {
	rp := &patchv1alpha1.ResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: patchv1alpha1.ResourcePatchSpec{
			Target: patchv1alpha1.TargetRef{
				Mode:       patchv1alpha1.TargetModeSingle,
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Name:       targetName,
			},
			Lifecycle: patchv1alpha1.LifecycleSpec{
				OnMissing:     patchv1alpha1.OnMissingWait,
				OnRelease:     patchv1alpha1.OnReleaseRevert,
				AdoptExisting: ptrTrue(),
				BaseReconcile: patchv1alpha1.BaseReconcileCreateOnly,
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
	if mutate != nil {
		mutate(rp)
	}
	return rp
}

// makeCreator turns a patch into the contributor that creates the target.
func makeCreator(targetName string) func(*patchv1alpha1.ResourcePatch) {
	return func(rp *patchv1alpha1.ResourcePatch) {
		rp.Spec.Lifecycle.OnMissing = patchv1alpha1.OnMissingCreate
		rp.Spec.Base = raw(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"` + targetName + `"}}`)
	}
}

func trackerFor(ns, targetName string) client.ObjectKey {
	key := scope.TargetKey{APIVersion: "v1", Kind: "ConfigMap", Namespace: ns, Name: targetName}
	return client.ObjectKey{Namespace: ns, Name: scope.TrackerName(key)}
}

// Two contributors in one namespace contribute disjoint keys; one creates the target. This is
// Appendix A.1 reduced to the namespaced pair.
func TestTwoContributorsShareATarget(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)
	dumpOnFailure(t, ctx, ns)

	creator := cmPatch(ns, "creator", "shared", "key-a", "value-a", makeCreator("shared"))
	patcher := cmPatch(ns, "patcher", "shared", "key-b", "value-b", nil)

	if err := k8sClient.Create(ctx, creator); err != nil {
		t.Fatalf("creating the creator: %v", err)
	}
	if err := k8sClient.Create(ctx, patcher); err != nil {
		t.Fatalf("creating the patcher: %v", err)
	}

	waitFor(t, "both contributions to land on the shared ConfigMap", func() bool {
		d := getConfigMap(t, ctx, ns, "shared")
		return d["key-a"] == "value-a" && d["key-b"] == "value-b"
	})

	// The tracker must record both, and know who created the object.
	waitFor(t, "the tracker to record both contributors and the creator", func() bool {
		sr := &patchv1alpha1.SharedResource{}
		if err := k8sClient.Get(ctx, trackerFor(ns, "shared"), sr); err != nil {
			return false
		}
		return len(sr.Status.Contributors) == 2 &&
			sr.Status.CreatedByOperator &&
			sr.Status.CreatorPatchRef != nil &&
			sr.Status.CreatorPatchRef.Name == "creator" &&
			sr.Status.ContributorCount == 2
	})

	// Both contributors must report Ready, so an XR gating on them proceeds.
	for _, name := range []string{"creator", "patcher"} {
		waitFor(t, name+" to report Ready", func() bool {
			rp := &patchv1alpha1.ResourcePatch{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, rp); err != nil {
				return false
			}
			c := patchv1alpha1.GetCondition(rp.Status.Conditions, patchv1alpha1.ConditionReady)
			return c != nil && c.Status == metav1.ConditionTrue
		})
	}
}

// The creating contributor is deleted first while a patch-only contributor remains. This is
// Appendix A.2, and it is where lead/follower designs break: the object must survive its creator.
func TestCreatorDeletedFirstLeavesTargetAndOtherContribution(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	creator := cmPatch(ns, "creator", "shared", "key-a", "value-a", makeCreator("shared"))
	patcher := cmPatch(ns, "patcher", "shared", "key-b", "value-b", nil)

	if err := k8sClient.Create(ctx, creator); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(ctx, patcher); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both contributions to land", func() bool {
		d := getConfigMap(t, ctx, ns, "shared")
		return d["key-a"] == "value-a" && d["key-b"] == "value-b"
	})

	// Delete the creator.
	if err := k8sClient.Delete(ctx, creator); err != nil {
		t.Fatal(err)
	}

	// Its finalizer must be released only after its field is withdrawn, so the object never
	// outlives the contributor with the field still on it.
	waitFor(t, "the creator's ResourcePatch to be fully gone", func() bool {
		rp := &patchv1alpha1.ResourcePatch{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "creator"}, rp)
		return apierrors.IsNotFound(err)
	})

	waitFor(t, "the creator's key to be withdrawn", func() bool {
		_, present := getConfigMap(t, ctx, ns, "shared")["key-a"]
		return !present
	})

	// The object survives, and the remaining contributor is untouched. Nothing deleted it: the
	// only contributor ever authorized to do so is gone, and its departure did not request it.
	if !configMapExists(ctx, ns, "shared") {
		t.Fatal("the target was deleted when its creator left, while another contributor remained")
	}
	if d := getConfigMap(t, ctx, ns, "shared"); d["key-b"] != "value-b" {
		t.Errorf("the remaining contributor's field was disturbed: %#v", d)
	}

	// And it keeps surviving: no delayed deletion.
	consistently(t, 2*time.Second, "the target to keep existing after its creator left", func() bool {
		return configMapExists(ctx, ns, "shared")
	})
}

// onRelease: Delete deletes the target when the creating contributor is the last one out.
func TestDeleteOnLastReleaseByCreator(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)
	dumpOnFailure(t, ctx, ns)

	creator := cmPatch(ns, "creator", "owned", "key-a", "value-a", func(rp *patchv1alpha1.ResourcePatch) {
		makeCreator("owned")(rp)
		rp.Spec.Lifecycle.OnRelease = patchv1alpha1.OnReleaseDelete
	})
	if err := k8sClient.Create(ctx, creator); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the target to be created", func() bool {
		return configMapExists(ctx, ns, "owned")
	})

	if err := k8sClient.Delete(ctx, creator); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the target to be deleted with its creator", func() bool {
		return !configMapExists(ctx, ns, "owned")
	})
}

// The delete-safety rule, exercised at runtime: a contributor may only delete a target it
// actually created.
//
// The interesting case is a contributor configured as a creator (Create + Delete, which is the
// only combination validation accepts) that nevertheless did NOT create the object, because the
// object already existed and was adopted. createdByOperator is then false, so the object must
// survive the contributor's departure however the contributor is configured.
//
// The blunter attack -- onRelease: Delete on a patch-only contributor -- is rejected outright by
// validation, which TestInvalidDeleteWithoutCreateIsRejected covers.
func TestAdoptedTargetIsNotDeletedOnRelease(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	// Something else owns this object; the operator did not create it.
	preexisting := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "not-ours", Namespace: ns},
		Data:       map[string]string{"owner": "someone-else"},
	}
	if err := k8sClient.Create(ctx, preexisting); err != nil {
		t.Fatal(err)
	}

	adopter := cmPatch(ns, "adopter", "not-ours", "injected", "v", func(rp *patchv1alpha1.ResourcePatch) {
		makeCreator("not-ours")(rp)
		rp.Spec.Lifecycle.OnRelease = patchv1alpha1.OnReleaseDelete
	})
	if err := k8sClient.Create(ctx, adopter); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the contribution to land on the pre-existing object", func() bool {
		return getConfigMap(t, ctx, ns, "not-ours")["injected"] == "v"
	})

	// The tracker must not claim to have created an object that already existed.
	sr := &patchv1alpha1.SharedResource{}
	if err := k8sClient.Get(ctx, trackerFor(ns, "not-ours"), sr); err != nil {
		t.Fatal(err)
	}
	if sr.Status.CreatedByOperator {
		t.Error("the tracker claims to have created an object that already existed")
	}

	if err := k8sClient.Delete(ctx, adopter); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the contributor to be released", func() bool {
		rp := &patchv1alpha1.ResourcePatch{}
		return apierrors.IsNotFound(
			k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "adopter"}, rp))
	})

	if !configMapExists(ctx, ns, "not-ours") {
		t.Fatal("a contributor deleted an object it adopted rather than created")
	}
	consistently(t, 2*time.Second, "the adopted object to survive", func() bool {
		return configMapExists(ctx, ns, "not-ours")
	})

	// Its own data must be intact, and the contribution withdrawn.
	d := getConfigMap(t, ctx, ns, "not-ours")
	if d["owner"] != "someone-else" {
		t.Errorf("the object's own data was disturbed: %#v", d)
	}
	if _, present := d["injected"]; present {
		t.Errorf("the contribution was not withdrawn: %#v", d)
	}
}

// onRelease: Delete without onMissing: Create is the configuration that would let a patch-only
// contributor attach itself to a production object and delete it on the way out. It is rejected,
// and the contributor never applies.
func TestInvalidDeleteWithoutCreateIsRejected(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	preexisting := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: ns},
		Data:       map[string]string{"owner": "someone-else"},
	}
	if err := k8sClient.Create(ctx, preexisting); err != nil {
		t.Fatal(err)
	}

	bad := cmPatch(ns, "attacker", "target", "injected", "v", func(rp *patchv1alpha1.ResourcePatch) {
		rp.Spec.Lifecycle.OnRelease = patchv1alpha1.OnReleaseDelete
	})
	if err := k8sClient.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the invalid lifecycle to be reported", func() bool {
		rp := &patchv1alpha1.ResourcePatch{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "attacker"}, rp); err != nil {
			return false
		}
		c := patchv1alpha1.GetCondition(rp.Status.Conditions, patchv1alpha1.ConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse &&
			c.Reason == patchv1alpha1.ReasonInvalidSpec
	})

	// And it never applied anything.
	if d := getConfigMap(t, ctx, ns, "target"); d["injected"] != "" {
		t.Errorf("a rejected contributor still applied: %#v", d)
	}
}

// onRelease: Orphan leaves the contributed field in place.
func TestOrphanLeavesFieldsBehind(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	c := cmPatch(ns, "orphaner", "shared", "left-behind", "v", func(rp *patchv1alpha1.ResourcePatch) {
		makeCreator("shared")(rp)
		rp.Spec.Lifecycle.OnRelease = patchv1alpha1.OnReleaseOrphan
	})
	if err := k8sClient.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the contribution to land", func() bool {
		return getConfigMap(t, ctx, ns, "shared")["left-behind"] == "v"
	})

	if err := k8sClient.Delete(ctx, c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the contributor to be released", func() bool {
		rp := &patchv1alpha1.ResourcePatch{}
		return apierrors.IsNotFound(
			k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "orphaner"}, rp))
	})

	if d := getConfigMap(t, ctx, ns, "shared"); d["left-behind"] != "v" {
		t.Errorf("Orphan withdrew the field anyway: %#v", d)
	}
}

// A patch-only contributor whose target does not exist must park, not error, and must not create
// the object as a side effect.
func TestWaitForMissingTarget(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	waiter := cmPatch(ns, "waiter", "not-yet", "k", "v", nil)
	if err := k8sClient.Create(ctx, waiter); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the contributor to report it is waiting", func() bool {
		rp := &patchv1alpha1.ResourcePatch{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "waiter"}, rp); err != nil {
			return false
		}
		c := patchv1alpha1.GetCondition(rp.Status.Conditions, patchv1alpha1.ConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse
	})

	if configMapExists(ctx, ns, "not-yet") {
		t.Fatal("a patch-only contributor created its target")
	}

	// Now the object appears; the waiting contributor must converge without being touched.
	created := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "not-yet", Namespace: ns}}
	if err := k8sClient.Create(ctx, created); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the waiting contribution to land once the target appears", func() bool {
		return getConfigMap(t, ctx, ns, "not-yet")["k"] == "v"
	})
}

// The tracker must be deleted once nothing references its target, or trackers accumulate forever.
func TestTrackerIsCleanedUpWhenEmpty(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	c := cmPatch(ns, "only", "shared", "k", "v", makeCreator("shared"))
	if err := k8sClient.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the tracker to exist", func() bool {
		sr := &patchv1alpha1.SharedResource{}
		return k8sClient.Get(ctx, trackerFor(ns, "shared"), sr) == nil
	})

	if err := k8sClient.Delete(ctx, c); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the tracker to be deleted once it has no contributors", func() bool {
		sr := &patchv1alpha1.SharedResource{}
		return apierrors.IsNotFound(k8sClient.Get(ctx, trackerFor(ns, "shared"), sr))
	})
}

// Two contributors racing to create the same target must both succeed. Tracker names are
// deterministic, so the loser's AlreadyExists is a normal outcome rather than an error.
func TestConcurrentCreatorsRace(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	a := cmPatch(ns, "racer-a", "raced", "key-a", "a", makeCreator("raced"))
	b := cmPatch(ns, "racer-b", "raced", "key-b", "b", makeCreator("raced"))

	if err := k8sClient.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := k8sClient.Create(ctx, b); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "both racing contributions to land", func() bool {
		d := getConfigMap(t, ctx, ns, "raced")
		return d["key-a"] == "a" && d["key-b"] == "b"
	})

	// Exactly one tracker, and exactly one recorded creator.
	list := &patchv1alpha1.SharedResourceList{}
	if err := k8sClient.List(ctx, list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly one tracker for one target, got %d", len(list.Items))
	}
	if list.Items[0].Status.CreatorPatchRef == nil {
		t.Error("no creator recorded after the race")
	}
}

// A contributor whose spec is invalid must be reported, not silently ignored, and must not write.
func TestInvalidSpecIsReported(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)

	// Cross-namespace on a ResourcePatch: rejected by the reconciler even though the webhook is
	// not running here, because containment must not depend on admission being reachable.
	bad := cmPatch(ns, "cross-ns", "elsewhere", "k", "v", func(rp *patchv1alpha1.ResourcePatch) {
		rp.Spec.Target.Namespace = "some-other-namespace"
	})
	if err := k8sClient.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the invalid spec to be reported", func() bool {
		rp := &patchv1alpha1.ResourcePatch{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "cross-ns"}, rp); err != nil {
			return false
		}
		c := patchv1alpha1.GetCondition(rp.Status.Conditions, patchv1alpha1.ConditionReady)
		return c != nil && c.Status == metav1.ConditionFalse &&
			c.Reason == patchv1alpha1.ReasonInvalidSpec
	})

	// And nothing was written anywhere.
	if configMapExists(ctx, "some-other-namespace", "elsewhere") {
		t.Fatal("a cross-namespace ResourcePatch reached another namespace")
	}
}
