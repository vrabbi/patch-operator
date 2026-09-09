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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/render"
)

// --- unit tests: no API server needed ---

func TestFieldManagerFor(t *testing.T) {
	tests := []struct {
		kind, ns, name string
		want           string
	}{
		{"ResourcePatch", "team-a", "checkout", "patch-operator/team-a/checkout"},
		{"ClusterResourcePatch", "", "platform-rule", "patch-operator/platform-rule"},
	}
	for _, tc := range tests {
		if got := FieldManagerFor(tc.kind, tc.ns, tc.name); got != tc.want {
			t.Errorf("FieldManagerFor(%q,%q,%q) = %q, want %q", tc.kind, tc.ns, tc.name, got, tc.want)
		}
	}
}

// A field manager over the API server's limit must be truncated but stay distinct, or two long
// contributor names would collide and silently share field ownership.
func TestFieldManagerTruncationStaysDistinct(t *testing.T) {
	longA := strings.Repeat("a", 200)
	longB := strings.Repeat("a", 199) + "b"

	gotA := FieldManagerFor("ResourcePatch", "ns", longA)
	gotB := FieldManagerFor("ResourcePatch", "ns", longB)

	if len(gotA) > maxFieldManagerLength {
		t.Errorf("length %d exceeds the limit of %d", len(gotA), maxFieldManagerLength)
	}
	if gotA == gotB {
		t.Error("two distinct long names truncated to the same field manager")
	}
	if !strings.HasPrefix(gotA, patchv1alpha1.FieldManagerPrefix) {
		t.Errorf("truncation lost the prefix that marks a manager as ours: %q", gotA)
	}
}

func TestIsOurManager(t *testing.T) {
	if !IsOurManager("patch-operator/team-a/x") {
		t.Error("our own manager not recognised")
	}
	for _, foreign := range []string{"kubectl-client-side-apply", "provider-kubernetes/object-x", ""} {
		if IsOurManager(foreign) {
			t.Errorf("%q should not be recognised as ours", foreign)
		}
	}
}

