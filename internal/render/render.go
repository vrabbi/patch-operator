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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apimachinery/pkg/runtime"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

// Contribution is one contributor's rendered claim on a target.
type Contribution struct {
	// Object is the partial object to apply: the contributed fields, plus the identifying
	// apiVersion/kind/metadata needed for a server-side apply.
	Object map[string]any

	// Paths are the leaf field paths this contribution sets, in deterministic order. These become
	// ContributorStatus.OwnedPaths, and are what a client-side revert walks.
	Paths []Path

	// Hash covers Object. An unchanged hash plus an unchanged target resourceVersion lets the
	// tracker skip the apply entirely, which is what stops the operator's own writes becoming a
	// hot loop.
	Hash string
}

// Target identifies the object a contribution is rendered against.
type Target struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

// Render turns a contributor's PatchSpec into a Contribution against target.
//
// For StrategicMerge and Merge the patch value *is* the contribution, so its leaf paths are read
// straight off it, with mergeKeys rewriting list positions into key matches. For JSON6902 the ops
// are applied to a skeleton and the touched paths derived from the op paths, because an op list is
// a set of instructions rather than a shape.
func Render(spec *patchv1alpha1.PatchSpec, target Target) (*Contribution, error) {
	if spec == nil {
		return nil, fmt.Errorf("patch spec is nil")
	}

	patchType := spec.Type
	if patchType == "" {
		patchType = patchv1alpha1.PatchTypeStrategicMerge
	}

	var (
		fields map[string]any
		paths  []Path
		err    error
	)

	switch patchType {
	case patchv1alpha1.PatchTypeStrategicMerge, patchv1alpha1.PatchTypeMerge:
		fields, err = decodeObject(spec.Value, "patch.value")
		if err != nil {
			return nil, err
		}
		if len(fields) == 0 {
			return nil, fmt.Errorf("patch.value must not be empty for type %s", patchType)
		}
		paths = LeafPaths(fields, spec.MergeKeys)

	case patchv1alpha1.PatchTypeJSON6902:
		fields, paths, err = renderJSON6902(spec.Ops)
		if err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("unsupported patch type %q", patchType)
	}

	obj := withIdentity(fields, target)

	h, err := HashObject(obj)
	if err != nil {
		return nil, err
	}

	return &Contribution{Object: obj, Paths: paths, Hash: h}, nil
}

// withIdentity returns a deep copy of fields carrying the target's identity, without which a
// server-side apply has nothing to address.
func withIdentity(fields map[string]any, target Target) map[string]any {
	obj := deepCopyMap(fields)
	obj["apiVersion"] = target.APIVersion
	obj["kind"] = target.Kind

	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["name"] = target.Name
	if target.Namespace != "" {
		meta["namespace"] = target.Namespace
	}
	obj["metadata"] = meta
	return obj
}

// Identity returns the minimal object that addresses target and claims nothing.
//
// Applying this under a contributor's field manager is how a server-side revert works: the manager
// re-applies owning no fields, so the API server releases everything it owned and deletes whatever
// no other manager owns. This is the mechanism the whole revert design rests on.
func Identity(target Target) map[string]any {
	obj := map[string]any{
		"apiVersion": target.APIVersion,
		"kind":       target.Kind,
		"metadata":   map[string]any{"name": target.Name},
	}
	if target.Namespace != "" {
		obj["metadata"].(map[string]any)["namespace"] = target.Namespace
	}
	return obj
}

// LeafPaths returns the leaf paths of a partial object, in deterministic order.
//
// mergeKeys rewrites a list index into a key match: without that, "spec.rules[0]" would mean
// "whatever happens to be first", and a revert would delete a different contributor's element
// after the list order shifted.
func LeafPaths(fields map[string]any, mergeKeys []patchv1alpha1.MergeKey) []Path {
	keyByPath := make(map[string]string, len(mergeKeys))
	for _, mk := range mergeKeys {
		keyByPath[mk.Path] = mk.Key
	}
	var out []Path
	collectLeaves(fields, nil, keyByPath, &out)
	sortPaths(out)
	return out
}

