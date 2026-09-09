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

// Package render turns a contributor's declared patch into a concrete contribution: the paths it
// owns, the values at those paths, and a hash that lets an unchanged contribution skip an apply.
//
// The fieldpath type here is load-bearing. Client-side revert restores a contributor's prior values
// path by path, so a path that cannot address a single list element by its merge key cannot be
// reverted precisely — it would take the whole list with it.
package render

import (
	"fmt"
	"strconv"
	"strings"
)

// Segment is one step in a field path.
//
// A segment is a map key, a numeric list index, or a list *key match* — the last being what makes
// a per-element revert possible on a list the target's schema declares as atomic.
type Segment struct {
	// Field is a map key, e.g. "spec". Empty when this segment indexes a list.
	Field string
	// Index is a list position, used when Field is empty and Key is empty. -1 when unset.
	Index int
	// Key and Value select a list element by one of its fields, e.g. host=a.example.com.
	Key   string
	Value string
}

// IsField reports whether this segment is a map key.
func (s Segment) IsField() bool { return s.Field != "" }

// IsIndex reports whether this segment is a numeric list index.
func (s Segment) IsIndex() bool { return s.Field == "" && s.Key == "" && s.Index >= 0 }

// IsKeyMatch reports whether this segment selects a list element by key.
func (s Segment) IsKeyMatch() bool { return s.Key != "" }

// Path is a parsed field path.
type Path []Segment

// String renders a path in the notation used in status: "spec.rules[host=a.example.com].http".
// Round-trips through Parse.
func (p Path) String() string {
	var b strings.Builder
	for _, s := range p {
		switch {
		case s.IsField():
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s.Field)
		case s.IsKeyMatch():
			b.WriteByte('[')
			b.WriteString(s.Key)
			b.WriteByte('=')
			b.WriteString(s.Value)
			b.WriteByte(']')
		case s.IsIndex():
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(s.Index))
			b.WriteByte(']')
		}
	}
	return b.String()
}

// Equal reports whether two paths address the same location.
func (p Path) Equal(other Path) bool {
	if len(p) != len(other) {
		return false
	}
	for i := range p {
		if p[i] != other[i] {
			return false
		}
	}
	return true
}

// Child returns a copy of p extended by one segment. It copies rather than appending in place,
// because callers walk a tree and share the prefix — appending would let a sibling overwrite it.
func (p Path) Child(s Segment) Path {
	out := make(Path, len(p), len(p)+1)
	copy(out, p)
	return append(out, s)
}

// FieldSegment builds a map-key segment.
func FieldSegment(name string) Segment { return Segment{Field: name, Index: -1} }

// IndexSegment builds a numeric list-index segment.
func IndexSegment(i int) Segment { return Segment{Index: i} }

// KeySegment builds a list key-match segment.
func KeySegment(key, value string) Segment { return Segment{Index: -1, Key: key, Value: value} }

// Parse parses the string form of a path. It is the inverse of Path.String.
func Parse(s string) (Path, error) {
	if s == "" {
		return nil, fmt.Errorf("empty path")
	}
	var out Path
	i := 0
	for i < len(s) {
		switch s[i] {
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("unterminated '[' at offset %d in %q", i, s)
			}
			inner := s[i+1 : i+end]
			if inner == "" {
				return nil, fmt.Errorf("empty bracket at offset %d in %q", i, s)
			}
			if eq := strings.IndexByte(inner, '='); eq >= 0 {
				key, val := inner[:eq], inner[eq+1:]
				if key == "" {
					return nil, fmt.Errorf("empty key in %q", inner)
				}
				out = append(out, KeySegment(key, val))
			} else {
				n, err := strconv.Atoi(inner)
				if err != nil {
					return nil, fmt.Errorf("bracket %q is neither an index nor key=value", inner)
				}
				if n < 0 {
					return nil, fmt.Errorf("negative index %d in %q", n, s)
				}
				out = append(out, IndexSegment(n))
			}
			i += end + 1
		case '.':
			i++
			if i >= len(s) {
				return nil, fmt.Errorf("trailing '.' in %q", s)
			}
		default:
			j := i
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			out = append(out, FieldSegment(s[i:j]))
			i = j
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no segments parsed from %q", s)
	}
	return out, nil
}

// MustParse is Parse for tests and constants.
func MustParse(s string) Path {
	p, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return p
}

// Get reads the value at p from an unstructured object, reporting whether it exists.
func Get(obj any, p Path) (any, bool) {
	cur := obj
	for _, seg := range p {
		switch {
		case seg.IsField():
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			cur, ok = m[seg.Field]
			if !ok {
				return nil, false
			}
		case seg.IsIndex():
			l, ok := cur.([]any)
			if !ok || seg.Index >= len(l) {
				return nil, false
			}
			cur = l[seg.Index]
		case seg.IsKeyMatch():
			l, ok := cur.([]any)
			if !ok {
				return nil, false
			}
			idx := findByKey(l, seg.Key, seg.Value)
			if idx < 0 {
				return nil, false
			}
			cur = l[idx]
		}
	}
	return cur, true
}

// Set writes value at p, creating intermediate maps as needed.
//
// A key-match segment whose element does not exist appends a new element carrying that key, which
// is how a contributor adds its own entry to a shared list without disturbing anyone else's.
func Set(obj map[string]any, p Path, value any) error {
	if len(p) == 0 {
		return fmt.Errorf("cannot set an empty path")
	}
	if !p[0].IsField() {
		return fmt.Errorf("path must start with a field, got %q", p.String())
	}
	return setIn(obj, p, value)
}