// AllOurs gates conflictPolicy: Priority. It must be false for an empty list, or an unattributed
// conflict would be force-resolved against an unknown owner.
func TestAllOurs(t *testing.T) {
	tests := []struct {
		in   []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"patch-operator/a"}, true},
		{[]string{"patch-operator/a", "patch-operator/b"}, true},
		{[]string{"patch-operator/a", "kubectl"}, false},
		{[]string{"kubectl"}, false},
	}
	for _, tc := range tests {
		if got := AllOurs(tc.in); got != tc.want {
			t.Errorf("AllOurs(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestForSelectsApplier(t *testing.T) {
	ssa, err := For(patchv1alpha1.ApplyModeServerSideApply, nil)
	if err != nil || ssa.Mode() != patchv1alpha1.ApplyModeServerSideApply {
		t.Errorf("SSA: %v, %v", ssa, err)
	}
	// An empty mode must default to SSA, matching the CRD default.
	def, err := For("", nil)
	if err != nil || def.Mode() != patchv1alpha1.ApplyModeServerSideApply {
		t.Errorf("default: %v, %v", def, err)
	}
	csa, err := For(patchv1alpha1.ApplyModeClientSideApply, nil)
	if err != nil || csa.Mode() != patchv1alpha1.ApplyModeClientSideApply {
		t.Errorf("CSA: %v, %v", csa, err)
	}
	if _, err := For(patchv1alpha1.ApplyMode("Nope"), nil); err == nil {
		t.Error("an unknown mode should be rejected")
	}
}

// A resourceVersion conflict and a field-ownership conflict are both HTTP 409 but mean different
// things: one is retryable, the other needs a policy decision. Confusing them would make the
// operator retry a disagreement forever.
func TestIsFieldConflictDistinguishesFromOptimisticConcurrency(t *testing.T) {
	rvConflict := apierrors.NewConflict(
		schemaGroupResource(render.Target{APIVersion: "v1", Kind: "ConfigMap"}),
		"shared", nil)
	if IsFieldConflict(rvConflict) {
		t.Error("a resourceVersion conflict was misread as a field conflict")
	}
	if IsFieldConflict(nil) {
		t.Error("nil is not a conflict")
	}
	if IsFieldConflict(apierrors.NewNotFound(schemaGroupResource(render.Target{Kind: "ConfigMap"}), "x")) {
		t.Error("NotFound is not a conflict")
	}
}

func TestConflictHelpersOnNonStatusError(t *testing.T) {
	if got := ConflictingManagers(nil); got != nil {
		t.Errorf("ConflictingManagers(nil) = %v", got)
	}
	if got := ConflictingFields(nil); got != nil {
		t.Errorf("ConflictingFields(nil) = %v", got)
	}
}

func TestConflictError(t *testing.T) {
	c := &Conflict{Fields: []string{"data.k"}, Managers: []string{"other"}}
	if !strings.Contains(c.Error(), "data.k") || !strings.Contains(c.Error(), "other") {
		t.Errorf("Conflict.Error() = %q; should name both the field and the owner", c.Error())
	}
}

func TestDeepMergeContributionWins(t *testing.T) {
	base := map[string]any{
		"spec": map[string]any{"ingressClassName": "from-base", "keep": "kept"},
	}
	contribution := map[string]any{
		"spec": map[string]any{"ingressClassName": "from-contribution"},
	}

	got, err := mergeBaseAndContribution(base, contribution)
	if err != nil {
		t.Fatal(err)
	}
	spec := got["spec"].(map[string]any)
	if spec["ingressClassName"] != "from-contribution" {
		t.Errorf("the base overrode the contribution: %#v", spec)
	}
	if spec["keep"] != "kept" {
		t.Errorf("merging dropped a base-only field: %#v", spec)
	}
	// The base must not be mutated: it is decoded once per reconcile and reused.
	if base["spec"].(map[string]any)["ingressClassName"] != "from-base" {
		t.Error("merge mutated the base")
	}
}

func TestEqualContent(t *testing.T) {
	a := map[string]any{"x": 1, "y": map[string]any{"z": 2}}
	b := map[string]any{"y": map[string]any{"z": 2}, "x": 1}
	if !equalContent(a, b) {
		t.Error("key order should not affect equality")
	}
	if equalContent(a, map[string]any{"x": 2}) {
		t.Error("different content compared equal")
	}
}

// --- integration tests: real API server ---

func rawExt(s string) *runtime.RawExtension { return &runtime.RawExtension{Raw: []byte(s)} }

func cmTarget(ns, name string) render.Target {
	return render.Target{APIVersion: "v1", Kind: "ConfigMap", Name: name, Namespace: ns}
}

func mustRender(t *testing.T, spec *patchv1alpha1.PatchSpec, target render.Target) *render.Contribution {
	t.Helper()
	c, err := render.Render(spec, target)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return c
}

func liveData(t *testing.T, ctx context.Context, ns, name string) map[string]string {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatalf("get: %v", err)
	}
	m, _, _ := unstructured.NestedStringMap(obj.Object, "data")
	return m
}

func TestSSAApplierAppliesAndReverts(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ServerSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")
	mgr := FieldManagerFor("ResourcePatch", ns, "contributor-a")

	res, err := a.Apply(ctx, Request{
		Target:       target,
		Contribution: mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"key-a":"value-a"}}`)}, target),
		FieldManager: mgr,
		AllowCreate:  true,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Created {
		t.Error("first apply should report Created")
	}
	if got := liveData(t, ctx, ns, "shared"); got["key-a"] != "value-a" {
		t.Fatalf("data = %#v", got)
	}

	if err := a.Revert(ctx, RevertRequest{Target: target, FieldManager: mgr}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got := liveData(t, ctx, ns, "shared"); len(got) != 0 {
		t.Errorf("revert left data behind: %#v", got)
	}
}

// A patch-only contributor must not create its target. It waits instead, which is what
// onMissing: Wait means.
func TestSSAApplierRefusesToCreateWhenNotAllowed(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ServerSideApplier{Client: k8sClient}
	target := cmTarget(ns, "absent")

	_, err := a.Apply(ctx, Request{
		Target:       target,
		Contribution: mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"v"}}`)}, target),
		FieldManager: "patch-operator/a",
		AllowCreate:  false,
	})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
	// And crucially, the object must not have been created as a side effect.
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("v1")
	obj.SetKind("ConfigMap")
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "absent"}, obj); !apierrors.IsNotFound(err) {
		t.Error("a patch-only contributor created its target")
	}
}

