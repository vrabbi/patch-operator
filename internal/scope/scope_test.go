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

package scope

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

func raw(s string) *runtime.RawExtension { return &runtime.RawExtension{Raw: []byte(s)} }

// validSpec is a spec that passes validation, so each test can perturb exactly one thing.
func validSpec() patchv1alpha1.ResourcePatchSpec {
	return patchv1alpha1.ResourcePatchSpec{
		Target: patchv1alpha1.TargetRef{
			Mode:       patchv1alpha1.TargetModeSingle,
			APIVersion: "networking.k8s.io/v1",
			Kind:       "Ingress",
			Name:       "shared-ingress",
		},
		Lifecycle: patchv1alpha1.LifecycleSpec{
			OnMissing: patchv1alpha1.OnMissingWait,
			OnRelease: patchv1alpha1.OnReleaseRevert,
		},
		Patch: patchv1alpha1.PatchSpec{
			Type:  patchv1alpha1.PatchTypeStrategicMerge,
			Value: raw(`{"spec":{"ingressClassName":"nginx"}}`),
		},
	}
}

func rp(ns, name string, mutate func(*patchv1alpha1.ResourcePatchSpec)) *patchv1alpha1.ResourcePatch {
	spec := validSpec()
	if mutate != nil {
		mutate(&spec)
	}
	return &patchv1alpha1.ResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
}

func crp(name string, mutate func(*patchv1alpha1.ResourcePatchSpec)) *patchv1alpha1.ClusterResourcePatch {
	spec := validSpec()
	spec.Target.Namespace = "platform"
	if mutate != nil {
		mutate(&spec)
	}
	return &patchv1alpha1.ClusterResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
}

// namespacedKinds answers the RESTMapper question for the kinds these tests use.
func namespacedKinds(apiVersion, kind string) (bool, error) {
	switch kind {
	case "Ingress", "ConfigMap", "Secret", "Deployment":
		return true, nil
	case "ClusterRole", "StorageClass", "Namespace":
		return false, nil
	}
	return true, nil
}

// errorFields renders a validation result for assertions, so a test can assert on the explanation
// a user would actually see rather than on an error count.
func errorFields(errs field.ErrorList) string {
	if err := errs.ToAggregate(); err != nil {
		return err.Error()
	}
	return ""
}

func TestValidateAcceptsGoodSpecs(t *testing.T) {
	if errs := Validate(rp("team-a", "checkout", nil), namespacedKinds); len(errs) > 0 {
		t.Errorf("a valid ResourcePatch was rejected: %v", errorFields(errs))
	}
	if errs := Validate(crp("platform-rule", nil), namespacedKinds); len(errs) > 0 {
		t.Errorf("a valid ClusterResourcePatch was rejected: %v", errorFields(errs))
	}
}

// The containment property, asserted directly: a ResourcePatch naming another namespace must be
// rejected. This is the check that makes the namespaced kind safe to grant freely.
func TestValidateResourcePatchCannotTargetAnotherNamespace(t *testing.T) {
	c := rp("team-a", "checkout", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.Namespace = "platform"
	})

	errs := Validate(c, namespacedKinds)
	if len(errs) == 0 {
		t.Fatal("a cross-namespace ResourcePatch was accepted; containment is broken")
	}
	if !strings.Contains(errorFields(errs), "own namespace") {
		t.Errorf("the error should explain the containment rule: %v", errorFields(errs))
	}
}

func TestValidateResourcePatchMayRestateItsOwnNamespace(t *testing.T) {
	c := rp("team-a", "checkout", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.Namespace = "team-a"
	})
	if errs := Validate(c, namespacedKinds); len(errs) > 0 {
		t.Errorf("restating the own namespace explicitly should be valid: %v", errorFields(errs))
	}
}

func TestValidateResourcePatchCannotTargetClusterScopedKind(t *testing.T) {
	c := rp("team-a", "rule", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.APIVersion = "rbac.authorization.k8s.io/v1"
		s.Target.Kind = "ClusterRole"
		s.Target.Name = "shared"
	})
	errs := Validate(c, namespacedKinds)
	if len(errs) == 0 {
		t.Fatal("a ResourcePatch targeting a cluster-scoped kind was accepted")
	}
	if !strings.Contains(errorFields(errs), "namespaced kinds") {
		t.Errorf("unexpected error: %v", errorFields(errs))
	}
}

