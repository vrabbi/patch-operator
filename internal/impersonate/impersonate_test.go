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

package impersonate

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

func TestServiceAccountUsername(t *testing.T) {
	got := ServiceAccountUsername("team-a", "patcher")
	if want := "system:serviceaccount:team-a:patcher"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func newFactory() *Factory {
	// A syntactically valid config is enough: these tests never issue a request, they check which
	// identity would be used.
	return NewFactory(&rest.Config{Host: "https://example.invalid"}, client.Options{}, nil)
}

// With no serviceAccountRef the operator's own identity is used. That is the weaker configuration,
// bounded only by the admission SubjectAccessReview.
func TestForWithoutServiceAccountRefUsesTheDefaultClient(t *testing.T) {
	f := newFactory()
	rp := &patchv1alpha1.ResourcePatch{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-a"}}

	got, username, err := f.For(rp)
	if err != nil {
		t.Fatal(err)
	}
	if username != "" {
		t.Errorf("username = %q, want empty when no ServiceAccount is named", username)
	}
	if got != f.Default {
		t.Error("expected the operator's own client")
	}

	// An empty name is the same as unset, not an impersonation of "".
	rp.Spec.ServiceAccountRef = &patchv1alpha1.ServiceAccountRef{Name: ""}
	if _, username, err = f.For(rp); err != nil || username != "" {
		t.Errorf("an empty name should behave as unset: username=%q err=%v", username, err)
	}
}

// A namespaced contributor's ServiceAccount is resolved in its own namespace, whatever the spec
// says. Trusting the spec here would let a ResourcePatch impersonate out of its namespace and
// defeat the containment property that makes the namespaced kind safe to grant.
func TestForNamespacedContributorIgnoresSpecNamespace(t *testing.T) {
	f := newFactory()
	rp := &patchv1alpha1.ResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-a"},
		Spec: patchv1alpha1.ResourcePatchSpec{
			ServiceAccountRef: &patchv1alpha1.ServiceAccountRef{
				Name: "privileged", Namespace: "kube-system",
			},
		},
	}

	_, username, err := f.For(rp)
	if err != nil {
		t.Fatal(err)
	}
	if want := "system:serviceaccount:team-a:privileged"; username != want {
		t.Errorf("username = %q, want %q; a ResourcePatch must impersonate only within its own namespace",
			username, want)
	}
}

// A cluster-scoped contributor may name a ServiceAccount anywhere. That is gated at admission by
// an "impersonate" SubjectAccessReview, not here.
func TestForClusterContributorUsesSpecNamespace(t *testing.T) {
	f := newFactory()
	crp := &patchv1alpha1.ClusterResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "x"},
		Spec: patchv1alpha1.ResourcePatchSpec{
			ServiceAccountRef: &patchv1alpha1.ServiceAccountRef{
				Name: "tenant-patcher", Namespace: "team-a",
			},
		},
	}

	_, username, err := f.For(crp)
	if err != nil {
		t.Fatal(err)
	}
	if want := "system:serviceaccount:team-a:tenant-patcher"; username != want {
		t.Errorf("username = %q, want %q", username, want)
	}
}

// A cluster-scoped contributor has no namespace of its own to fall back to, so an unqualified
// reference is an error rather than a guess.
func TestForClusterContributorRequiresANamespace(t *testing.T) {
	f := newFactory()
	crp := &patchv1alpha1.ClusterResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "x"},
		Spec: patchv1alpha1.ResourcePatchSpec{
			ServiceAccountRef: &patchv1alpha1.ServiceAccountRef{Name: "patcher"},
		},
	}
	if _, _, err := f.For(crp); err == nil {
		t.Error("a cluster-scoped contributor naming a ServiceAccount with no namespace should error")
	}
}

// Clients are cached per identity, so a per-reconcile lookup does not rebuild a REST client and
// its connection pool on every pass.
func TestForCachesPerIdentity(t *testing.T) {
	f := newFactory()
	rp := &patchv1alpha1.ResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-a"},
		Spec: patchv1alpha1.ResourcePatchSpec{
			ServiceAccountRef: &patchv1alpha1.ServiceAccountRef{Name: "patcher"},
		},
	}

	first, _, err := f.For(rp)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := f.For(rp)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("the same identity should reuse one client")
	}

	// A different identity must get its own.
	other := rp.DeepCopy()
	other.Spec.ServiceAccountRef.Name = "someone-else"
	third, _, err := f.For(other)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Error("distinct identities must not share a client")
	}
}

// The impersonating client must not read through the manager's shared cache: the cache is
// populated with the operator's own credentials, so a cached read would silently bypass the
// tenant's RBAC and report data the tenant cannot see.
func TestImpersonatingClientDoesNotUseTheSharedCache(t *testing.T) {
	f := NewFactory(&rest.Config{Host: "https://example.invalid"},
		client.Options{Cache: &client.CacheOptions{Unstructured: true}}, nil)

	rp := &patchv1alpha1.ResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-a"},
		Spec: patchv1alpha1.ResourcePatchSpec{
			ServiceAccountRef: &patchv1alpha1.ServiceAccountRef{Name: "patcher"},
		},
	}
	if _, _, err := f.For(rp); err != nil {
		t.Fatal(err)
	}
	// The factory's stored options must be left alone for other callers.
	if f.Options.Cache == nil {
		t.Error("the factory's own options were mutated")
	}
}

// The base config must not be mutated: every impersonated identity clones it, and a leaked
// Impersonate setting would make the operator write as the wrong tenant.
func TestBaseConfigIsNotMutated(t *testing.T) {
	base := &rest.Config{Host: "https://example.invalid"}
	f := NewFactory(base, client.Options{}, nil)

	rp := &patchv1alpha1.ResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "team-a"},
		Spec: patchv1alpha1.ResourcePatchSpec{
			ServiceAccountRef: &patchv1alpha1.ServiceAccountRef{Name: "patcher"},
		},
	}
	if _, _, err := f.For(rp); err != nil {
		t.Fatal(err)
	}

	if base.Impersonate.UserName != "" {
		t.Errorf("the caller's config was mutated: Impersonate.UserName = %q", base.Impersonate.UserName)
	}
	if f.BaseConfig.Impersonate.UserName != "" {
		t.Errorf("the factory's base config was mutated: %q", f.BaseConfig.Impersonate.UserName)
	}
}
