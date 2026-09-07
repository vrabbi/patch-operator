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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These tests pin down the server-side apply semantics the whole revert design rests on. They are
// deliberately written against the raw client rather than this package's Applier: if the API
// server does not behave as DESIGN.md 5.1 claims, the design is wrong and no amount of applier
// code fixes it. Everything downstream assumes these pass.

func configMap(ns, name string, data map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetUnstructuredContent(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	})
	if data != nil {
		_ = unstructured.SetNestedMap(obj.Object, data, "data")
	}
	return obj
}

func ssaApply(ctx context.Context, t *testing.T, obj *unstructured.Unstructured, manager string, force bool) error {
	t.Helper()
	opts := []client.PatchOption{client.FieldOwner(manager)}
	if force {
		opts = append(opts, client.ForceOwnership)
	}
	return k8sClient.Patch(ctx, obj, client.Apply, opts...)
}

func getConfigMap(ctx context.Context, t *testing.T, ns, name string) *unstructured.Unstructured {
	t.Helper()
	out := configMap(ns, name, nil)
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, out); err != nil {
		t.Fatalf("getting %s/%s: %v", ns, name, err)
	}
	return out
}

func dataOf(t *testing.T, obj *unstructured.Unstructured) map[string]string {
	t.Helper()
	m, _, err := unstructured.NestedStringMap(obj.Object, "data")
	if err != nil {
		t.Fatalf("reading data: %v", err)
	}
	return m
}

func managers(obj *unstructured.Unstructured) []string {
	var out []string
	for _, e := range obj.GetManagedFields() {
		out = append(out, e.Manager)
	}
	return out
}

func hasManager(obj *unstructured.Unstructured, manager string) bool {
	for _, e := range obj.GetManagedFields() {
		if e.Manager == manager {
			return true
		}
	}
	return false
}

// TestSSAOwnershipReleaseOnIdentityApply is the single most important test in this repository.
//
// DESIGN.md 5.1 claims that re-applying an object carrying only apiVersion/kind/metadata.name
// under a field manager releases every field that manager owned, deleting the ones no other
// manager owns. Client-side revert exists only as a fallback; server-side revert is supposed to be
// free. If this does not hold, the revert design has to change.
func TestSSAOwnershipReleaseOnIdentityApply(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	const (
		mgrA = "patch-operator/team-a/contributor-a"
		mgrB = "patch-operator/team-a/contributor-b"
	)

	// Two contributors claim disjoint keys of one shared ConfigMap.
	if err := ssaApply(ctx, t, configMap(ns, "shared", map[string]any{"key-a": "value-a"}), mgrA, false); err != nil {
		t.Fatalf("contributor A apply: %v", err)
	}
	if err := ssaApply(ctx, t, configMap(ns, "shared", map[string]any{"key-b": "value-b"}), mgrB, false); err != nil {
		t.Fatalf("contributor B apply: %v", err)
	}

	live := getConfigMap(ctx, t, ns, "shared")
	if got := dataOf(t, live); got["key-a"] != "value-a" || got["key-b"] != "value-b" {
		t.Fatalf("both contributions should be present, got %#v", got)
	}
	if !hasManager(live, mgrA) || !hasManager(live, mgrB) {
		t.Fatalf("expected both field managers, got %v", managers(live))
	}

	// Now revert contributor A: apply identity only, under A's field manager.
	identity := configMap(ns, "shared", nil)
	if err := ssaApply(ctx, t, identity, mgrA, false); err != nil {
		t.Fatalf("identity apply under %s: %v", mgrA, err)
	}

	live = getConfigMap(ctx, t, ns, "shared")
	got := dataOf(t, live)

	if _, present := got["key-a"]; present {
		t.Errorf("A's field survived the identity apply: %#v", got)
	}
	if got["key-b"] != "value-b" {
		t.Errorf("B's field was disturbed by A's revert: %#v", got)
	}
	if hasManager(live, mgrA) {
		t.Errorf("A's managedFields entry should be gone, got %v", managers(live))
	}
	if !hasManager(live, mgrB) {
		t.Errorf("B's managedFields entry should remain, got %v", managers(live))
	}
}