// conflictPolicy: Fail must report the conflict and write nothing.
func TestSSAApplierConflictPolicyFail(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ServerSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")

	if _, err := a.Apply(ctx, Request{
		Target:       target,
		Contribution: mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"from-a"}}`)}, target),
		FieldManager: "patch-operator/a",
		AllowCreate:  true,
	}); err != nil {
		t.Fatalf("A apply: %v", err)
	}

	res, err := a.Apply(ctx, Request{
		Target:         target,
		Contribution:   mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"from-b"}}`)}, target),
		FieldManager:   "patch-operator/b",
		ConflictPolicy: patchv1alpha1.ConflictPolicyFail,
		AllowCreate:    true,
	})
	if err != nil {
		t.Fatalf("a reported conflict should not be an error: %v", err)
	}
	if res.Conflict == nil {
		t.Fatal("no conflict reported")
	}
	if !res.Conflict.AllOurs {
		t.Errorf("both managers are ours, so AllOurs should be true: %#v", res.Conflict)
	}
	if got := liveData(t, ctx, ns, "shared"); got["k"] != "from-a" {
		t.Errorf("Fail policy still changed the value: %#v", got)
	}
}

// conflictPolicy: Priority forces, but only because the other owner is also ours.
func TestSSAApplierConflictPolicyPriorityForcesOurs(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ServerSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")

	if _, err := a.Apply(ctx, Request{
		Target:       target,
		Contribution: mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"from-a"}}`)}, target),
		FieldManager: "patch-operator/a",
		AllowCreate:  true,
	}); err != nil {
		t.Fatalf("A apply: %v", err)
	}

	res, err := a.Apply(ctx, Request{
		Target:         target,
		Contribution:   mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"from-b"}}`)}, target),
		FieldManager:   "patch-operator/b",
		ConflictPolicy: patchv1alpha1.ConflictPolicyPriority,
		AllowCreate:    true,
	})
	if err != nil {
		t.Fatalf("priority apply: %v", err)
	}
	if res.Conflict != nil {
		t.Fatalf("priority should have forced an internal conflict, got %#v", res.Conflict)
	}
	if got := liveData(t, ctx, ns, "shared"); got["k"] != "from-b" {
		t.Errorf("priority did not take the field: %#v", got)
	}
}