func setIn(container any, p Path, value any) error {
	seg := p[0]
	last := len(p) == 1

	switch {
	case seg.IsField():
		m, ok := container.(map[string]any)
		if !ok {
			return fmt.Errorf("expected a map at %q", seg.Field)
		}
		if last {
			m[seg.Field] = value
			return nil
		}
		child, exists := m[seg.Field]
		if !exists || child == nil {
			child = emptyFor(p[1])
			m[seg.Field] = child
		}
		// A list child must be written back, since a slice append does not mutate in place.
		if next, ok := child.([]any); ok {
			updated, err := setInList(next, p[1:], value)
			if err != nil {
				return err
			}
			m[seg.Field] = updated
			return nil
		}
		return setIn(child, p[1:], value)

	case seg.IsIndex(), seg.IsKeyMatch():
		return fmt.Errorf("internal: list segment %q reached setIn; use setInList", seg)
	}
	return fmt.Errorf("unhandled segment %+v", seg)
}

func setInList(l []any, p Path, value any) ([]any, error) {
	seg := p[0]
	last := len(p) == 1

	var idx int
	switch {
	case seg.IsIndex():
		idx = seg.Index
		for len(l) <= idx {
			l = append(l, map[string]any{})
		}
	case seg.IsKeyMatch():
		idx = findByKey(l, seg.Key, seg.Value)
		if idx < 0 {
			// Append this contributor's own element, seeded with the identifying key.
			l = append(l, map[string]any{seg.Key: seg.Value})
			idx = len(l) - 1
		}
	default:
		return nil, fmt.Errorf("expected a list segment, got %+v", seg)
	}

	if last {
		l[idx] = value
		return l, nil
	}
	if l[idx] == nil {
		l[idx] = emptyFor(p[1])
	}
	if next, ok := l[idx].([]any); ok {
		updated, err := setInList(next, p[1:], value)
		if err != nil {
			return nil, err
		}
		l[idx] = updated
		return l, nil
	}
	if err := setIn(l[idx], p[1:], value); err != nil {
		return nil, err
	}
	return l, nil
}

// Delete removes the value at p. It reports whether anything was removed.
//
// Deleting a key-matched list element removes just that element, which is what makes a revert
// surgical rather than list-wide. Empty parent maps left behind are pruned, so a revert does not
// leave `spec: {}` litter on the target.
func Delete(obj map[string]any, p Path) bool {
	if len(p) == 0 {
		return false
	}
	removed := deleteIn(obj, p)
	if removed {
		pruneEmpty(obj, p)
	}
	return removed
}

func deleteIn(container any, p Path) bool {
	seg := p[0]
	last := len(p) == 1

	switch {
	case seg.IsField():
		m, ok := container.(map[string]any)
		if !ok {
			return false
		}
		if last {
			if _, exists := m[seg.Field]; !exists {
				return false
			}
			delete(m, seg.Field)
			return true
		}
		child, exists := m[seg.Field]
		if !exists {
			return false
		}
		if l, ok := child.([]any); ok {
			updated, removed := deleteInList(l, p[1:])
			if removed {
				m[seg.Field] = updated
			}
			return removed
		}
		return deleteIn(child, p[1:])

	default:
		return false
	}
}

func deleteInList(l []any, p Path) ([]any, bool) {
	seg := p[0]
	last := len(p) == 1

	var idx int
	switch {
	case seg.IsIndex():
		idx = seg.Index
		if idx >= len(l) {
			return l, false
		}
	case seg.IsKeyMatch():
		idx = findByKey(l, seg.Key, seg.Value)
		if idx < 0 {
			return l, false
		}
	default:
		return l, false
	}

	if last {
		return append(l[:idx:idx], l[idx+1:]...), true
	}
	if next, ok := l[idx].([]any); ok {
		updated, removed := deleteInList(next, p[1:])
		if removed {
			l[idx] = updated
		}
		return l, removed
	}
	return l, deleteIn(l[idx], p[1:])
}

// pruneEmpty removes map parents along p that became empty after a delete, deepest first.
func pruneEmpty(obj map[string]any, p Path) {
	for i := len(p) - 1; i > 0; i-- {
		prefix := p[:i]
		v, ok := Get(obj, prefix)
		if !ok {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok || len(m) > 0 {
			return
		}
		if !deleteIn(obj, prefix) {
			return
		}
	}
}

// findByKey returns the index of the first list element whose Key field equals value, or -1.
func findByKey(l []any, key, value string) int {
	for i, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprintf("%v", m[key]) == value {
			return i
		}
	}
	return -1
}

// emptyFor returns the zero container a segment needs as its parent.
func emptyFor(next Segment) any {
	if next.IsIndex() || next.IsKeyMatch() {
		return []any{}
	}
	return map[string]any{}
}

// String renders a segment for error messages.
func (s Segment) String() string {
	switch {
	case s.IsField():
		return s.Field
	case s.IsKeyMatch():
		return "[" + s.Key + "=" + s.Value + "]"
	case s.IsIndex():
		return "[" + strconv.Itoa(s.Index) + "]"
	}
	return "<empty>"
}
