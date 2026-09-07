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

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:generate=false

// RecordedIdentity is the JSON shape stored in the authorized-identity annotation. It mirrors
// authenticationv1.UserInfo, but is declared here so the stored representation is explicitly this
// package's contract rather than whatever the vendored type happens to serialise to.
type RecordedIdentity struct {
	Username string              `json:"username"`
	UID      string              `json:"uid,omitempty"`
	Groups   []string            `json:"groups,omitempty"`
	Extra    map[string][]string `json:"extra,omitempty"`
}

// IdentityFromAnnotations reconstructs the admitting principal from a contributor's annotations.
//
// It reports false when nothing was recorded -- an object that predates the webhook or bypassed
// it. Callers must not treat that as an authorization result either way: there is nothing to
// re-check against.
//
// Objects recorded before the identity annotation existed carry only the username; those fall back
// to a username-only review, which is what the old behaviour was.
func IdentityFromAnnotations(obj metav1.Object) (authenticationv1.UserInfo, bool) {
	annotations := obj.GetAnnotations()

	if raw := annotations[AuthorizedIdentityAnnotation]; raw != "" {
		var stored RecordedIdentity
		if err := json.Unmarshal([]byte(raw), &stored); err == nil && stored.Username != "" {
			user := authenticationv1.UserInfo{
				Username: stored.Username,
				UID:      stored.UID,
				Groups:   stored.Groups,
			}
			if len(stored.Extra) > 0 {
				user.Extra = map[string]authenticationv1.ExtraValue{}
				for k, v := range stored.Extra {
					user.Extra[k] = authenticationv1.ExtraValue(v)
				}
			}
			return user, true
		}
	}

	if username := annotations[AuthorizedAsAnnotation]; username != "" {
		return authenticationv1.UserInfo{Username: username}, true
	}
	return authenticationv1.UserInfo{}, false
}
