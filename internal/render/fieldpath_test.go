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
)

func TestParseRoundTrip(t *testing.T) {
	for _, in := range []string{
		"spec",
		"spec.replicas",
		"spec.template.spec.containers",
		"spec.rules[0]",
		"spec.rules[host=a.example.com]",
		"spec.rules[host=a.example.com].http.paths[0].backend",
		"data[key=x]",
		"spec.rules[host=a.b.c].http",
		// A value containing '=' must survive: only the first '=' separates key from value.
		"spec.env[name=FOO=BAR]",
		// An empty value is legal: a contributor may key on a field that is set to "".
		"spec.rules[host=]",
	} {
		t.Run(in, func(t *testing.T) {
			p, err := Parse(in)
			if err != nil {
				t.Fatalf("Parse(%q) failed: %v", in, err)
			}
			if got := p.String(); got != in {
				t.Errorf("round trip: Parse(%q).String() = %q", in, got)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{
		"",
		"spec.",
		"spec[",
		"spec[]",
		"spec[=x]",
		"spec[notanindex]",
		"spec[-1]",
	} {
		t.Run(in, func(t *testing.T) {
			if _, err := Parse(in); err == nil {
				t.Errorf("Parse(%q) should have failed", in)
			}
		})
	}
}

func TestGet(t *testing.T) {
	obj := mustJSON(t, `{
	  "spec": {
	    "replicas": 3,
	    "rules": [
	      {"host": "a.example.com", "http": {"paths": [{"path": "/a"}]}},
	      {"host": "b.example.com", "http": {"paths": [{"path": "/b"}]}}
	    ]
	  }
	}`)

	tests := []struct {
		path   string
		want   any
		exists bool
	}{
		{"spec.replicas", float64(3), true},
		{"spec.rules[host=b.example.com].http.paths[0].path", "/b", true},
		{"spec.rules[1].host", "b.example.com", true},
		{"spec.missing", nil, false},
		{"spec.rules[host=nope]", nil, false},
		{"spec.rules[9]", nil, false},
		// Descending through a scalar must not panic.
		{"spec.replicas.deeper", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := Get(obj, MustParse(tc.path))
			if ok != tc.exists {
				t.Fatalf("exists = %v, want %v", ok, tc.exists)
			}
			if tc.exists && !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestSetCreatesIntermediates(t *testing.T) {
	obj := map[string]any{}
	if err := Set(obj, MustParse("spec.template.spec.replicas"), 3); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	got, ok := Get(obj, MustParse("spec.template.spec.replicas"))
	if !ok || got != 3 {
		t.Fatalf("got %#v exists=%v", got, ok)
	}
}

// A key-match segment against a list that has no such element must append a new element carrying
// the key. That is how a contributor adds its own entry to a shared list.
func TestSetAppendsKeyedElement(t *testing.T) {
	obj := mustJSON(t, `{"spec": {"rules": [{"host": "a.example.com"}]}}`)

	if err := Set(obj, MustParse("spec.rules[host=b.example.com].port"), 8080); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	rules, _ := Get(obj, MustParse("spec.rules"))
	list, ok := rules.([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("expected 2 rules, got %#v", rules)
	}
	if got, ok := Get(obj, MustParse("spec.rules[host=a.example.com]")); !ok {
		t.Errorf("the pre-existing element was lost: %#v", got)
	}
	port, ok := Get(obj, MustParse("spec.rules[host=b.example.com].port"))
	if !ok || port != 8080 {
		t.Errorf("appended element wrong: port=%#v exists=%v", port, ok)
	}
}

func TestSetIntoEmptyListPath(t *testing.T) {
	obj := map[string]any{}
	if err := Set(obj, MustParse("spec.rules[host=a].port"), 80); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if got, ok := Get(obj, MustParse("spec.rules[host=a].port")); !ok || got != 80 {
		t.Fatalf("got %#v exists=%v", got, ok)
	}
}

func TestSetRejectsListFirstSegment(t *testing.T) {
	if err := Set(map[string]any{}, Path{IndexSegment(0)}, 1); err == nil {
		t.Error("a path starting with a list segment should be rejected")
	}
}

// Deleting a keyed list element must remove exactly that element. This is the property that makes
// revert surgical instead of list-wide, so it is tested directly.
func TestDeleteKeyedElementLeavesSiblings(t *testing.T) {
	obj := mustJSON(t, `{"spec": {"rules": [
	  {"host": "a.example.com"},
	  {"host": "b.example.com"},
	  {"host": "c.example.com"}
	]}}`)

	if !Delete(obj, MustParse("spec.rules[host=b.example.com]")) {
		t.Fatal("Delete reported nothing removed")
	}

	rules, _ := Get(obj, MustParse("spec.rules"))
	list := rules.([]any)
	if len(list) != 2 {
		t.Fatalf("expected 2 rules left, got %d: %#v", len(list), list)
	}
	for _, want := range []string{"a.example.com", "c.example.com"} {
		if _, ok := Get(obj, MustParse("spec.rules[host="+want+"]")); !ok {
			t.Errorf("sibling %q was removed", want)
		}
	}
	if _, ok := Get(obj, MustParse("spec.rules[host=b.example.com]")); ok {
		t.Error("target element still present")
	}
}

func TestDeleteMissingIsNoOp(t *testing.T) {
	obj := mustJSON(t, `{"spec": {"replicas": 1}}`)
	if Delete(obj, MustParse("spec.missing")) {
		t.Error("Delete of a missing path should report false")
	}
	if Delete(obj, MustParse("spec.rules[host=x]")) {
		t.Error("Delete against a missing list should report false")
	}
	if got, ok := Get(obj, MustParse("spec.replicas")); !ok || got != float64(1) {
		t.Error("Delete of a missing path mutated the object")
	}
}

// A revert should not leave `spec: {}` litter behind on someone else's object.
func TestDeletePrunesEmptyParents(t *testing.T) {
	obj := mustJSON(t, `{"metadata": {"name": "x"}, "spec": {"template": {"replicas": 1}}}`)

	if !Delete(obj, MustParse("spec.template.replicas")) {
		t.Fatal("Delete reported nothing removed")
	}
	if _, ok := obj["spec"]; ok {
		t.Errorf("empty parents were not pruned: %#v", obj)
	}
	if _, ok := Get(obj, MustParse("metadata.name")); !ok {
		t.Error("pruning removed an unrelated branch")
	}
}

func TestDeleteDoesNotPruneNonEmptyParents(t *testing.T) {
	obj := mustJSON(t, `{"spec": {"template": {"replicas": 1, "keep": true}}}`)
	if !Delete(obj, MustParse("spec.template.replicas")) {
		t.Fatal("Delete reported nothing removed")
	}
	if _, ok := Get(obj, MustParse("spec.template.keep")); !ok {
		t.Error("a sibling field was pruned away")
	}
}

// Set then Delete must return the object to its original state. This is the round trip a revert
// performs, so it gets an explicit test.
func TestSetDeleteRoundTrip(t *testing.T) {
	original := `{"spec": {"rules": [{"host": "a.example.com"}], "replicas": 2}}`
	obj := mustJSON(t, original)

	path := MustParse("spec.rules[host=new.example.com]")
	if err := Set(obj, path, map[string]any{"host": "new.example.com", "port": 80}); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if !Delete(obj, path) {
		t.Fatal("Delete reported nothing removed")
	}

	want := mustJSON(t, original)
	if !reflect.DeepEqual(obj, want) {
		gotJSON, _ := json.Marshal(obj)
		t.Errorf("round trip changed the object:\n got: %s\nwant: %s", gotJSON, original)
	}
}

func TestChildDoesNotAliasPrefix(t *testing.T) {
	base := MustParse("spec.rules")
	a := base.Child(KeySegment("host", "a"))
	b := base.Child(KeySegment("host", "b"))

	if a.String() != "spec.rules[host=a]" || b.String() != "spec.rules[host=b]" {
		t.Fatalf("Child aliased the shared prefix: a=%q b=%q", a, b)
	}
}

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	return out
}
