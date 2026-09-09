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

package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

// These tests use a fake authorizer rather than envtest deliberately.
//
// envtest's API server authorizes permissively, so a SubjectAccessReview against it returns
// allowed unconditionally -- a denial test would pass for the wrong reason and prove nothing. The
// decision logic is exercised here where the answers are controlled, and the real wiring is
// covered by the kind-based e2e suite where RBAC is genuine.

func raw(s string) *runtime.RawExtension { return &runtime.RawExtension{Raw: []byte(s)} }

// resourceFor answers the RESTMapper question for the kinds these tests use.
func resourceFor(_, kind string) (string, bool, error) {
	switch kind {
	case "Ingress":
		return "ingresses", true, nil
	case "ConfigMap":
		return "configmaps", true, nil
	case "ClusterRole":
		return "clusterroles", false, nil
	}
	return strings.ToLower(kind) + "s", true, nil
}

func namespacedContributor(mutate func(*patchv1alpha1.ResourcePatch)) *patchv1alpha1.ResourcePatch {
	rp := &patchv1alpha1.ResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "team-a"},
		Spec: patchv1alpha1.ResourcePatchSpec{
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
			Patch: patchv1alpha1.PatchSpec{Value: raw(`{"spec":{"ingressClassName":"nginx"}}`)},
		},
	}
	if mutate != nil {
		mutate(rp)
	}
	return rp
}

func clusterContributor(mutate func(*patchv1alpha1.ClusterResourcePatch)) *patchv1alpha1.ClusterResourcePatch {
	crp := &patchv1alpha1.ClusterResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-rule"},
		Spec: patchv1alpha1.ResourcePatchSpec{
			Target: patchv1alpha1.TargetRef{
				Mode:       patchv1alpha1.TargetModeSingle,
				APIVersion: "networking.k8s.io/v1",
				Kind:       "Ingress",
				Name:       "shared-ingress",
				Namespace:  "platform",
			},
			Lifecycle: patchv1alpha1.LifecycleSpec{
				OnMissing: patchv1alpha1.OnMissingWait,
				OnRelease: patchv1alpha1.OnReleaseRevert,
			},
			Patch: patchv1alpha1.PatchSpec{Value: raw(`{"spec":{"ingressClassName":"nginx"}}`)},
		},
	}
	if mutate != nil {
		mutate(crp)
	}
	return crp
}

func checkStrings(checks []Check) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.String())
	}
	return out
}

func hasVerb(checks []Check, verb Verb) bool {
	for _, c := range checks {
		if c.Verb == verb {
			return true
		}
	}
	return false
}

