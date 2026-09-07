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

package render

import (
	"encoding/json"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

var ingressTarget = Target{
	APIVersion: "networking.k8s.io/v1",
	Kind:       "Ingress",
	Name:       "shared-ingress",
	Namespace:  "team-a",
}

func raw(s string) *runtime.RawExtension { return &runtime.RawExtension{Raw: []byte(s)} }

func TestRenderStrategicMerge(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{
		Type:  patchv1alpha1.PatchTypeStrategicMerge,
		Value: raw(`{"spec": {"ingressClassName": "nginx", "replicas": 3}}`),
	}

	c, err := Render(spec, ingressTarget)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	if c.Object["apiVersion"] != "networking.k8s.io/v1" || c.Object["kind"] != "Ingress" {
		t.Errorf("identity not stamped: %#v", c.Object)
	}
	meta := c.Object["metadata"].(map[string]any)
	if meta["name"] != "shared-ingress" || meta["namespace"] != "team-a" {
		t.Errorf("metadata wrong: %#v", meta)
	}

	want := []string{"spec.ingressClassName", "spec.replicas"}
	if got := PathStrings(c.Paths); !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
	if c.Hash == "" {
		t.Error("hash not set")
	}
}

// A cluster-scoped target has no namespace, and metadata.namespace must be absent rather than "".
func TestRenderClusterScopedTargetOmitsNamespace(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{Value: raw(`{"rules": [{"verbs": ["get"]}]}`)}
	c, err := Render(spec, Target{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "shared"})
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	meta := c.Object["metadata"].(map[string]any)
	if _, present := meta["namespace"]; present {
		t.Errorf("namespace should be absent for a cluster-scoped target: %#v", meta)
	}
}

// mergeKeys must turn list positions into key matches. Without this, an owned path would mean
// "whatever is first", and a revert after a reorder would delete another contributor's element.
func TestRenderMergeKeysProduceKeyedPaths(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{
		Value: raw(`{"spec": {"rules": [
		  {"host": "a.example.com", "http": {"paths": [{"path": "/"}]}}
		]}}`),
		MergeKeys: []patchv1alpha1.MergeKey{{Path: "spec.rules", Key: "host"}},
	}

	c, err := Render(spec, ingressTarget)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	want := []string{"spec.rules[host=a.example.com]"}
	if got := PathStrings(c.Paths); !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

// Without a mergeKey the same patch yields index-based paths, descending into the element.
func TestRenderWithoutMergeKeysUsesIndexPaths(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{
		Value: raw(`{"spec": {"rules": [{"host": "a.example.com"}]}}`),
	}
	c, err := Render(spec, ingressTarget)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	want := []string{"spec.rules[0].host"}
	if got := PathStrings(c.Paths); !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

// Two contributors with distinct merge keys must produce non-overlapping owned paths. This is the
// property that makes them not-a-conflict, so it is asserted rather than assumed.
func TestRenderDistinctMergeKeysDoNotOverlap(t *testing.T) {
	mk := []patchv1alpha1.MergeKey{{Path: "spec.rules", Key: "host"}}

	a, err := Render(&patchv1alpha1.PatchSpec{
		Value:     raw(`{"spec": {"rules": [{"host": "a.example.com", "port": 80}]}}`),
		MergeKeys: mk,
	}, ingressTarget)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(&patchv1alpha1.PatchSpec{
		Value:     raw(`{"spec": {"rules": [{"host": "b.example.com", "port": 80}]}}`),
		MergeKeys: mk,
	}, ingressTarget)
	if err != nil {
		t.Fatal(err)
	}

	for _, pa := range a.Paths {
		for _, pb := range b.Paths {
			if pa.Equal(pb) {
				t.Errorf("paths overlap on %q, so distinct hosts would be treated as a conflict", pa)
			}
		}
	}
}

func TestRenderMerge7386(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{
		Type:  patchv1alpha1.PatchTypeMerge,
		Value: raw(`{"data": {"key-a": "value-a"}}`),
	}
	c, err := Render(spec, Target{APIVersion: "v1", Kind: "ConfigMap", Name: "shared", Namespace: "team-a"})
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	if got := PathStrings(c.Paths); !reflect.DeepEqual(got, []string{"data.key-a"}) {
		t.Errorf("paths = %v", got)
	}
}

func TestRenderJSON6902(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{
		Type: patchv1alpha1.PatchTypeJSON6902,
		Ops:  raw(`[{"op": "add", "path": "/spec/rules/-", "value": {"host": "a.example.com"}}]`),
	}

	c, err := Render(spec, ingressTarget)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	rules, ok := Get(c.Object, MustParse("spec.rules"))
	if !ok {
		t.Fatalf("ops did not produce spec.rules: %#v", c.Object)
	}
	list := rules.([]any)
	if len(list) != 1 || list[0].(map[string]any)["host"] != "a.example.com" {
		t.Errorf("unexpected rendered rules: %#v", list)
	}
	if len(c.Paths) != 1 || c.Paths[0].String() != "spec.rules[0]" {
		t.Errorf("paths = %v", PathStrings(c.Paths))
	}
}

func TestRenderJSON6902NestedAdd(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{
		Type: patchv1alpha1.PatchTypeJSON6902,
		Ops:  raw(`[{"op": "add", "path": "/metadata/annotations/team", "value": "a"}]`),
	}
	c, err := Render(spec, ingressTarget)
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}
	got, ok := Get(c.Object, MustParse("metadata.annotations.team"))
	if !ok || got != "a" {
		t.Errorf("annotation not rendered: %#v exists=%v", got, ok)
	}
}

func TestRenderErrors(t *testing.T) {
	tests := map[string]*patchv1alpha1.PatchSpec{
		"nil value":       {Type: patchv1alpha1.PatchTypeStrategicMerge},
		"empty value":     {Type: patchv1alpha1.PatchTypeStrategicMerge, Value: raw(`{}`)},
		"bad json":        {Type: patchv1alpha1.PatchTypeStrategicMerge, Value: raw(`{`)},
		"unknown type":    {Type: patchv1alpha1.PatchType("Nope"), Value: raw(`{"a": 1}`)},
		"ops missing":     {Type: patchv1alpha1.PatchTypeJSON6902},
		"ops empty":       {Type: patchv1alpha1.PatchTypeJSON6902, Ops: raw(`[]`)},
		"ops whole doc":   {Type: patchv1alpha1.PatchTypeJSON6902, Ops: raw(`[{"op":"add","path":"/","value":1}]`)},
		"ops bad pointer": {Type: patchv1alpha1.PatchTypeJSON6902, Ops: raw(`[{"op":"add","path":"spec","value":1}]`)},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Render(spec, ingressTarget); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestRenderNilSpec(t *testing.T) {
	if _, err := Render(nil, ingressTarget); err == nil {
		t.Error("expected an error for a nil spec")
	}
}

// The hash must be stable across processes, since "hash unchanged, skip the apply" depends on it.
// Map iteration order differing between renders must not change the hash.
func TestHashIsStableAcrossRenders(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{
		Value: raw(`{"spec": {"a": 1, "b": 2, "c": {"d": 4, "e": 5}}}`),
	}
	first, err := Render(spec, ingressTarget)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := Render(spec, ingressTarget)
		if err != nil {
			t.Fatal(err)
		}
		if again.Hash != first.Hash {
			t.Fatalf("hash changed between renders: %s vs %s", first.Hash, again.Hash)
		}
	}
}

func TestHashDistinguishesContent(t *testing.T) {
	a, _ := Render(&patchv1alpha1.PatchSpec{Value: raw(`{"spec": {"replicas": 1}}`)}, ingressTarget)
	b, _ := Render(&patchv1alpha1.PatchSpec{Value: raw(`{"spec": {"replicas": 2}}`)}, ingressTarget)
	if a.Hash == b.Hash {
		t.Error("different contributions hashed identically")
	}
}

// The same patch against a different target must hash differently, or a Selector-mode contributor
// would skip applying to every target after the first.
func TestHashDistinguishesTarget(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{Value: raw(`{"spec": {"replicas": 1}}`)}
	a, _ := Render(spec, ingressTarget)
	b, _ := Render(spec, Target{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Name: "other", Namespace: "team-a"})
	if a.Hash == b.Hash {
		t.Error("the same patch against different targets hashed identically")
	}
}

func TestIdentityClaimsNothing(t *testing.T) {
	obj := Identity(ingressTarget)

	if len(obj) != 3 {
		t.Errorf("identity should carry only apiVersion, kind and metadata: %#v", obj)
	}
	meta := obj["metadata"].(map[string]any)
	if len(meta) != 2 {
		t.Errorf("identity metadata should carry only name and namespace: %#v", meta)
	}
	// Nothing under spec: this is what makes the API server release every owned field.
	if _, ok := obj["spec"]; ok {
		t.Error("identity must not carry a spec")
	}
}

func TestIdentityClusterScoped(t *testing.T) {
	obj := Identity(Target{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: "shared"})
	meta := obj["metadata"].(map[string]any)
	if _, present := meta["namespace"]; present {
		t.Errorf("cluster-scoped identity must not carry a namespace: %#v", meta)
	}
}

// Render must not alias the caller's decoded patch, or a merge into a live object would corrupt
// the informer cache.
func TestRenderDoesNotAliasInput(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{Value: raw(`{"spec": {"nested": {"v": 1}}}`)}

	c, err := Render(spec, ingressTarget)
	if err != nil {
		t.Fatal(err)
	}
	if err := Set(c.Object, MustParse("spec.nested.v"), 999); err != nil {
		t.Fatal(err)
	}

	again, err := Render(spec, ingressTarget)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(again.Object, MustParse("spec.nested.v")); got != float64(1) {
		t.Errorf("mutating a rendered object leaked back into the spec: got %#v", got)
	}
}

func TestParsePathsRoundTrip(t *testing.T) {
	in := []string{"spec.replicas", "spec.rules[host=a.example.com]"}
	paths, err := ParsePaths(in)
	if err != nil {
		t.Fatalf("ParsePaths failed: %v", err)
	}
	if got := PathStrings(paths); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip = %v, want %v", got, in)
	}
	if _, err := ParsePaths([]string{"spec["}); err == nil {
		t.Error("ParsePaths should reject a malformed stored path")
	}
}

func TestDecodeBase(t *testing.T) {
	got, err := DecodeBase(raw(`{"apiVersion": "v1", "kind": "ConfigMap"}`))
	if err != nil {
		t.Fatalf("DecodeBase failed: %v", err)
	}
	if got["kind"] != "ConfigMap" {
		t.Errorf("got %#v", got)
	}
	if _, err := DecodeBase(nil); err == nil {
		t.Error("DecodeBase(nil) should fail")
	}
}

func TestDeepCopyMapIsDeep(t *testing.T) {
	src := mustJSON(t, `{"a": {"b": [{"c": 1}]}}`)
	dst := DeepCopyMap(src)

	if err := Set(dst, MustParse("a.b[0].c"), 2); err != nil {
		t.Fatal(err)
	}
	if got, _ := Get(src, MustParse("a.b[0].c")); got != float64(1) {
		t.Errorf("copy was shallow: source changed to %#v", got)
	}
}

func TestHashObjectRejectsUnmarshalable(t *testing.T) {
	if _, err := HashObject(map[string]any{"fn": func() {}}); err == nil {
		t.Error("expected an error hashing an unmarshalable value")
	}
}

func TestLeafPathsSkipsIdentityFieldsAtRoot(t *testing.T) {
	obj := mustJSON(t, `{"apiVersion": "v1", "kind": "ConfigMap", "data": {"k": "v"}}`)
	got := PathStrings(LeafPaths(obj, nil))

	for _, p := range got {
		if p == "apiVersion" || p == "kind" {
			t.Errorf("identity field %q claimed as a contribution", p)
		}
	}
	if want := []string{"data.k"}; !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

func TestLeafPathsEmptyContainersAreClaims(t *testing.T) {
	// An explicitly empty map or list is a real claim: "this field should be empty".
	obj := mustJSON(t, `{"spec": {"selector": {}, "rules": []}}`)
	got := PathStrings(LeafPaths(obj, nil))
	want := []string{"spec.rules", "spec.selector"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

func TestContributionObjectIsValidJSON(t *testing.T) {
	spec := &patchv1alpha1.PatchSpec{Value: raw(`{"spec": {"rules": [{"host": "a"}]}}`)}
	c, err := Render(spec, ingressTarget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(c.Object); err != nil {
		t.Errorf("rendered object is not marshalable: %v", err)
	}
}