func TestValidateResourcePatchForbidsNamespaceSelector(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.Mode = patchv1alpha1.TargetModeSelector
		s.Target.Name = ""
		s.Target.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
		s.Target.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}}
	})
	if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "namespaceSelector") {
		t.Errorf("namespaceSelector should be forbidden on a ResourcePatch: %v", errorFields(errs))
	}
}

// Containment extends to the identity the write runs as, or a namespaced contributor could borrow
// a privileged ServiceAccount from elsewhere.
func TestValidateResourcePatchForbidsForeignServiceAccountNamespace(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.ServiceAccountRef = &patchv1alpha1.ServiceAccountRef{Name: "privileged", Namespace: "kube-system"}
	})
	if errs := Validate(c, namespacedKinds); len(errs) == 0 {
		t.Fatal("a ResourcePatch borrowing a ServiceAccount from another namespace was accepted")
	}

	own := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.ServiceAccountRef = &patchv1alpha1.ServiceAccountRef{Name: "patcher", Namespace: "team-a"}
	})
	if errs := Validate(own, namespacedKinds); len(errs) > 0 {
		t.Errorf("a same-namespace ServiceAccount should be valid: %v", errorFields(errs))
	}
}

// The cluster kind may do all the things the namespaced kind may not. That asymmetry is the point.
func TestValidateClusterResourcePatchMayCrossNamespaces(t *testing.T) {
	c := crp("platform-rule", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.Namespace = "platform"
		s.ServiceAccountRef = &patchv1alpha1.ServiceAccountRef{Name: "patcher", Namespace: "team-a"}
	})
	if errs := Validate(c, namespacedKinds); len(errs) > 0 {
		t.Errorf("a cross-namespace ClusterResourcePatch was rejected: %v", errorFields(errs))
	}
}

func TestValidateClusterResourcePatchRequiresNamespaceForNamespacedKind(t *testing.T) {
	c := crp("x", func(s *patchv1alpha1.ResourcePatchSpec) { s.Target.Namespace = "" })
	if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "namespace is required") {
		t.Errorf("expected a required-namespace error: %v", errorFields(errs))
	}
}

func TestValidateClusterResourcePatchForbidsNamespaceForClusterScopedKind(t *testing.T) {
	c := crp("x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.APIVersion = "rbac.authorization.k8s.io/v1"
		s.Target.Kind = "ClusterRole"
		s.Target.Namespace = "platform"
	})
	if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "forbidden") {
		t.Errorf("expected a forbidden-namespace error: %v", errorFields(errs))
	}
}

func TestValidateClusterResourcePatchMayTargetClusterScopedKind(t *testing.T) {
	c := crp("x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.APIVersion = "rbac.authorization.k8s.io/v1"
		s.Target.Kind = "ClusterRole"
		s.Target.Namespace = ""
	})
	if errs := Validate(c, namespacedKinds); len(errs) > 0 {
		t.Errorf("a cluster-scoped target should be valid on the cluster kind: %v", errorFields(errs))
	}
}

func TestValidateSelectorModeIsPatchOnly(t *testing.T) {
	selector := func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.Mode = patchv1alpha1.TargetModeSelector
		s.Target.Name = ""
		s.Target.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
	}

	t.Run("create forbidden", func(t *testing.T) {
		c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
			selector(s)
			s.Lifecycle.OnMissing = patchv1alpha1.OnMissingCreate
			s.Base = raw(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress"}`)
		})
		if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "onMissing: Create is invalid in Selector mode") {
			t.Errorf("got %v", errorFields(errs))
		}
	})

	t.Run("delete forbidden", func(t *testing.T) {
		c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
			selector(s)
			s.Lifecycle.OnRelease = patchv1alpha1.OnReleaseDelete
		})
		if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "Selector mode") {
			t.Errorf("got %v", errorFields(errs))
		}
	})

	t.Run("name forbidden", func(t *testing.T) {
		c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
			selector(s)
			s.Target.Name = "explicit"
		})
		if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "name is forbidden") {
			t.Errorf("got %v", errorFields(errs))
		}
	})

	t.Run("needs a selector", func(t *testing.T) {
		c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
			s.Target.Mode = patchv1alpha1.TargetModeSelector
			s.Target.Name = ""
		})
		if errs := Validate(c, namespacedKinds); len(errs) == 0 {
			t.Error("Selector mode with no selector should be rejected")
		}
	})
}

func TestValidateSingleModeRequiresName(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) { s.Target.Name = "" })
	if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "name is required") {
		t.Errorf("got %v", errorFields(errs))
	}
}

