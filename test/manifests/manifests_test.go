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

// Package manifests asserts what the kustomize overlays actually render.
//
// The CI job that builds both overlays proves only that they *render*, never what they contain.
// That gap shipped a namespace-only overlay whose RBAC patch indexed into the wrong rules: it
// stripped the operator's events, namespaces and impersonation rights -- silently disabling
// spec.serviceAccountRef -- while leaving the cluster-scoped grants it existed to remove fully
// intact. `kustomize build` succeeded throughout, because the paths existed.
//
// So these tests read the rendered output and assert the properties each overlay is supposed to
// establish. They run against the repo's pinned bin/kustomize, so they check exactly what
// `make deploy` and `make build-installer` produce.
package manifests

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// render runs the pinned kustomize over one overlay and returns every object it emits.
//
// Skips when the binary is absent so `go test ./...` works in a bare checkout -- but honours
// REQUIRE_KUSTOMIZE, which CI sets. A skip that reads as a pass is the failure mode worth
// designing against: it is how the e2e suite reported success for a run in which no test executed.
func render(t *testing.T, overlay string, extraArgs ...string) []*unstructured.Unstructured {
	t.Helper()

	bin := filepath.Join(projectRoot(), "bin", "kustomize")
	if _, err := os.Stat(bin); err != nil {
		if os.Getenv("REQUIRE_KUSTOMIZE") != "" {
			t.Fatalf("REQUIRE_KUSTOMIZE is set but %s is missing; run `make kustomize`", bin)
		}
		t.Skipf("no kustomize at %s; run `make kustomize` or `make test`", bin)
	}

	args := append(append([]string{"build"}, extraArgs...), filepath.Join(projectRoot(), overlay))
	cmd := exec.Command(bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kustomize build %s: %v\n%s", overlay, err, stderr.String())
	}

	var objs []*unstructured.Unstructured
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			break
		}
		if len(obj.Object) == 0 {
			continue
		}
		objs = append(objs, obj)
	}
	if len(objs) == 0 {
		t.Fatalf("%s rendered no objects", overlay)
	}
	return objs
}

func projectRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// namespacedOnly renders the namespace-only overlay. The load restrictor is required because the
// overlay selects a subset of the generated CRDs, which lives above its own directory.
func namespacedOnly(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	return render(t, "config/overlays/namespaced-only", "--load-restrictor", "LoadRestrictionsNone")
}

func defaultOverlay(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	return render(t, "config/default")
}

// --- lookup helpers ---

// findByKind returns every object of a kind.
func findByKind(objs []*unstructured.Unstructured, kind string) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if o.GetKind() == kind {
			out = append(out, o)
		}
	}
	return out
}

// managerRole returns the operator's own ClusterRole, decoded.
//
// Matched by name suffix because kustomize's namePrefix has already been applied by this point.
func managerRole(t *testing.T, objs []*unstructured.Unstructured) *rbacv1.ClusterRole {
	t.Helper()
	for _, o := range findByKind(objs, "ClusterRole") {
		if !strings.HasSuffix(o.GetName(), "manager-role") {
			continue
		}
		role := &rbacv1.ClusterRole{}
		if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(o.Object, role); err != nil {
			t.Fatalf("decoding %s: %v", o.GetName(), err)
		}
		return role
	}
	t.Fatal("no manager-role ClusterRole was rendered")
	return nil
}

// crdKinds maps each rendered CRD's kind to its scope.
func crdKinds(t *testing.T, objs []*unstructured.Unstructured) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, o := range findByKind(objs, "CustomResourceDefinition") {
		kind, _, err := unstructured.NestedString(o.Object, "spec", "names", "kind")
		if err != nil || kind == "" {
			t.Fatalf("CRD %s has no spec.names.kind", o.GetName())
		}
		scope, _, _ := unstructured.NestedString(o.Object, "spec", "scope")
		out[kind] = scope
	}
	return out
}

