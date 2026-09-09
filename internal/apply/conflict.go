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
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

// IsFieldConflict reports whether err is a server-side apply field-ownership conflict, as opposed
// to an optimistic-concurrency conflict on resourceVersion. Both surface as HTTP 409, and treating
// them alike would be wrong: a resourceVersion conflict is retryable, a field conflict is a
// disagreement that needs a policy decision.
func IsFieldConflict(err error) bool {
	if err == nil || !apierrors.IsConflict(err) {
		return false
	}
	status := apierrors.APIStatus(nil)
	if !asAPIStatus(err, &status) {
		return false
	}
	details := status.Status().Details
	if details == nil {
		return false
	}
	for _, c := range details.Causes {
		if c.Type == metav1.CauseTypeFieldManagerConflict {
			return true
		}
	}
	return false
}

// conflictOwnerPattern extracts the manager name the API server quotes in a conflict message, e.g.
//
//	Apply failed with 1 conflict: conflict with "other-manager": .data.key
var conflictOwnerPattern = regexp.MustCompile(`conflict with "([^"]+)"`)

// ConflictingManagers returns the field managers currently owning the fields a rejected apply
// tried to claim, deduplicated and sorted for a stable status.
//
// The structured Causes carry the field path in Field and the owner inside Message, so the owner
// is recovered by pattern rather than by a dedicated field. That is a property of the API's error
// shape, not a shortcut.
func ConflictingManagers(err error) []string {
	if err == nil {
		return nil
	}
	status := apierrors.APIStatus(nil)
	if !asAPIStatus(err, &status) {
		return nil
	}
	details := status.Status().Details
	if details == nil {
		return nil
	}

	seen := map[string]bool{}
	for _, c := range details.Causes {
		if c.Type != metav1.CauseTypeFieldManagerConflict {
			continue
		}
		if m := conflictOwnerPattern.FindStringSubmatch(c.Message); len(m) == 2 {
			seen[m[1]] = true
		}
	}
	// Some server versions put the whole summary in the top-level message only.
	if len(seen) == 0 {
		for _, m := range conflictOwnerPattern.FindAllStringSubmatch(status.Status().Message, -1) {
			seen[m[1]] = true
		}
	}

	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ConflictingFields returns the field paths a rejected apply could not claim.
func ConflictingFields(err error) []string {
	if err == nil {
		return nil
	}
	status := apierrors.APIStatus(nil)
	if !asAPIStatus(err, &status) {
		return nil
	}
	details := status.Status().Details
	if details == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, c := range details.Causes {
		if c.Type == metav1.CauseTypeFieldManagerConflict && c.Field != "" {
			seen[strings.TrimPrefix(c.Field, ".")] = true
		}
	}
	out := make([]string, 0, len(seen))
	for f := range seen {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// IsOurManager reports whether a field manager belongs to this operator.
//
// This is the distinction conflictPolicy: Priority turns on. Forcing another patch-operator
// manager out is internal arbitration by the priority the user declared. Forcing a *foreign*
// controller out is a flap, not a resolution: it will write its value straight back.
func IsOurManager(manager string) bool {
	return strings.HasPrefix(manager, patchv1alpha1.FieldManagerPrefix)
}

// AllOurs reports whether every conflicting manager belongs to this operator, which is the
// precondition for forcing under conflictPolicy: Priority.
func AllOurs(managers []string) bool {
	if len(managers) == 0 {
		return false
	}
	for _, m := range managers {
		if !IsOurManager(m) {
			return false
		}
	}
	return true
}

// asAPIStatus unwraps err into an APIStatus, following wrapped errors.
func asAPIStatus(err error, out *apierrors.APIStatus) bool {
	type statusError interface {
		Status() metav1.Status
	}
	for e := err; e != nil; {
		if s, ok := e.(statusError); ok {
			*out = s.(apierrors.APIStatus)
			return true
		}
		unwrapper, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = unwrapper.Unwrap()
	}
	return false
}