// conflictPolicy: Priority must NOT force a foreign controller out. Stealing a field from
// something that will write it straight back is a flap, not a resolution.
func TestSSAApplierPriorityDoesNotForceForeignManager(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	// A foreign controller owns the field.
	foreign := configMap(ns, "shared", map[string]any{"k": "from-foreign"})
	if err := ssaApply(ctx, t, foreign, "some-other-controller", false); err != nil {
		t.Fatalf("foreign apply: %v", err)
	}

	a := &ServerSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")

	res, err := a.Apply(ctx, Request{
		Target:         target,
		Contribution:   mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"from-us"}}`)}, target),
		FieldManager:   "patch-operator/b",
		ConflictPolicy: patchv1alpha1.ConflictPolicyPriority,
		AllowCreate:    true,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Conflict == nil {
		t.Fatal("priority forced a foreign manager out; it must report instead")
	}
	if res.Conflict.AllOurs {
		t.Errorf("AllOurs should be false against a foreign owner: %#v", res.Conflict)
	}
	if got := liveData(t, ctx, ns, "shared"); got["k"] != "from-foreign" {
		t.Errorf("the foreign controller's value was overwritten: %#v", got)
	}
}

func TestSSARevertOnMissingTargetSucceeds(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ServerSideApplier{Client: k8sClient}
	// Releasing a contributor whose target is already gone must succeed, or the finalizer wedges.
	if err := a.Revert(ctx, RevertRequest{
		Target:       cmTarget(ns, "never-existed"),
		FieldManager: "patch-operator/a",
	}); err != nil {
		t.Errorf("revert against a missing target should succeed, got %v", err)
	}
}

// The headline CSA property, on the exact case that motivates the mode: Ingress.spec.rules has no
// listMapKey, so it is atomic and server-side apply cannot let two managers both append to it.
// Under ClientSideApply with a declared merge key, both rules land -- and one contributor
// reverting leaves the other's rule untouched.
func TestCSATwoContributorsShareAtomicIngressRules(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ClientSideApplier{Client: k8sClient}
	target := render.Target{
		APIVersion: "networking.k8s.io/v1", Kind: "Ingress",
		Name: "shared-ingress", Namespace: ns,
	}
	mergeKeys := []patchv1alpha1.MergeKey{{Path: "spec.rules", Key: "host"}}

	rule := func(host, svc string) string {
		return `{"spec":{"rules":[{"host":"` + host + `","http":{"paths":[{"path":"/","pathType":"Prefix",` +
			`"backend":{"service":{"name":"` + svc + `","port":{"number":80}}}}]}}]}}`
	}

	base, err := render.DecodeBase(rawExt(
		`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","spec":{"ingressClassName":"nginx"}}`))
	if err != nil {
		t.Fatal(err)
	}

	// Contributor A creates the Ingress and adds its own rule.
	contribA := mustRender(t, &patchv1alpha1.PatchSpec{
		Value: rawExt(rule("a.example.com", "svc-a")), MergeKeys: mergeKeys,
	}, target)
	resA, err := a.Apply(ctx, Request{
		Target: target, Contribution: contribA,
		FieldManager: "patch-operator/" + ns + "/a", Base: base, AllowCreate: true,
	})
	if err != nil {
		t.Fatalf("A apply: %v", err)
	}
	if !resA.Created {
		t.Error("A should have created the Ingress")
	}
	// The owned path must be keyed by host, not by list position.
	if got := render.PathStrings(contribA.Paths); len(got) != 1 || got[0] != "spec.rules[host=a.example.com]" {
		t.Fatalf("A owned paths = %v; a positional path would make revert delete the wrong rule", got)
	}

	// Contributor B appends a second rule to the same atomic list.
	contribB := mustRender(t, &patchv1alpha1.PatchSpec{
		Value: rawExt(rule("b.example.com", "svc-b")), MergeKeys: mergeKeys,
	}, target)
	resB, err := a.Apply(ctx, Request{
		Target: target, Contribution: contribB, FieldManager: "patch-operator/" + ns + "/b",
	})
	if err != nil {
		t.Fatalf("B apply: %v", err)
	}
	if !resB.Changed {
		t.Error("B's apply should have changed the object")
	}

	hosts := ingressHosts(t, ctx, ns, "shared-ingress")
	if len(hosts) != 2 || !hosts["a.example.com"] || !hosts["b.example.com"] {
		t.Fatalf("both rules should be present on the atomic list, got %v", hosts)
	}

	// A releases. Only A's rule may go.
	if err := a.Revert(ctx, RevertRequest{
		Target: target, OwnedPaths: contribA.Paths, PriorValues: resA.PriorValues,
	}); err != nil {
		t.Fatalf("A revert: %v", err)
	}

	hosts = ingressHosts(t, ctx, ns, "shared-ingress")
	if hosts["a.example.com"] {
		t.Error("A's rule survived its own revert")
	}
	if !hosts["b.example.com"] {
		t.Error("A's revert removed B's rule -- the atomic list was taken wholesale")
	}
	// And the object itself must still be there: revert withdraws fields, it does not delete.
	if got := len(hosts); got != 1 {
		t.Errorf("expected exactly B's rule to remain, got %d rules", got)
	}
}