// A field two managers both own must survive one of them releasing it. This is what makes
// redundant contributions (two XRs asking for the same ingress class) safe.
func TestSSASharedFieldSurvivesOneRelease(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	const mgrA, mgrB = "patch-operator/a", "patch-operator/b"
	same := map[string]any{"shared-key": "same-value"}

	if err := ssaApply(ctx, t, configMap(ns, "shared", same), mgrA, false); err != nil {
		t.Fatalf("A apply: %v", err)
	}
	// Identical value, so this is co-ownership rather than a conflict.
	if err := ssaApply(ctx, t, configMap(ns, "shared", same), mgrB, false); err != nil {
		t.Fatalf("B apply of an identical value should not conflict: %v", err)
	}

	if err := ssaApply(ctx, t, configMap(ns, "shared", nil), mgrA, false); err != nil {
		t.Fatalf("A identity apply: %v", err)
	}

	live := getConfigMap(ctx, t, ns, "shared")
	if got := dataOf(t, live); got["shared-key"] != "same-value" {
		t.Errorf("a co-owned field was deleted when only one owner released it: %#v", got)
	}
}

// A conflicting claim must be *reported*, not silently applied. This is what lets conflictPolicy:
// Fail exist at all, and is the concrete improvement over unconditional forcing.
func TestSSAConflictIsReported(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	const mgrA, mgrB = "patch-operator/a", "patch-operator/b"

	if err := ssaApply(ctx, t, configMap(ns, "shared", map[string]any{"k": "from-a"}), mgrA, false); err != nil {
		t.Fatalf("A apply: %v", err)
	}

	err := ssaApply(ctx, t, configMap(ns, "shared", map[string]any{"k": "from-b"}), mgrB, false)
	if err == nil {
		t.Fatal("B's conflicting claim was accepted silently; conflictPolicy: Fail would be unimplementable")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected a conflict error, got %T: %v", err, err)
	}
	// The error must name the current owner, so the Conflict condition can say who is fighting.
	if !IsFieldConflict(err) {
		t.Errorf("IsFieldConflict did not recognise %v", err)
	}
	if owners := ConflictingManagers(err); len(owners) == 0 {
		t.Errorf("could not extract the conflicting manager from %v", err)
	} else if owners[0] != mgrA {
		t.Errorf("conflicting manager = %q, want %q", owners[0], mgrA)
	}

	live := getConfigMap(ctx, t, ns, "shared")
	if got := dataOf(t, live); got["k"] != "from-a" {
		t.Errorf("the rejected apply still changed the value: %#v", got)
	}
}

// Forcing must take ownership and change the value. conflictPolicy: Priority relies on this, but
// only ever against another patch-operator manager.
func TestSSAForceTakesOwnership(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	const mgrA, mgrB = "patch-operator/a", "patch-operator/b"

	if err := ssaApply(ctx, t, configMap(ns, "shared", map[string]any{"k": "from-a"}), mgrA, false); err != nil {
		t.Fatalf("A apply: %v", err)
	}
	if err := ssaApply(ctx, t, configMap(ns, "shared", map[string]any{"k": "from-b"}), mgrB, true); err != nil {
		t.Fatalf("B forced apply: %v", err)
	}

	live := getConfigMap(ctx, t, ns, "shared")
	if got := dataOf(t, live); got["k"] != "from-b" {
		t.Errorf("force did not take the field: %#v", got)
	}
	if hasManager(live, mgrA) {
		// The API server drops the loser's claim on that field. With only one field in play, its
		// whole entry goes.
		t.Logf("A's entry remains after being forced out: %v", managers(live))
	}
}

// An SSA apply against a non-existent object creates it. This is what makes create-on-missing a
// single call rather than a get-then-create race.
func TestSSAApplyCreates(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	if err := ssaApply(ctx, t, configMap(ns, "created", map[string]any{"k": "v"}), "patch-operator/a", false); err != nil {
		t.Fatalf("apply-create: %v", err)
	}
	live := getConfigMap(ctx, t, ns, "created")
	if got := dataOf(t, live); got["k"] != "v" {
		t.Errorf("got %#v", got)
	}
}

// Two contributors racing to create the same object: SSA makes this benign, since apply is
// idempotent and neither call is a bare Create that could fail with AlreadyExists.
func TestSSAConcurrentCreateIsBenign(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := testNamespace(t, ctx)

	errs := make(chan error, 2)
	for _, mgr := range []string{"patch-operator/a", "patch-operator/b"} {
		go func(m string) {
			key := "key-" + m[len(m)-1:]
			errs <- ssaApply(ctx, t, configMap(ns, "raced", map[string]any{key: "v"}), m, false)
		}(mgr)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent apply failed: %v", err)
		}
	}

	live := getConfigMap(ctx, t, ns, "raced")
	if got := dataOf(t, live); len(got) != 2 {
		t.Errorf("expected both contributions after the race, got %#v", got)
	}
}