func collectLeaves(v any, prefix Path, keyByPath map[string]string, out *[]Path) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			if len(prefix) > 0 {
				*out = append(*out, prefix)
			}
			return
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			// Identity fields are not contributed claims; they only address the object.
			if len(prefix) == 0 && (k == "apiVersion" || k == "kind") {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collectLeaves(t[k], prefix.Child(FieldSegment(k)), keyByPath, out)
		}

	case []any:
		if len(t) == 0 {
			if len(prefix) > 0 {
				*out = append(*out, prefix)
			}
			return
		}
		mergeKey := keyByPath[prefix.String()]
		for i, e := range t {
			seg := IndexSegment(i)
			if mergeKey != "" {
				if m, ok := e.(map[string]any); ok {
					if kv, present := m[mergeKey]; present {
						seg = KeySegment(mergeKey, fmt.Sprintf("%v", kv))
					}
				}
			}
			// A keyed element is itself the claim: the contributor owns that entry, not each of
			// its interior fields. Descending further would make a revert delete sub-fields and
			// leave a husk element behind.
			if seg.IsKeyMatch() {
				*out = append(*out, prefix.Child(seg))
				continue
			}
			collectLeaves(e, prefix.Child(seg), keyByPath, out)
		}

	default:
		if len(prefix) > 0 {
			*out = append(*out, prefix)
		}
	}
}

// sortPaths orders paths by their string form, so OwnedPaths is stable across reconciles and a
// status diff means a real change.
func sortPaths(p []Path) {
	sort.Slice(p, func(i, j int) bool { return p[i].String() < p[j].String() })
}

// PathStrings renders paths for status.
func PathStrings(paths []Path) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, p.String())
	}
	return out
}