func TestValidateSingleModeForbidsSelector(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
	})
	if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "selector") {
		t.Errorf("got %v", errorFields(errs))
	}
}

// onRelease: Delete without onMissing: Create is the configuration that would let a patch-only
// contributor attach itself to a production object and delete it on the way out.
func TestValidateDeleteRequiresCreate(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Lifecycle.OnRelease = patchv1alpha1.OnReleaseDelete
		s.Lifecycle.OnMissing = patchv1alpha1.OnMissingWait
	})
	errs := Validate(c, namespacedKinds)
	if !strings.Contains(errorFields(errs), "requires onMissing: Create") {
		t.Fatalf("a patch-only contributor was allowed to declare Delete: %v", errorFields(errs))
	}
}

func TestValidateCreateRequiresBase(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Lifecycle.OnMissing = patchv1alpha1.OnMissingCreate
		s.Base = nil
	})
	if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "spec.base") {
		t.Errorf("got %v", errorFields(errs))
	}
}

func TestValidateCreateWithBaseAndDeleteIsValid(t *testing.T) {
	c := rp("team-a", "creator", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Lifecycle.OnMissing = patchv1alpha1.OnMissingCreate
		s.Lifecycle.OnRelease = patchv1alpha1.OnReleaseDelete
		s.Base = raw(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress"}`)
	})
	if errs := Validate(c, namespacedKinds); len(errs) > 0 {
		t.Errorf("a creating contributor with Delete should be valid: %v", errorFields(errs))
	}
}

// Enforce is documented as unimplemented in v1alpha1, so accepting it would silently do nothing.
func TestValidateRejectsBaseReconcileEnforce(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Lifecycle.BaseReconcile = patchv1alpha1.BaseReconcileEnforce
	})
	if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "not implemented") {
		t.Errorf("got %v", errorFields(errs))
	}
}

func TestValidatePatchTypes(t *testing.T) {
	t.Run("merge needs value", func(t *testing.T) {
		c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) { s.Patch.Value = nil })
		if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "value is required") {
			t.Errorf("got %v", errorFields(errs))
		}
	})
	t.Run("merge rejects ops", func(t *testing.T) {
		c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) { s.Patch.Ops = raw(`[]`) })
		if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "ops") {
			t.Errorf("got %v", errorFields(errs))
		}
	})
	t.Run("json6902 needs ops", func(t *testing.T) {
		c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
			s.Patch.Type = patchv1alpha1.PatchTypeJSON6902
			s.Patch.Value = nil
		})
		if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "ops is required") {
			t.Errorf("got %v", errorFields(errs))
		}
	})
	t.Run("json6902 rejects value", func(t *testing.T) {
		c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
			s.Patch.Type = patchv1alpha1.PatchTypeJSON6902
			s.Patch.Ops = raw(`[{"op":"add","path":"/spec/x","value":1}]`)
		})
		if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "value is not used") {
			t.Errorf("got %v", errorFields(errs))
		}
	})
	t.Run("unknown type", func(t *testing.T) {
		c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
			s.Patch.Type = patchv1alpha1.PatchType("Nope")
		})
		if errs := Validate(c, namespacedKinds); len(errs) == 0 {
			t.Error("an unknown patch type should be rejected")
		}
	})
}

// mergeKeys under SSA do nothing, so accepting them silently would imply they take effect.
func TestValidateMergeKeysRejectedUnderSSA(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Apply.Mode = patchv1alpha1.ApplyModeServerSideApply
		s.Patch.MergeKeys = []patchv1alpha1.MergeKey{{Path: "spec.rules", Key: "host"}}
	})
	if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "ClientSideApply") {
		t.Errorf("got %v", errorFields(errs))
	}

	ok := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Apply.Mode = patchv1alpha1.ApplyModeClientSideApply
		s.Patch.MergeKeys = []patchv1alpha1.MergeKey{{Path: "spec.rules", Key: "host"}}
	})
	if errs := Validate(ok, namespacedKinds); len(errs) > 0 {
		t.Errorf("mergeKeys should be valid under CSA: %v", errorFields(errs))
	}
}

func TestValidateMergeKeyFieldsRequired(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Apply.Mode = patchv1alpha1.ApplyModeClientSideApply
		s.Patch.MergeKeys = []patchv1alpha1.MergeKey{{}}
	})
	if errs := Validate(c, namespacedKinds); len(errs) < 2 {
		t.Errorf("both path and key should be required: %v", errorFields(errs))
	}
}