// webhookPaths returns the clientConfig paths a webhook configuration registers.
func webhookPaths(t *testing.T, obj *unstructured.Unstructured) []string {
	t.Helper()
	hooks, _, err := unstructured.NestedSlice(obj.Object, "webhooks")
	if err != nil {
		t.Fatalf("reading webhooks of %s: %v", obj.GetName(), err)
	}
	out := make([]string, 0, len(hooks))
	for _, h := range hooks {
		m, ok := h.(map[string]any)
		if !ok {
			t.Fatalf("malformed webhook entry in %s", obj.GetName())
		}
		path, _, _ := unstructured.NestedString(m, "clientConfig", "service", "path")
		out = append(out, path)
	}
	return out
}

// rbacResources returns every resource named by any rule on an object that has rules at all
// (ClusterRole, Role), and nothing for anything else.
func rbacResources(t *testing.T, obj *unstructured.Unstructured) []string {
	t.Helper()
	if obj.GetKind() != "ClusterRole" && obj.GetKind() != "Role" {
		return nil
	}
	rules, found, err := unstructured.NestedSlice(obj.Object, "rules")
	if err != nil || !found {
		return nil
	}
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("malformed rule in %s %s", obj.GetKind(), obj.GetName())
		}
		resources, _, _ := unstructured.NestedStringSlice(m, "resources")
		out = append(out, resources...)
	}
	return out
}

