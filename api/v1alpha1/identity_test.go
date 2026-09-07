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

package v1alpha1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func withAnnotations(a map[string]string) metav1.Object {
	return &ResourcePatch{ObjectMeta: metav1.ObjectMeta{Annotations: a}}
}

// The whole point of the annotation is that the re-check can ask the same question admission
// asked. Groups are the part that matters: RBAC is bound to them far more often than to names.
func TestIdentityFromAnnotationsRoundTrip(t *testing.T) {
	stored, err := json.Marshal(RecordedIdentity{
		Username: "alice@example.com",
		UID:      "uid-1",
		Groups:   []string{"system:masters", "system:authenticated"},
		Extra:    map[string][]string{"scopes": {"a", "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	user, ok := IdentityFromAnnotations(withAnnotations(map[string]string{
		AuthorizedAsAnnotation:       "alice@example.com",
		AuthorizedIdentityAnnotation: string(stored),
	}))
	if !ok {
		t.Fatal("a recorded identity was not read back")
	}
	if user.Username != "alice@example.com" || user.UID != "uid-1" {
		t.Errorf("subject = %+v", user)
	}
	if len(user.Groups) != 2 || user.Groups[0] != "system:masters" {
		t.Errorf("groups = %v; without them a group-authorized principal is wrongly denied", user.Groups)
	}
	if len(user.Extra["scopes"]) != 2 {
		t.Errorf("extra = %v", user.Extra)
	}
}

// Objects annotated before the identity annotation existed carry only a username, and must still
// be re-checkable rather than treated as unrecorded.
func TestIdentityFromAnnotationsFallsBackToUsername(t *testing.T) {
	user, ok := IdentityFromAnnotations(withAnnotations(map[string]string{
		AuthorizedAsAnnotation: "bob@example.com",
	}))
	if !ok {
		t.Fatal("a username-only annotation should still yield an identity")
	}
	if user.Username != "bob@example.com" || len(user.Groups) != 0 {
		t.Errorf("subject = %+v", user)
	}
}

// Corrupt JSON must not silently produce an empty subject: an empty username in a
// SubjectAccessReview is a different question from the one intended, and would deny.
func TestIdentityFromAnnotationsIgnoresCorruptJSON(t *testing.T) {
	user, ok := IdentityFromAnnotations(withAnnotations(map[string]string{
		AuthorizedAsAnnotation:       "carol@example.com",
		AuthorizedIdentityAnnotation: "{not json",
	}))
	if !ok || user.Username != "carol@example.com" {
		t.Errorf("corrupt JSON should fall back to the username, got ok=%v subject=%+v", ok, user)
	}
}

func TestIdentityFromAnnotationsReportsNothingRecorded(t *testing.T) {
	if _, ok := IdentityFromAnnotations(withAnnotations(nil)); ok {
		t.Error("an object with no annotations must report no recorded identity")
	}
}