// A contributor targeting the operator's own kinds would reconcile itself.
func TestValidateRejectsOwnKinds(t *testing.T) {
	for _, kind := range []string{
		patchv1alpha1.KindResourcePatch,
		patchv1alpha1.KindClusterResourcePatch,
		patchv1alpha1.KindSharedResource,
		patchv1alpha1.KindClusterSharedResource,
	} {
		t.Run(kind, func(t *testing.T) {
			c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
				s.Target.APIVersion = "terasky.com/v1alpha1"
				s.Target.Kind = kind
				s.Target.Name = "other"
			})
			if errs := Validate(c, namespacedKinds); !strings.Contains(errorFields(errs), "own kinds") {
				t.Errorf("got %v", errorFields(errs))
			}
		})
	}

	// A same-named kind in another group is somebody else's API and must be allowed.
	other := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.APIVersion = "example.com/v1"
		s.Target.Kind = patchv1alpha1.KindResourcePatch
		s.Target.Name = "other"
	})
	if errs := Validate(other, namespacedKinds); len(errs) > 0 {
		t.Errorf("a same-named kind in another group should be allowed: %v", errorFields(errs))
	}
}

func TestValidateWithoutRESTMapper(t *testing.T) {
	// Admission may have no RESTMapper. The namespace-containment checks must still run; only the
	// kind-scope check is skipped.
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) { s.Target.Namespace = "platform" })
	if errs := Validate(c, nil); len(errs) == 0 {
		t.Error("containment must be enforced even without a RESTMapper")
	}
}

func TestValidateRequiresTargetIdentity(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) {
		s.Target.APIVersion = ""
		s.Target.Kind = ""
	})
	if errs := Validate(c, namespacedKinds); len(errs) < 2 {
		t.Errorf("both apiVersion and kind should be required: %v", errorFields(errs))
	}
}

// ResolveTargetNamespace is the runtime half of containment: even if a spec somehow carried
// another namespace, a ResourcePatch resolves in its own.
func TestResolveTargetNamespaceForcesOwnNamespace(t *testing.T) {
	c := rp("team-a", "x", func(s *patchv1alpha1.ResourcePatchSpec) { s.Target.Namespace = "platform" })
	if got := ResolveTargetNamespace(c); got != "team-a" {
		t.Errorf("got %q; a ResourcePatch must resolve in its own namespace regardless of spec", got)
	}

	cluster := crp("x", func(s *patchv1alpha1.ResourcePatchSpec) { s.Target.Namespace = "platform" })
	if got := ResolveTargetNamespace(cluster); got != "platform" {
		t.Errorf("got %q; a ClusterResourcePatch uses the namespace it names", got)
	}
}

func TestTargetKeyFor(t *testing.T) {
	c := rp("team-a", "x", nil)
	key := TargetKeyFor(c)
	want := TargetKey{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Namespace: "team-a", Name: "shared-ingress"}
	if key != want {
		t.Errorf("key = %+v, want %+v", key, want)
	}
	if key.IsClusterScopedTarget() {
		t.Error("a namespaced key should not report a cluster-scoped target")
	}
}

func TestTargetKeyString(t *testing.T) {
	namespaced := TargetKey{APIVersion: "v1", Kind: "ConfigMap", Namespace: "team-a", Name: "shared"}
	if got := namespaced.String(); got != "v1/ConfigMap/team-a/shared" {
		t.Errorf("got %q", got)
	}
	clusterScoped := TargetKey{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "shared"}
	if got := clusterScoped.String(); got != "rbac.authorization.k8s.io/v1/ClusterRole/shared" {
		t.Errorf("got %q", got)
	}
	if !clusterScoped.IsClusterScopedTarget() {
		t.Error("a key with no namespace addresses a cluster-scoped target")
	}
	// Distinct targets must not collide, since this string is the index key.
	other := TargetKey{APIVersion: "v1", Kind: "ConfigMap", Namespace: "team-b", Name: "shared"}
	if namespaced.String() == other.String() {
		t.Error("two distinct targets produced the same key")
	}
}