// grants reports whether the role allows every one of verbs on group/resource.
func grants(role *rbacv1.ClusterRole, group, resource string, verbs ...string) bool {
	for _, r := range role.Rules {
		if !contains(r.APIGroups, group) || !contains(r.Resources, resource) {
			continue
		}
		all := true
		for _, v := range verbs {
			if !contains(r.Verbs, v) && !contains(r.Verbs, "*") {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// allResources flattens every resource named anywhere in the role.
func allResources(role *rbacv1.ClusterRole) []string {
	var out []string
	for _, r := range role.Rules {
		out = append(out, r.Resources...)
	}
	return out
}

// --- the namespace-only overlay ---

// The property this overlay exists to establish, stated once and over the whole render.
//
// Deliberately not scoped to the manager role: the first version of this test only checked that
// one, and missed a clusterresourcepatch-editor ClusterRole the overlay still shipped for a kind
// it does not install. Scanning every object is the property actually wanted.
func TestNamespacedOnlyMentionsNoClusterScopedKindAnywhere(t *testing.T) {
	for _, obj := range namespacedOnly(t) {
		for _, res := range rbacResources(t, obj) {
			base := strings.SplitN(res, "/", 2)[0]
			if base == "clusterresourcepatches" || base == "clustersharedresources" {
				t.Errorf("%s %q grants %q; that CRD is not installed in this overlay",
					obj.GetKind(), obj.GetName(), res)
			}
		}
	}
}

// The manager role specifically, since it is the one a mis-indexed patch broke.
func TestNamespacedOnlyRoleGrantsNothingOverClusterScopedKinds(t *testing.T) {
	role := managerRole(t, namespacedOnly(t))

	for _, res := range allResources(role) {
		base := strings.SplitN(res, "/", 2)[0]
		if base == "clusterresourcepatches" || base == "clustersharedresources" {
			t.Errorf("the namespace-only role still grants %q; those CRDs are not even installed here", res)
		}
	}
}

// The regression that motivated this whole package. Removing impersonation does not fail loudly --
// spec.serviceAccountRef simply stops working, and it is the strongest control in the design.
func TestNamespacedOnlyRoleKeepsTheRightsTheOperatorNeeds(t *testing.T) {
	role := managerRole(t, namespacedOnly(t))

	for _, tc := range []struct {
		what     string
		group    string
		resource string
		verbs    []string
	}{
		{"impersonation, or spec.serviceAccountRef silently stops working", "", "serviceaccounts", []string{"impersonate"}},
		{"impersonation of users", "", "users", []string{"impersonate"}},
		{"impersonation of groups", "", "groups", []string{"impersonate"}},
		{"event recording", "", "events", []string{"create", "patch"}},
		{"namespace resolution", "", "namespaces", []string{"get", "list", "watch"}},
		{"the SubjectAccessReview boundary", "authorization.k8s.io", "subjectaccessreviews", []string{"create"}},
		{"leader election", "coordination.k8s.io", "leases", []string{"create", "get", "update"}},
		{"the wildcard target grant", "*", "*", []string{"get", "patch", "update", "create", "delete"}},
	} {
		if !grants(role, tc.group, tc.resource, tc.verbs...) {
			t.Errorf("the namespace-only role lost %s (%s/%s %v)",
				tc.what, tc.group, tc.resource, tc.verbs)
		}
	}
}

// The kinds this install does serve still need full rights, subresources included.
func TestNamespacedOnlyRoleServesTheNamespacedKinds(t *testing.T) {
	role := managerRole(t, namespacedOnly(t))

	write := []string{"create", "delete", "get", "list", "patch", "update", "watch"}
	for _, res := range []string{"resourcepatches", "sharedresources"} {
		if !grants(role, "terasky.com", res, write...) {
			t.Errorf("missing full rights on terasky.com/%s", res)
		}
		if !grants(role, "terasky.com", res+"/status", "get", "patch", "update") {
			t.Errorf("missing status rights on terasky.com/%s/status", res)
		}
		if !grants(role, "terasky.com", res+"/finalizers", "update") {
			t.Errorf("missing finalizer rights on terasky.com/%s/finalizers", res)
		}
	}
}

// Cross-namespace contribution must be absent rather than merely denied: with no
// ClusterResourcePatch kind installed, there is no API to express it.
func TestNamespacedOnlyInstallsOnlyTheNamespacedCRDs(t *testing.T) {
	got := crdKinds(t, namespacedOnly(t))

	want := map[string]string{"ResourcePatch": "Namespaced", "SharedResource": "Namespaced"}
	if len(got) != len(want) {
		t.Errorf("rendered CRDs = %v, want exactly %v", got, want)
	}
	for kind, scope := range want {
		if got[kind] != scope {
			t.Errorf("CRD %s scope = %q, want %q", kind, got[kind], scope)
		}
	}
}

// A webhook still registered for a kind that is not installed would reject nothing and confuse
// anyone reading the configuration.
func TestNamespacedOnlyWebhooksCoverOnlyTheNamespacedKind(t *testing.T) {
	objs := namespacedOnly(t)

	for _, kind := range []string{"ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"} {
		configs := findByKind(objs, kind)
		if len(configs) != 1 {
			t.Fatalf("expected exactly one %s, got %d", kind, len(configs))
		}
		paths := webhookPaths(t, configs[0])
		if len(paths) != 1 {
			t.Errorf("%s registers %v; only the resourcepatch path belongs in this install", kind, paths)
		}
		for _, p := range paths {
			if strings.Contains(p, "clusterresourcepatch") {
				t.Errorf("%s still registers %q for a kind this install does not have", kind, p)
			}
		}
	}
}

// The safe tenant role must survive: dropping the privileged one must not take it along.
func TestNamespacedOnlyKeepsTheNamespacedTenantRoles(t *testing.T) {
	objs := namespacedOnly(t)

	names := clusterRoleNames(objs)
	for _, want := range []string{"resourcepatch-editor", "resourcepatch-viewer"} {
		if !anyHasSuffix(names, want) {
			t.Errorf("%s is missing from the namespace-only install; rendered: %v", want, names)
		}
	}
	if anyHasSuffix(names, "clusterresourcepatch-editor") {
		t.Errorf("clusterresourcepatch-editor is still shipped; an admin could bind a role that "+
			"cannot work, and would quietly start working if the cluster CRDs appeared: %v", names)
	}
}

// clusterRoleNames lists the ClusterRoles a render emits.
func clusterRoleNames(objs []*unstructured.Unstructured) []string {
	roles := findByKind(objs, "ClusterRole")
	out := make([]string, 0, len(roles))
	for _, o := range roles {
		out = append(out, o.GetName())
	}
	return out
}

func anyHasSuffix(names []string, suffix string) bool {
	for _, n := range names {
		if strings.HasSuffix(n, suffix) {
			return true
		}
	}
	return false
}

// --- the default overlay ---

func TestDefaultInstallsAllFourCRDsWithCorrectScopes(t *testing.T) {
	got := crdKinds(t, defaultOverlay(t))

	want := map[string]string{
		"ResourcePatch":         "Namespaced",
		"SharedResource":        "Namespaced",
		"ClusterResourcePatch":  "Cluster",
		"ClusterSharedResource": "Cluster",
	}
	for kind, scope := range want {
		if got[kind] != scope {
			t.Errorf("CRD %s scope = %q, want %q (rendered: %v)", kind, got[kind], scope, got)
		}
	}
}

func TestDefaultRoleGrantsTheClusterScopedKinds(t *testing.T) {
	role := managerRole(t, defaultOverlay(t))

	write := []string{"create", "delete", "get", "list", "patch", "update", "watch"}
	for _, res := range []string{"clusterresourcepatches", "clustersharedresources"} {
		if !grants(role, "terasky.com", res, write...) {
			t.Errorf("the default role is missing rights on terasky.com/%s", res)
		}
	}
	// The privileged tenant role belongs in this install, and only this one.
	names := clusterRoleNames(defaultOverlay(t))
	if !anyHasSuffix(names, "clusterresourcepatch-editor") {
		t.Errorf("the default install lost clusterresourcepatch-editor: %v", names)
	}

	// And the shared rules the namespace-only overlay once ate.
	if !grants(role, "", "serviceaccounts", "impersonate") {
		t.Error("the default role lost impersonation")
	}
	if !grants(role, "", "events", "create", "patch") {
		t.Error("the default role lost event recording")
	}
}

// Both webhook configurations must point at the prefixed Service and carry the cert-manager
// annotation. The Issuer/Certificate wiring behind this broke every install once: namePrefix
// renamed the Issuer and left issuerRef.name stale, so the serving Secret never appeared and every
// operator pod hung on FailedMount. Pinned here so it cannot come back quietly.
func TestDefaultWebhooksArePointedAtTheServiceAndTheCA(t *testing.T) {
	objs := defaultOverlay(t)

	for _, kind := range []string{"ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"} {
		configs := findByKind(objs, kind)
		if len(configs) != 1 {
			t.Fatalf("expected exactly one %s, got %d", kind, len(configs))
		}
		cfg := configs[0]

		ca := cfg.GetAnnotations()["cert-manager.io/inject-ca-from"]
		if !strings.Contains(ca, "/") || strings.Contains(ca, "CERTIFICATE_") {
			t.Errorf("%s inject-ca-from = %q; the replacement did not resolve", kind, ca)
		}

		hooks, _, _ := unstructured.NestedSlice(cfg.Object, "webhooks")
		if len(hooks) != 2 {
			t.Errorf("%s registers %d webhooks, want both contributor kinds", kind, len(hooks))
		}
		for _, h := range hooks {
			m := h.(map[string]any)
			name, _, _ := unstructured.NestedString(m, "clientConfig", "service", "name")
			ns, _, _ := unstructured.NestedString(m, "clientConfig", "service", "namespace")
			if !strings.HasSuffix(name, "webhook-service") || ns == "system" {
				t.Errorf("%s clientConfig service = %s/%s; namePrefix or namespace was not applied",
					kind, ns, name)
			}
		}
	}
}

// The Certificate must reference the Issuer by the name it actually has after namePrefix.
func TestDefaultCertificateReferencesTheRenamedIssuer(t *testing.T) {
	objs := defaultOverlay(t)

	certs := findByKind(objs, "Certificate")
	if len(certs) != 1 {
		t.Fatalf("expected one Certificate, got %d", len(certs))
	}
	issuerRef, _, _ := unstructured.NestedString(certs[0].Object, "spec", "issuerRef", "name")

	issuers := findByKind(objs, "Issuer")
	if len(issuers) != 1 {
		t.Fatalf("expected one Issuer, got %d", len(issuers))
	}
	if issuerRef != issuers[0].GetName() {
		t.Errorf("Certificate.spec.issuerRef.name = %q but the Issuer is %q; "+
			"the certificate would never be issued and every pod would hang on FailedMount",
			issuerRef, issuers[0].GetName())
	}
}

// Guards the test itself: if kustomize ever stops emitting a manager-role, every RBAC assertion
// above would vacuously pass.
func TestBothOverlaysRenderAManagerRole(t *testing.T) {
	for name, objs := range map[string][]*unstructured.Unstructured{
		"default":         defaultOverlay(t),
		"namespaced-only": namespacedOnly(t),
	} {
		role := managerRole(t, objs)
		if len(role.Rules) == 0 {
			t.Errorf("%s rendered a manager-role with no rules", name)
		}
		fmt.Fprintf(os.Stderr, "%s manager-role: %d rules\n", name, len(role.Rules))
	}
}