// A create implies the writes the contributor will make, and no more.
func TestChecksForCreateWriteVerbs(t *testing.T) {
	checks, err := ChecksFor(OperationCreate, namespacedContributor(nil), nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []Verb{VerbUpdate, VerbPatch} {
		if !hasVerb(checks, want) {
			t.Errorf("a patch-only contributor must be checked for %q: %v", want, checkStrings(checks))
		}
	}
	// It cannot create or delete, so it must not be asked for those rights.
	for _, unwanted := range []Verb{VerbCreate, VerbDelete} {
		if hasVerb(checks, unwanted) {
			t.Errorf("a patch-only contributor should not require %q: %v", unwanted, checkStrings(checks))
		}
	}
}

func TestChecksForCreateIncludesCreateAndDelete(t *testing.T) {
	c := namespacedContributor(func(rp *patchv1alpha1.ResourcePatch) {
		rp.Spec.Lifecycle.OnMissing = patchv1alpha1.OnMissingCreate
		rp.Spec.Lifecycle.OnRelease = patchv1alpha1.OnReleaseDelete
	})
	checks, err := ChecksFor(OperationCreate, c, nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []Verb{VerbCreate, VerbDelete, VerbUpdate, VerbPatch} {
		if !hasVerb(checks, want) {
			t.Errorf("a creating contributor must be checked for %q: %v", want, checkStrings(checks))
		}
	}
}

// A namespaced contributor's checks are scoped to its own namespace, whatever its spec says. This
// is the containment property reaching into the authorization layer.
func TestChecksForNamespacedContributorUsesOwnNamespace(t *testing.T) {
	c := namespacedContributor(func(rp *patchv1alpha1.ResourcePatch) {
		rp.Spec.Target.Namespace = "platform" // rejected by validation, but must not leak here either
	})
	checks, err := ChecksFor(OperationCreate, c, nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range checks {
		if ch.Namespace != "team-a" {
			t.Errorf("check %q is scoped to %q; a ResourcePatch must only ever be checked in its own namespace",
				ch, ch.Namespace)
		}
	}
}

func TestChecksForClusterContributorUsesTargetNamespace(t *testing.T) {
	checks, err := ChecksFor(OperationCreate, clusterContributor(nil), nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ch := range checks {
		if ch.Namespace == "platform" {
			found = true
		}
	}
	if !found {
		t.Errorf("a ClusterResourcePatch must be checked in the namespace it names: %v", checkStrings(checks))
	}
}

// A cluster-scoped target has no namespace to check in.
func TestChecksForClusterScopedTarget(t *testing.T) {
	c := clusterContributor(func(crp *patchv1alpha1.ClusterResourcePatch) {
		crp.Spec.Target.APIVersion = "rbac.authorization.k8s.io/v1"
		crp.Spec.Target.Kind = "ClusterRole"
		crp.Spec.Target.Namespace = ""
		crp.Spec.Target.Name = "shared"
	})
	checks, err := ChecksFor(OperationCreate, c, nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range checks {
		if ch.Namespace != "" {
			t.Errorf("a cluster-scoped target must be checked cluster-wide, got namespace %q", ch.Namespace)
		}
		if ch.Resource != "clusterroles" {
			t.Errorf("resource = %q, want clusterroles", ch.Resource)
		}
	}
}

// Retargeting is the bypass a create-only check misses: admit a harmless patch, then point it
// somewhere else. The old target must be checked too, because the contributor will withdraw fields
// from it.
func TestChecksForUpdateCoversBothTargets(t *testing.T) {
	old := clusterContributor(nil)
	updated := clusterContributor(func(crp *patchv1alpha1.ClusterResourcePatch) {
		crp.Spec.Target.Namespace = "somewhere-else"
	})

	checks, err := ChecksFor(OperationUpdate, updated, old, resourceFor)
	if err != nil {
		t.Fatal(err)
	}

	sawOld, sawNew := false, false
	for _, ch := range checks {
		switch ch.Namespace {
		case "platform":
			sawOld = true
		case "somewhere-else":
			sawNew = true
		}
	}
	if !sawOld {
		t.Errorf("the target being left must still be checked: %v", checkStrings(checks))
	}
	if !sawNew {
		t.Errorf("the target being joined must be checked: %v", checkStrings(checks))
	}
}

func TestChecksForUpdateWithoutRetargetChecksOnce(t *testing.T) {
	old := clusterContributor(nil)
	same := clusterContributor(nil)

	checks, err := ChecksFor(OperationUpdate, same, old, resourceFor)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range checks {
		if ch.Namespace != "platform" {
			t.Errorf("unexpected extra target checked: %q", ch)
		}
	}
}

// The other create-only bypass: flip onRelease to Delete, then delete the object. So DELETE is
// checked, and the verb depends on the policy being removed.
func TestChecksForDeleteVerbDependsOnPolicy(t *testing.T) {
	tests := []struct {
		policy    patchv1alpha1.OnReleasePolicy
		wantVerb  Verb
		wantEmpty bool
	}{
		{patchv1alpha1.OnReleaseRevert, VerbUpdate, false},
		{patchv1alpha1.OnReleaseDelete, VerbDelete, false},
		// Orphan writes nothing to the target, so there is nothing to authorize.
		{patchv1alpha1.OnReleaseOrphan, "", true},
	}

	for _, tc := range tests {
		t.Run(string(tc.policy), func(t *testing.T) {
			c := namespacedContributor(func(rp *patchv1alpha1.ResourcePatch) {
				rp.Spec.Lifecycle.OnRelease = tc.policy
				if tc.policy == patchv1alpha1.OnReleaseDelete {
					rp.Spec.Lifecycle.OnMissing = patchv1alpha1.OnMissingCreate
				}
			})

			// A DELETE AdmissionReview carries oldObject and no object, so the policy has to be
			// read from the old side. Passing nil as the new object is what that looks like.
			checks, err := ChecksFor(OperationDelete, nil, c, resourceFor)
			if err != nil {
				t.Fatal(err)
			}

			if tc.wantEmpty {
				if len(checks) != 0 {
					t.Errorf("Orphan should require no checks, got %v", checkStrings(checks))
				}
				return
			}
			if !hasVerb(checks, tc.wantVerb) {
				t.Errorf("expected verb %q, got %v", tc.wantVerb, checkStrings(checks))
			}
		})
	}
}

// Reading the policy from the wrong side of a DELETE would fail open: a Delete policy would be
// checked as if it were an Orphan and wave the request through.
func TestChecksForDeleteWithNeitherObjectFails(t *testing.T) {
	if _, err := ChecksFor(OperationDelete, nil, nil, resourceFor); err == nil {
		t.Error("a DELETE carrying neither object nor oldObject must be an error, not an allow")
	}
}

func TestChecksForUnknownOperation(t *testing.T) {
	if _, err := ChecksFor(Operation("CONNECT"), namespacedContributor(nil), nil, resourceFor); err == nil {
		t.Error("an unsupported operation should be rejected")
	}
}

// Selector mode is checked with an empty resource name, which asks the stricter namespace-wide
// question. The matched names are not knowable at admission, and a selector may match objects that
// do not exist yet.
func TestChecksForSelectorModeUsesNamespaceWideCheck(t *testing.T) {
	c := namespacedContributor(func(rp *patchv1alpha1.ResourcePatch) {
		rp.Spec.Target.Mode = patchv1alpha1.TargetModeSelector
		rp.Spec.Target.Name = ""
		rp.Spec.Target.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}}
	})

	checks, err := ChecksFor(OperationCreate, c, nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range checks {
		if ch.Name != "" {
			t.Errorf("Selector mode must be checked namespace-wide, got a name-scoped check: %q", ch)
		}
	}
}

// serviceAccountRef gets its own impersonate check. Without it, naming a ServiceAccount would let
// a caller borrow privilege rather than voluntarily drop it.
func TestChecksForServiceAccountRefRequiresImpersonate(t *testing.T) {
	c := namespacedContributor(func(rp *patchv1alpha1.ResourcePatch) {
		rp.Spec.ServiceAccountRef = &patchv1alpha1.ServiceAccountRef{Name: "team-a-patcher"}
	})
	checks, err := ChecksFor(OperationCreate, c, nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}

	var impersonation *Check
	for i := range checks {
		if checks[i].Verb == VerbImpersonate {
			impersonation = &checks[i]
		}
	}
	if impersonation == nil {
		t.Fatalf("serviceAccountRef must require an impersonate check: %v", checkStrings(checks))
	}
	if impersonation.Resource != "serviceaccounts" {
		t.Errorf("resource = %q, want serviceaccounts", impersonation.Resource)
	}
	if impersonation.Name != "team-a-patcher" {
		t.Errorf("name = %q, want the referenced ServiceAccount", impersonation.Name)
	}
	// A namespaced contributor's ServiceAccount is resolved in its own namespace, so containment
	// covers the identity the write runs as.
	if impersonation.Namespace != "team-a" {
		t.Errorf("namespace = %q, want the contributor's own namespace", impersonation.Namespace)
	}
}

func TestChecksForClusterServiceAccountRefMayBeCrossNamespace(t *testing.T) {
	c := clusterContributor(func(crp *patchv1alpha1.ClusterResourcePatch) {
		crp.Spec.ServiceAccountRef = &patchv1alpha1.ServiceAccountRef{
			Name: "tenant-patcher", Namespace: "team-a",
		}
	})
	checks, err := ChecksFor(OperationCreate, c, nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range checks {
		if ch.Verb == VerbImpersonate && ch.Namespace != "team-a" {
			t.Errorf("a cluster contributor may name a ServiceAccount elsewhere; got %q", ch)
		}
	}
}

func TestChecksAreDeduplicatedAndOrdered(t *testing.T) {
	old := clusterContributor(nil)
	updated := clusterContributor(nil)
	checks, err := ChecksFor(OperationUpdate, updated, old, resourceFor)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	prev := ""
	for _, ch := range checks {
		s := ch.String()
		if seen[s] {
			t.Errorf("duplicate check %q", s)
		}
		seen[s] = true
		if prev != "" && s < prev {
			t.Errorf("checks are not ordered: %q came after %q", s, prev)
		}
		prev = s
	}
}

func TestChecksForBadAPIVersion(t *testing.T) {
	c := namespacedContributor(func(rp *patchv1alpha1.ResourcePatch) {
		rp.Spec.Target.APIVersion = "a/b/c"
	})
	if _, err := ChecksFor(OperationCreate, c, nil, resourceFor); err == nil {
		t.Error("an unparseable target apiVersion should be an error")
	}
}

// Without a RESTMapper the resource name is guessed. A wrong guess must fail closed: the principal
// will not hold rights on a resource that does not exist.
func TestChecksForWithoutResourceMapper(t *testing.T) {
	checks, err := ChecksFor(OperationCreate, namespacedContributor(nil), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) == 0 {
		t.Fatal("checks should still be produced without a mapper")
	}
	for _, ch := range checks {
		if ch.Resource == "" {
			t.Error("a check with an empty resource would be meaningless")
		}
	}
}

func TestGuessResource(t *testing.T) {
	tests := map[string]string{
		"Ingress":    "ingresses",
		"ConfigMap":  "configmaps",
		"Policy":     "policies",
		"Deployment": "deployments",
		"Box":        "boxes",
	}
	for kind, want := range tests {
		if got := guessResource(kind); got != want {
			t.Errorf("guessResource(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestCheckString(t *testing.T) {
	tests := []struct {
		check Check
		want  string
	}{
		{Check{Verb: VerbPatch, Group: "networking.k8s.io", Resource: "ingresses", Namespace: "team-a", Name: "x"},
			"patch ingresses.networking.k8s.io team-a/x"},
		{Check{Verb: VerbPatch, Group: "networking.k8s.io", Resource: "ingresses", Namespace: "team-a"},
			"patch ingresses.networking.k8s.io in namespace team-a"},
		{Check{Verb: VerbDelete, Resource: "clusterroles", Name: "shared"},
			"delete clusterroles shared"},
		{Check{Verb: VerbCreate, Resource: "clusterroles"},
			"create clusterroles (cluster-wide)"},
	}
	for _, tc := range tests {
		if got := tc.check.String(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

// --- Authorizer, against a fake SubjectAccessReview responder ---

// sarClient is a minimal client that only answers SubjectAccessReview creations.
type sarClient struct {
	client.Client
	decide func(*authv1.SubjectAccessReview) (allowed bool, denied bool, reason string)
	calls  []*authv1.SubjectAccessReview
	err    error
}

func (c *sarClient) Create(_ context.Context, obj client.Object, _ ...client.CreateOption) error {
	if c.err != nil {
		return c.err
	}
	sar, ok := obj.(*authv1.SubjectAccessReview)
	if !ok {
		return errors.New("unexpected object")
	}
	c.calls = append(c.calls, sar.DeepCopy())
	allowed, denied, reason := c.decide(sar)
	sar.Status = authv1.SubjectAccessReviewStatus{Allowed: allowed, Denied: denied, Reason: reason}
	return nil
}

func allowAll(*authv1.SubjectAccessReview) (bool, bool, string) { return true, false, "" }

func TestAuthorizeAllowsWhenEveryCheckPasses(t *testing.T) {
	sc := &sarClient{decide: allowAll}
	a := &Authorizer{Client: sc}

	checks, err := ChecksFor(OperationCreate, namespacedContributor(nil), nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}

	denial, err := a.Authorize(context.Background(), authenticationv1.UserInfo{Username: "alice"}, checks)
	if err != nil {
		t.Fatal(err)
	}
	if denial != nil {
		t.Errorf("expected no denial, got %+v", denial)
	}
	if len(sc.calls) != len(checks) {
		t.Errorf("issued %d SubjectAccessReviews for %d checks", len(sc.calls), len(checks))
	}
	// The subject must carry the requesting principal, not the operator's identity.
	if sc.calls[0].Spec.User != "alice" {
		t.Errorf("SAR subject = %q, want the requester", sc.calls[0].Spec.User)
	}
}

func TestAuthorizeDeniesOnFirstFailure(t *testing.T) {
	sc := &sarClient{decide: func(sar *authv1.SubjectAccessReview) (bool, bool, string) {
		if sar.Spec.ResourceAttributes.Verb == string(VerbPatch) {
			return false, false, "no patch for you"
		}
		return true, false, ""
	}}
	a := &Authorizer{Client: sc}

	checks, err := ChecksFor(OperationCreate, namespacedContributor(nil), nil, resourceFor)
	if err != nil {
		t.Fatal(err)
	}

	denial, err := a.Authorize(context.Background(), authenticationv1.UserInfo{Username: "alice"}, checks)
	if err != nil {
		t.Fatal(err)
	}
	if denial == nil {
		t.Fatal("expected a denial")
	}
	if denial.Check.Verb != VerbPatch {
		t.Errorf("denial names verb %q, want patch", denial.Check.Verb)
	}
	if denial.Reason != "no patch for you" {
		t.Errorf("reason = %q; the RBAC explanation should be surfaced", denial.Reason)
	}
}

// An explicit Denied must be honoured even alongside Allowed, which is how a deny authorizer
// reports an override.
func TestAuthorizeHonoursExplicitDenied(t *testing.T) {
	sc := &sarClient{decide: func(*authv1.SubjectAccessReview) (bool, bool, string) {
		return true, true, "denied by policy"
	}}
	a := &Authorizer{Client: sc}

	denial, err := a.Authorize(context.Background(),
		authenticationv1.UserInfo{Username: "alice"},
		[]Check{{Verb: VerbPatch, Resource: "configmaps", Namespace: "team-a", Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if denial == nil {
		t.Fatal("an explicit Denied must deny even when Allowed is also set")
	}
}

// A check that could not be completed is not an approval.
func TestAuthorizeFailsClosedOnError(t *testing.T) {
	sc := &sarClient{decide: allowAll, err: errors.New("apiserver unreachable")}
	a := &Authorizer{Client: sc}

	denial, err := a.Authorize(context.Background(),
		authenticationv1.UserInfo{Username: "alice"},
		[]Check{{Verb: VerbPatch, Resource: "configmaps", Namespace: "team-a"}})
	if err == nil {
		t.Fatal("a failed SubjectAccessReview must surface as an error, not an allow")
	}
	if denial != nil {
		t.Errorf("no denial should be returned alongside an error: %+v", denial)
	}
}

func TestAuthorizeForwardsGroupsAndExtra(t *testing.T) {
	sc := &sarClient{decide: allowAll}
	a := &Authorizer{Client: sc}

	user := authenticationv1.UserInfo{
		Username: "system:serviceaccount:crossplane-system:crossplane",
		Groups:   []string{"system:serviceaccounts", "system:authenticated"},
		UID:      "uid-1",
		Extra:    map[string]authenticationv1.ExtraValue{"scopes": {"a", "b"}},
	}
	if _, err := a.Authorize(context.Background(), user,
		[]Check{{Verb: VerbPatch, Resource: "configmaps", Namespace: "team-a"}}); err != nil {
		t.Fatal(err)
	}

	got := sc.calls[0].Spec
	if got.User != user.Username || got.UID != "uid-1" {
		t.Errorf("subject not forwarded: %+v", got)
	}
	if len(got.Groups) != 2 {
		t.Errorf("groups not forwarded: %+v", got.Groups)
	}
	// Groups matter: an RBAC binding to system:serviceaccounts is only honoured if they are sent.
	if len(got.Extra["scopes"]) != 2 {
		t.Errorf("extra not forwarded: %+v", got.Extra)
	}
}

// The re-check is what closes the revoked-RBAC gap: admission is point-in-time, and a contributor
// nothing ever touches again would otherwise keep writing forever.
func TestReauthorizerAllowsAndDenies(t *testing.T) {
	c := namespacedContributor(nil)

	t.Run("allowed", func(t *testing.T) {
		r := &Reauthorizer{Authorizer: &Authorizer{Client: &sarClient{decide: allowAll}}}
		ok, reason, err := r.Authorize(context.Background(), authenticationv1.UserInfo{Username: "alice"}, c)
		if err != nil || !ok {
			t.Fatalf("ok=%v reason=%q err=%v", ok, reason, err)
		}
	})

	t.Run("revoked", func(t *testing.T) {
		r := &Reauthorizer{Authorizer: &Authorizer{Client: &sarClient{
			decide: func(*authv1.SubjectAccessReview) (bool, bool, string) {
				return false, false, "binding removed"
			},
		}}}
		ok, reason, err := r.Authorize(context.Background(), authenticationv1.UserInfo{Username: "alice"}, c)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("a revoked principal must not re-authorize")
		}
		// The message has to say who lost what, or an operator cannot act on the condition.
		if !strings.Contains(reason, "alice") || !strings.Contains(reason, "binding removed") {
			t.Errorf("reason = %q; it should name the principal and the RBAC explanation", reason)
		}
	})
}

// A ServiceAccount principal must be re-checked with the groups the API server would attach, or a
// binding to system:serviceaccounts would be missed and a still-authorized contributor denied.
func TestReauthorizerAttachesServiceAccountGroups(t *testing.T) {
	sc := &sarClient{decide: allowAll}
	r := &Reauthorizer{Authorizer: &Authorizer{Client: sc}}

	sa := authenticationv1.UserInfo{Username: "system:serviceaccount:team-a:patcher"}
	if _, _, err := r.Authorize(context.Background(), sa, namespacedContributor(nil)); err != nil {
		t.Fatal(err)
	}
	if len(sc.calls) == 0 {
		t.Fatal("no SubjectAccessReview issued")
	}
	groups := sc.calls[0].Spec.Groups
	if len(groups) == 0 {
		t.Errorf("a ServiceAccount principal was re-checked with no groups: %+v", sc.calls[0].Spec)
	}
}

// The regression the kind e2e suite found. kubernetes-admin is authorized through system:masters,
// not by name, so a review carrying only the username denied a principal that was still fully
// authorized -- the operator stopped writing and a pending revert never completed.
func TestReauthorizerForwardsRecordedGroups(t *testing.T) {
	sc := &sarClient{decide: func(sar *authv1.SubjectAccessReview) (bool, bool, string) {
		// Stands in for RBAC bound to a group rather than a username.
		for _, g := range sar.Spec.Groups {
			if g == "system:masters" {
				return true, false, ""
			}
		}
		return false, false, "no binding for user " + sar.Spec.User
	}}
	r := &Reauthorizer{Authorizer: &Authorizer{Client: sc}}

	user := authenticationv1.UserInfo{
		Username: "kubernetes-admin",
		Groups:   []string{"system:masters", "system:authenticated"},
	}
	ok, reason, err := r.Authorize(context.Background(), user, namespacedContributor(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("a group-authorized principal was denied on re-check: %s", reason)
	}
	if len(sc.calls) == 0 {
		t.Fatal("no SubjectAccessReview issued")
	}
	if len(sc.calls[0].Spec.Groups) != 2 {
		t.Errorf("groups were not forwarded: %+v", sc.calls[0].Spec)
	}
}

// The reconstructed ServiceAccount groups must not overwrite groups that were actually recorded.
func TestReauthorizerKeepsRecordedServiceAccountGroups(t *testing.T) {
	sc := &sarClient{decide: allowAll}
	r := &Reauthorizer{Authorizer: &Authorizer{Client: sc}}

	user := authenticationv1.UserInfo{
		Username: "system:serviceaccount:team-a:patcher",
		Groups:   []string{"system:serviceaccounts:team-a", "custom-group"},
	}
	if _, _, err := r.Authorize(context.Background(), user, namespacedContributor(nil)); err != nil {
		t.Fatal(err)
	}
	got := sc.calls[0].Spec.Groups
	if len(got) != 2 || got[1] != "custom-group" {
		t.Errorf("recorded groups were replaced by reconstructed ones: %v", got)
	}
}

func TestReauthorizerReportsInvalidSpec(t *testing.T) {
	c := namespacedContributor(func(rp *patchv1alpha1.ResourcePatch) {
		rp.Spec.Target.APIVersion = "a/b/c"
	})
	r := &Reauthorizer{Authorizer: &Authorizer{Client: &sarClient{decide: allowAll}}}

	ok, reason, err := r.Authorize(context.Background(), authenticationv1.UserInfo{Username: "alice"}, c)
	if err != nil {
		t.Fatal(err)
	}
	if ok || reason == "" {
		t.Errorf("an unparseable spec should report not-authorized with a reason, got ok=%v reason=%q", ok, reason)
	}
}

func TestOperationsCoversAllThreeVerbs(t *testing.T) {
	ops := Operations()
	if len(ops) != 3 {
		t.Fatalf("expected CREATE, UPDATE and DELETE, got %v", ops)
	}
	// Registering for CREATE alone is the bypass this exists to close.
	seen := map[string]bool{}
	for _, o := range ops {
		seen[string(o)] = true
	}
	for _, want := range []string{"CREATE", "UPDATE", "DELETE"} {
		if !seen[want] {
			t.Errorf("missing operation %q: %v", want, ops)
		}
	}
}