// The tracker-selection rule, from DESIGN.md 3.5. It must be a function of the contributor set,
// never of arrival order, so a create race reaches the same answer whoever wins.
func TestTrackerKindFor(t *testing.T) {
	namespacedTarget := TargetKey{APIVersion: "v1", Kind: "ConfigMap", Namespace: "team-a", Name: "shared"}
	clusterTarget := TargetKey{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "shared"}

	tests := []struct {
		name        string
		key         TargetKey
		hasCluster  bool
		wantTracker string
	}{
		{"namespaced target, namespaced contributors", namespacedTarget, false, patchv1alpha1.KindSharedResource},
		{"namespaced target with a cluster contributor promotes", namespacedTarget, true, patchv1alpha1.KindClusterSharedResource},
		{"cluster-scoped target always cluster tracker", clusterTarget, false, patchv1alpha1.KindClusterSharedResource},
		{"cluster-scoped target with a cluster contributor", clusterTarget, true, patchv1alpha1.KindClusterSharedResource},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TrackerKindFor(tc.key, tc.hasCluster); got != tc.wantTracker {
				t.Errorf("got %q, want %q", got, tc.wantTracker)
			}
		})
	}
}

func TestTrackerNamespace(t *testing.T) {
	key := TargetKey{APIVersion: "v1", Kind: "ConfigMap", Namespace: "team-a", Name: "shared"}
	if got := TrackerNamespace(patchv1alpha1.KindSharedResource, key); got != "team-a" {
		t.Errorf("a namespaced tracker lives in the target's namespace, got %q", got)
	}
	if got := TrackerNamespace(patchv1alpha1.KindClusterSharedResource, key); got != "" {
		t.Errorf("a cluster-scoped tracker has no namespace, got %q", got)
	}
}

// Determinism is what makes a create race benign: both racers compute the same name, so one wins
// and the other's AlreadyExists is a normal outcome.
func TestTrackerNameIsDeterministic(t *testing.T) {
	key := TargetKey{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Namespace: "team-a", Name: "shared-ingress"}
	first := TrackerName(key)
	for i := 0; i < 10; i++ {
		if got := TrackerName(key); got != first {
			t.Fatalf("TrackerName is not deterministic: %q vs %q", first, got)
		}
	}
	if !strings.Contains(first, "ingress") {
		t.Errorf("the name should stay readable: %q", first)
	}
}

func TestTrackerNameIsValidAndDistinct(t *testing.T) {
	keys := []TargetKey{
		{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Namespace: "team-a", Name: "shared"},
		{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Namespace: "team-b", Name: "shared"},
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "team-a", Name: "shared"},
		{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "shared"},
		// Names that need sanitising.
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "team-a", Name: "Weird_Name!"},
		// A name long enough to force truncation.
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: strings.Repeat("n", 200), Name: strings.Repeat("x", 200)},
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: strings.Repeat("n", 200), Name: strings.Repeat("y", 200)},
	}

	seen := map[string]TargetKey{}
	for _, k := range keys {
		name := TrackerName(k)

		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			t.Errorf("TrackerName(%v) = %q is not a valid DNS-1123 subdomain: %v", k, name, errs)
		}
		if len(name) > validation.DNS1123SubdomainMaxLength {
			t.Errorf("name is %d characters, over the %d limit: %q",
				len(name), validation.DNS1123SubdomainMaxLength, name)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("name collision between %v and %v: both %q", prev, k, name)
		}
		seen[name] = k
	}
}

// Two targets that truncate or sanitise to the same readable form must still get distinct names,
// because the hash covers the whole key.
func TestTrackerNameDistinctAfterTruncation(t *testing.T) {
	long := strings.Repeat("a", 300)
	a := TrackerName(TargetKey{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: long + "1"})
	b := TrackerName(TargetKey{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: long + "2"})
	if a == b {
		t.Errorf("two long distinct names collided: %q", a)
	}
}

func TestSanitizeDNS1123(t *testing.T) {
	tests := map[string]string{
		"already-fine":   "already-fine",
		"Weird_Name!":    "weird-name",
		"UPPER":          "upper",
		"--leading":      "leading",
		"trailing--":     "trailing",
		"a..b":           "a..b",
		"!!!":            "",
		"multi___spaces": "multi-spaces",
	}
	for in, want := range tests {
		if got := sanitizeDNS1123(in); got != want {
			t.Errorf("sanitizeDNS1123(%q) = %q, want %q", in, got, want)
		}
	}
}

// A target whose name sanitises away entirely must still produce a valid tracker name.
func TestTrackerNameHandlesFullySanitizedAwayInput(t *testing.T) {
	name := TrackerName(TargetKey{APIVersion: "!!!", Kind: "!!!", Name: "!!!"})
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		t.Errorf("got %q: %v", name, errs)
	}
}
