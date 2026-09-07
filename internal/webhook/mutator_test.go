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
	"encoding/json"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

// The recorded principal is what makes the controller's TTL re-authorization possible at all. When
// the annotation is missing, reauthorize finds nothing to check and returns without asserting
// anything -- so these tests guard a silent failure, not a visible one.

func recorder(t *testing.T) *PrincipalRecorder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := patchv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	return &PrincipalRecorder{
		New:     func() patchv1alpha1.Contributor { return &patchv1alpha1.ResourcePatch{} },
		Decoder: admission.NewDecoder(scheme),
	}
}

func mustJSON(t *testing.T, obj any) runtime.RawExtension {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return runtime.RawExtension{Raw: b}
}

// annotationFromPatch replays the JSON patch a response carries onto the submitted object, so the
// assertion is about the object the API server would end up storing rather than about patch
// syntax.
func annotationFromPatch(t *testing.T, req admission.Request, resp admission.Response) string {
	t.Helper()
	if !resp.Allowed {
		t.Fatalf("request was denied: %v", resp.Result)
	}
	obj := &patchv1alpha1.ResourcePatch{}
	if err := json.Unmarshal(req.Object.Raw, obj); err != nil {
		t.Fatalf("decoding submitted object: %v", err)
	}
	for _, op := range resp.Patches {
		if op.Operation == "remove" {
			t.Fatalf("the recorder removed %s; it must only add the annotation", op.Path)
		}
		switch op.Path {
		case "/metadata/annotations":
			m, ok := op.Value.(map[string]any)
			if !ok {
				t.Fatalf("annotations patch value is %T", op.Value)
			}
			if v, ok := m[patchv1alpha1.AuthorizedAsAnnotation].(string); ok {
				return v
			}
		case "/metadata/annotations/terasky.com~1authorized-as":
			v, ok := op.Value.(string)
			if !ok {
				t.Fatalf("annotation patch value is %T", op.Value)
			}
			return v
		}
	}
	return obj.GetAnnotations()[patchv1alpha1.AuthorizedAsAnnotation]
}

// applyPatches replays a response's JSON patch onto the submitted object.
func applyPatches(t *testing.T, req admission.Request, resp admission.Response) *patchv1alpha1.ResourcePatch {
	t.Helper()
	patch, err := json.Marshal(resp.Patches)
	if err != nil {
		t.Fatalf("marshalling patch: %v", err)
	}
	decoded, err := jsonpatch.DecodePatch(patch)
	if err != nil {
		t.Fatalf("decoding patch %s: %v", patch, err)
	}
	out, err := decoded.Apply(req.Object.Raw)
	if err != nil {
		t.Fatalf("applying patch %s: %v", patch, err)
	}
	obj := &patchv1alpha1.ResourcePatch{}
	if err := json.Unmarshal(out, obj); err != nil {
		t.Fatalf("decoding patched object: %v", err)
	}
	return obj
}

func TestPrincipalRecorderStampsCreator(t *testing.T) {
	rp := namespacedContributor(nil)
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		UserInfo:  authenticationv1.UserInfo{Username: "alice@example.com"},
		Object:    mustJSON(t, rp),
	}}

	got := annotationFromPatch(t, req, recorder(t).Handle(context.Background(), req))
	if got != "alice@example.com" {
		t.Errorf("recorded principal = %q, want alice@example.com", got)
	}
}

// A principal the submitter wrote themselves must not be believed: it is the whole basis of the
// re-check, so it is always overwritten with the authenticated identity.
func TestPrincipalRecorderOverwritesForgedPrincipal(t *testing.T) {
	rp := namespacedContributor(nil)
	rp.SetAnnotations(map[string]string{
		patchv1alpha1.AuthorizedAsAnnotation: "system:masters",
	})
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		UserInfo:  authenticationv1.UserInfo{Username: "mallory@example.com"},
		Object:    mustJSON(t, rp),
	}}

	got := annotationFromPatch(t, req, recorder(t).Handle(context.Background(), req))
	if got != "mallory@example.com" {
		t.Errorf("recorded principal = %q; a forged annotation was trusted", got)
	}
}