// Re-applying an unchanged contribution must not turn the contributor's own value into its
// recorded prior. This is the bug the kind e2e job found: on the second apply the live object
// already holds what this contributor wrote, and capturing that as the prior makes revert *restore*
// the contribution -- so the field never goes away and no error is reported anywhere.
func TestCSARepeatedApplyDoesNotCaptureItsOwnValueAsPrior(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ClientSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")
	spec := &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"ours"}}`)}
	base, err := render.DecodeBase(rawExt(`{"apiVersion":"v1","kind":"ConfigMap"}`))
	if err != nil {
		t.Fatal(err)
	}

	first, err := a.Apply(ctx, Request{
		Target: target, Contribution: mustRender(t, spec, target),
		FieldManager: "patch-operator/a", Base: base, AllowCreate: true,
	})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if !first.PriorValuesCaptured {
		t.Error("the first apply must report the capture as done, or the second repeats it")
	}

	// The second apply is the dangerous one, and it is deliberately made to look like a first
	// apply: PriorValuesCaptured is false, exactly as it would be if the tracker's status write
	// had been lost or conflicted.
	second, err := a.Apply(ctx, Request{
		Target: target, Contribution: mustRender(t, spec, target),
		FieldManager: "patch-operator/a", PreviousPaths: first.OwnedPaths,
		PriorValues: nil, PriorValuesCaptured: false,
	})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(second.PriorValues) != 0 {
		t.Fatalf("the contributor's own value was recorded as a prior: %#v", second.PriorValues)
	}

	// Which means revert actually withdraws.
	if err := a.Revert(ctx, RevertRequest{
		Target: target, OwnedPaths: second.OwnedPaths, PriorValues: second.PriorValues,
	}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if _, present := liveData(t, ctx, ns, "shared")["k"]; present {
		t.Error("revert restored the contribution instead of withdrawing it")
	}
}

// A value that genuinely pre-existed must still be captured and restored -- the guard above must
// not be so broad that it breaks the reason priorValues exists.
func TestCSAPreExistingValueIsRestoredOnRevert(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	// Someone else set the key first.
	if err := ssaApply(ctx, t, configMap(ns, "shared", map[string]any{"k": "theirs"}),
		"some-other-controller", false); err != nil {
		t.Fatalf("foreign apply: %v", err)
	}

	a := &ClientSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")
	res, err := a.Apply(ctx, Request{
		Target: target,
		Contribution: mustRender(t,
			&patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"ours"}}`)}, target),
		FieldManager: "patch-operator/a",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.PriorValues["data.k"] != "theirs" {
		t.Fatalf("the pre-existing value was not captured: %#v", res.PriorValues)
	}
	if got := liveData(t, ctx, ns, "shared"); got["k"] != "ours" {
		t.Fatalf("the contribution did not land: %#v", got)
	}

	if err := a.Revert(ctx, RevertRequest{
		Target: target, OwnedPaths: res.OwnedPaths, PriorValues: res.PriorValues,
	}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got := liveData(t, ctx, ns, "shared"); got["k"] != "theirs" {
		t.Errorf("revert deleted a pre-existing value instead of restoring it: %#v", got)
	}
}

// ingressHosts returns the set of rule hosts on an Ingress.
func ingressHosts(t *testing.T, ctx context.Context, ns, name string) map[string]bool {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("networking.k8s.io/v1")
	obj.SetKind("Ingress")
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, obj); err != nil {
		t.Fatalf("get ingress: %v", err)
	}
	rules, _, err := unstructured.NestedSlice(obj.Object, "spec", "rules")
	if err != nil {
		t.Fatalf("reading spec.rules: %v", err)
	}
	out := map[string]bool{}
	for _, r := range rules {
		if m, ok := r.(map[string]any); ok {
			if h, ok := m["host"].(string); ok {
				out[h] = true
			}
		}
	}
	return out
}