// ParsePaths parses stored status paths back into Paths.
func ParsePaths(in []string) ([]Path, error) {
	out := make([]Path, 0, len(in))
	for _, s := range in {
		p, err := Parse(s)
		if err != nil {
			return nil, fmt.Errorf("parsing stored path %q: %w", s, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// renderJSON6902 applies the ops to an empty object to derive the resulting shape, and derives the
// touched paths from the op paths.
func renderJSON6902(raw *runtime.RawExtension) (map[string]any, []Path, error) {
	if raw == nil || len(raw.Raw) == 0 {
		return nil, nil, fmt.Errorf("patch.ops must be set for type JSON6902")
	}

	var ops []jsonPatchOp
	if err := json.Unmarshal(raw.Raw, &ops); err != nil {
		return nil, nil, fmt.Errorf("decoding patch.ops: %w", err)
	}
	if len(ops) == 0 {
		return nil, nil, fmt.Errorf("patch.ops must not be empty")
	}

	patch, err := jsonpatch.DecodePatch(raw.Raw)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding patch.ops as RFC 6902: %w", err)
	}

	// "add" with an appending "/-" cannot apply to an empty document, and "remove"/"replace" need
	// an existing value. Seed the skeleton with the containers the ops address so the rendered
	// shape is derivable without the live object.
	skeleton, err := json.Marshal(seedFor(ops))
	if err != nil {
		return nil, nil, err
	}
	applied, err := patch.ApplyIndent(skeleton, "")
	if err != nil {
		return nil, nil, fmt.Errorf("applying patch.ops to a skeleton: %w", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(applied, &fields); err != nil {
		return nil, nil, fmt.Errorf("decoding the result of patch.ops: %w", err)
	}

	var paths []Path
	seen := map[string]bool{}
	for _, op := range ops {
		p, err := pathFromJSONPointer(op.Path)
		if err != nil {
			return nil, nil, err
		}
		if s := p.String(); s != "" && !seen[s] {
			seen[s] = true
			paths = append(paths, p)
		}
	}
	sortPaths(paths)
	return fields, paths, nil
}

// seedFor builds the minimal document the ops can apply to: every map parent along each op path,
// with a list where the path appends.
func seedFor(ops []jsonPatchOp) map[string]any {
	seed := map[string]any{}
	for _, op := range ops {
		tokens := pointerTokens(op.Path)
		if len(tokens) == 0 {
			continue
		}
		cur := seed
		for i := 0; i < len(tokens)-1; i++ {
			tok := tokens[i]
			next, ok := cur[tok].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[tok] = next
			}
			cur = next
		}
		leaf := tokens[len(tokens)-1]
		if leaf == "-" || isNumeric(leaf) {
			// The parent is a list. Replace the map we just created for it.
			if len(tokens) >= 2 {
				parent := seed
				for i := 0; i < len(tokens)-2; i++ {
					parent = parent[tokens[i]].(map[string]any)
				}
				parent[tokens[len(tokens)-2]] = []any{}
			}
		}
	}
	return seed
}

// pathFromJSONPointer converts an RFC 6901 pointer into a Path. A numeric or "-" token becomes an
// index segment; "-" is recorded as index 0 since the appended element's position is only knowable
// against the live list.
func pathFromJSONPointer(ptr string) (Path, error) {
	if ptr == "" || ptr == "/" {
		return nil, fmt.Errorf("op path %q addresses the whole document, which a contribution may not claim", ptr)
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, fmt.Errorf("op path %q must start with '/'", ptr)
	}
	var out Path
	for _, tok := range pointerTokens(ptr) {
		switch {
		case tok == "-":
			out = append(out, IndexSegment(0))
		case isNumeric(tok):
			n := 0
			for _, c := range tok {
				n = n*10 + int(c-'0')
			}
			out = append(out, IndexSegment(n))
		default:
			out = append(out, FieldSegment(tok))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no segments in op path %q", ptr)
	}
	return out, nil
}

func pointerTokens(ptr string) []string {
	ptr = strings.TrimPrefix(ptr, "/")
	if ptr == "" {
		return nil
	}
	parts := strings.Split(ptr, "/")
	for i, p := range parts {
		// RFC 6901 escaping: ~1 is '/', ~0 is '~'. Order matters.
		p = strings.ReplaceAll(p, "~1", "/")
		parts[i] = strings.ReplaceAll(p, "~0", "~")
	}
	return parts
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type jsonPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	From  string          `json:"from,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

// HashObject returns a stable hash of an unstructured object.
//
// json.Marshal sorts map keys, so the encoding — and therefore the hash — is deterministic across
// processes and restarts. That determinism is what makes "hash unchanged, skip the apply" safe.
func HashObject(obj any) (string, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("hashing object: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// decodeObject decodes a RawExtension into a map.
func decodeObject(raw *runtime.RawExtension, field string) (map[string]any, error) {
	if raw == nil || len(raw.Raw) == 0 {
		return nil, fmt.Errorf("%s must be set", field)
	}
	var out map[string]any
	if err := json.Unmarshal(raw.Raw, &out); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", field, err)
	}
	return out, nil
}

// DecodeBase decodes a contributor's base manifest.
func DecodeBase(raw *runtime.RawExtension) (map[string]any, error) {
	return decodeObject(raw, "spec.base")
}

func deepCopyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopyMap(t)
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = deepCopyValue(t[i])
		}
		return out
	default:
		return v
	}
}

// DeepCopyMap exposes the deep copy used internally, so callers merging into a live object never
// alias the informer cache.
func DeepCopyMap(in map[string]any) map[string]any { return deepCopyMap(in) }

// DeepCopyValue exposes the deep copy for a single value, so a caller merging into a live object
// never aliases the informer cache.
func DeepCopyValue(v any) any { return deepCopyValue(v) }