// The operator itself updates contributors to add and remove its finalizer. Re-stamping there
// would replace the tenant's identity with the operator's own service account, which is
// privileged -- and every later re-check would then pass vacuously.
func TestPrincipalRecorderLeavesMetadataOnlyUpdateAlone(t *testing.T) {
	old := namespacedContributor(nil)
	old.SetAnnotations(map[string]string{
		patchv1alpha1.AuthorizedAsAnnotation: "alice@example.com",
	})
	updated := old.DeepCopy()
	updated.SetFinalizers([]string{patchv1alpha1.ContributorFinalizer})

	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		UserInfo: authenticationv1.UserInfo{
			Username: "system:serviceaccount:patch-operator-system:controller-manager",
		},
		Object:    mustJSON(t, updated),
		OldObject: mustJSON(t, old),
	}}

	resp := recorder(t).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("metadata-only update was denied: %v", resp.Result)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("the operator's own finalizer update re-stamped the principal: %v", resp.Patches)
	}
}

// Redirecting a contributor at a different target is precisely the escalation the re-check exists
// to catch, so a spec change re-records whoever made it.
func TestPrincipalRecorderReStampsOnSpecChange(t *testing.T) {
	old := namespacedContributor(nil)
	old.SetAnnotations(map[string]string{
		patchv1alpha1.AuthorizedAsAnnotation: "alice@example.com",
	})
	updated := old.DeepCopy()
	updated.Spec.Target.Name = "some-other-object"

	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		UserInfo:  authenticationv1.UserInfo{Username: "bob@example.com"},
		Object:    mustJSON(t, updated),
		OldObject: mustJSON(t, old),
	}}

	got := annotationFromPatch(t, req, recorder(t).Handle(context.Background(), req))
	if got != "bob@example.com" {
		t.Errorf("recorded principal = %q, want bob@example.com after a target change", got)
	}
}

// An object with no principal gets one on any update: a missing annotation disables the re-check
// entirely, which is worse than attributing it to the updater.
func TestPrincipalRecorderBackfillsMissingPrincipal(t *testing.T) {
	old := namespacedContributor(nil)
	updated := old.DeepCopy()
	updated.SetLabels(map[string]string{"team": "a"})

	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Update,
		UserInfo:  authenticationv1.UserInfo{Username: "carol@example.com"},
		Object:    mustJSON(t, updated),
		OldObject: mustJSON(t, old),
	}}

	got := annotationFromPatch(t, req, recorder(t).Handle(context.Background(), req))
	if got != "carol@example.com" {
		t.Errorf("recorded principal = %q, want carol@example.com", got)
	}
}

// The recorded identity must carry the groups, not just the name. This is the regression the kind
// e2e suite found: a cluster admin authenticating by client certificate is authorized through
// system:masters, so re-checking with the username alone denied a principal that was still fully
// authorized, the operator stopped writing, and a revert never happened.
func TestPrincipalRecorderRecordsGroups(t *testing.T) {
	rp := namespacedContributor(nil)
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		UserInfo: authenticationv1.UserInfo{
			Username: "kubernetes-admin",
			UID:      "uid-9",
			Groups:   []string{"system:masters", "system:authenticated"},
			Extra:    map[string]authenticationv1.ExtraValue{"scopes": {"a"}},
		},
		Object: mustJSON(t, rp),
	}}

	resp := recorder(t).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("denied: %v", resp.Result)
	}

	patched := applyPatches(t, req, resp)
	user, ok := patchv1alpha1.IdentityFromAnnotations(patched)
	if !ok {
		t.Fatal("no identity was recorded")
	}
	if user.Username != "kubernetes-admin" || user.UID != "uid-9" {
		t.Errorf("subject = %+v", user)
	}
	if len(user.Groups) != 2 || user.Groups[0] != "system:masters" {
		t.Errorf("groups = %v; a group-authorized principal would be denied on re-check", user.Groups)
	}
	if len(user.Extra["scopes"]) != 1 {
		t.Errorf("extra = %v", user.Extra)
	}
}

// DELETE carries no object to annotate, and the validator covers authorization on that path.
func TestPrincipalRecorderIgnoresDelete(t *testing.T) {
	old := namespacedContributor(nil)
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Delete,
		UserInfo:  authenticationv1.UserInfo{Username: "alice@example.com"},
		OldObject: mustJSON(t, old),
	}}

	resp := recorder(t).Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("delete was denied by the recorder: %v", resp.Result)
	}
	if len(resp.Patches) != 0 {
		t.Errorf("the recorder patched a delete request: %v", resp.Patches)
	}
}

// The registered operations must exclude DELETE; a mutating webhook has nothing to patch there.
func TestMutatingOperations(t *testing.T) {
	ops := MutatingOperations()
	if len(ops) != 2 {
		t.Fatalf("MutatingOperations() = %v, want CREATE and UPDATE", ops)
	}
	for _, op := range ops {
		if string(op) == "DELETE" {
			t.Errorf("the recorder is registered for DELETE: %v", ops)
		}
	}
}