// CSA revert must restore a value the contributor overwrote, not delete the field. This is the
// behaviour SSA cannot express and the reason priorValues is tracked at all.
func TestCSARevertRestoresPriorValue(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ClientSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")

	// Something else created the object with a pre-existing value.
	pre := configMap(ns, "shared", map[string]any{"k": "original"})
	if err := k8sClient.Create(ctx, pre); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	contribution := mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"overwritten"}}`)}, target)
	res, err := a.Apply(ctx, Request{
		Target:       target,
		Contribution: contribution,
		FieldManager: "patch-operator/a",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := liveData(t, ctx, ns, "shared"); got["k"] != "overwritten" {
		t.Fatalf("apply did not take effect: %#v", got)
	}
	if res.PriorValues["data.k"] != "original" {
		t.Fatalf("prior value not captured: %#v", res.PriorValues)
	}

	if err := a.Revert(ctx, RevertRequest{
		Target:      target,
		OwnedPaths:  contribution.Paths,
		PriorValues: res.PriorValues,
	}); err != nil {
		t.Fatalf("revert: %v", err)
	}

	got := liveData(t, ctx, ns, "shared")
	if got["k"] != "original" {
		t.Errorf("revert deleted the field instead of restoring it: %#v", got)
	}
}

// A path the contributor added, that did not exist before, must be deleted on revert rather than
// restored to nothing.
func TestCSARevertDeletesFieldItAdded(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ClientSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")

	pre := configMap(ns, "shared", map[string]any{"existing": "keep-me"})
	if err := k8sClient.Create(ctx, pre); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	contribution := mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"added":"v"}}`)}, target)
	res, err := a.Apply(ctx, Request{
		Target: target, Contribution: contribution, FieldManager: "patch-operator/a",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, captured := res.PriorValues["data.added"]; captured {
		t.Errorf("a path that did not exist should have no prior value: %#v", res.PriorValues)
	}

	if err := a.Revert(ctx, RevertRequest{
		Target: target, OwnedPaths: contribution.Paths, PriorValues: res.PriorValues,
	}); err != nil {
		t.Fatalf("revert: %v", err)
	}

	got := liveData(t, ctx, ns, "shared")
	if _, present := got["added"]; present {
		t.Errorf("revert left the added field: %#v", got)
	}
	if got["existing"] != "keep-me" {
		t.Errorf("revert disturbed a field it never owned: %#v", got)
	}
}

// Re-capturing prior values on a second apply would record this contributor's own value and make
// revert a no-op. The first capture must win.
func TestCSAPriorValuesCapturedOnceOnly(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ClientSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")

	if err := k8sClient.Create(ctx, configMap(ns, "shared", map[string]any{"k": "original"})); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	first := mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"v1"}}`)}, target)
	res1, err := a.Apply(ctx, Request{Target: target, Contribution: first, FieldManager: "patch-operator/a"})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}

	second := mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"v2"}}`)}, target)
	res2, err := a.Apply(ctx, Request{
		Target:        target,
		Contribution:  second,
		FieldManager:  "patch-operator/a",
		PreviousPaths: first.Paths,
		PriorValues:   res1.PriorValues,
	})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}

	if res2.PriorValues["data.k"] != "original" {
		t.Errorf("the second apply overwrote the prior value with its own: %#v", res2.PriorValues)
	}

	if err := a.Revert(ctx, RevertRequest{
		Target: target, OwnedPaths: second.Paths, PriorValues: res2.PriorValues,
	}); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got := liveData(t, ctx, ns, "shared"); got["k"] != "original" {
		t.Errorf("revert after two applies did not restore the original: %#v", got)
	}
}

// A shrinking contribution must withdraw the paths it no longer claims, or they become orphans
// nobody owns and nobody can find.
func TestCSAShrinkingContributionWithdrawsDroppedPaths(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ClientSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")

	wide := mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"keep":"v","drop":"v"}}`)}, target)
	res1, err := a.Apply(ctx, Request{
		Target: target, Contribution: wide, FieldManager: "patch-operator/a", AllowCreate: true,
	})
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}

	narrow := mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"keep":"v"}}`)}, target)
	if _, err := a.Apply(ctx, Request{
		Target:        target,
		Contribution:  narrow,
		FieldManager:  "patch-operator/a",
		PreviousPaths: wide.Paths,
		PriorValues:   res1.PriorValues,
	}); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	got := liveData(t, ctx, ns, "shared")
	if _, present := got["drop"]; present {
		t.Errorf("a dropped path was left behind: %#v", got)
	}
	if got["keep"] != "v" {
		t.Errorf("the retained path was lost: %#v", got)
	}
}

// An unchanged contribution must report Changed=false and issue no write. Writing unconditionally
// would make the operator's own watch events a hot loop.
func TestCSAUnchangedApplyIsNoOp(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ClientSideApplier{Client: k8sClient}
	target := cmTarget(ns, "shared")
	contribution := mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"v"}}`)}, target)

	if _, err := a.Apply(ctx, Request{
		Target: target, Contribution: contribution, FieldManager: "patch-operator/a", AllowCreate: true,
	}); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	before := getConfigMap(ctx, t, ns, "shared").GetResourceVersion()

	res, err := a.Apply(ctx, Request{
		Target: target, Contribution: contribution, FieldManager: "patch-operator/a",
		PreviousPaths: contribution.Paths,
	})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.Changed {
		t.Error("an unchanged contribution reported Changed=true")
	}
	if after := getConfigMap(ctx, t, ns, "shared").GetResourceVersion(); after != before {
		t.Errorf("an unchanged apply still wrote: resourceVersion %s -> %s", before, after)
	}
}

func TestCSARefusesToCreateWhenNotAllowed(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ClientSideApplier{Client: k8sClient}
	target := cmTarget(ns, "absent")

	_, err := a.Apply(ctx, Request{
		Target:       target,
		Contribution: mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"k":"v"}}`)}, target),
		FieldManager: "patch-operator/a",
		AllowCreate:  false,
	})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestCSARevertOnMissingTargetSucceeds(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	a := &ClientSideApplier{Client: k8sClient}
	if err := a.Revert(ctx, RevertRequest{Target: cmTarget(ns, "never-existed")}); err != nil {
		t.Errorf("revert against a missing target should succeed, got %v", err)
	}
}

// A creating contributor's base seeds the object, and the contribution overlays it.
func TestApplyWithBaseSeedsTheTarget(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	for name, applier := range map[string]Applier{
		"ssa": &ServerSideApplier{Client: k8sClient},
		"csa": &ClientSideApplier{Client: k8sClient},
	} {
		t.Run(name, func(t *testing.T) {
			target := cmTarget(ns, "seeded-"+name)
			base, err := render.DecodeBase(rawExt(`{"apiVersion":"v1","kind":"ConfigMap","data":{"from-base":"b"}}`))
			if err != nil {
				t.Fatal(err)
			}

			res, err := applier.Apply(ctx, Request{
				Target:       target,
				Contribution: mustRender(t, &patchv1alpha1.PatchSpec{Value: rawExt(`{"data":{"from-patch":"p"}}`)}, target),
				FieldManager: "patch-operator/a",
				Base:         base,
				AllowCreate:  true,
			})
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !res.Created {
				t.Error("expected Created")
			}
			got := liveData(t, ctx, ns, "seeded-"+name)
			if got["from-base"] != "b" || got["from-patch"] != "p" {
				t.Errorf("base and contribution should both land: %#v", got)
			}
		})
	}
}
