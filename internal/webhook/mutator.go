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
	"fmt"
	"net/http"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

// PrincipalRecorder stamps the admitting principal onto a contributor.
//
// This is a *mutating* webhook, separate from ContributorValidator, because a
// ValidatingWebhookConfiguration's patch is discarded by the API server: a validating handler can
// only admit or deny. The authorization boundary stays in the validator, which runs afterwards and
// sees the final object; this handler exists solely so the recorded principal is there for the
// controller's TTL re-check.
//
// Without it, ContributorReconciler.reauthorize finds no principal and returns without checking
// anything -- so a contributor whose author later lost the rights would keep writing forever.
type PrincipalRecorder struct {
	// New returns a fresh empty object of the mutated kind.
	New func() patchv1alpha1.Contributor

	// Decoder decodes admission request objects.
	Decoder admission.Decoder
}

var _ admission.Handler = &PrincipalRecorder{}

// Handle records req.UserInfo.Username in the authorized-as annotation.
//
// The annotation is (re)written on CREATE and on any UPDATE that changes the spec, and left alone
// otherwise. That distinction matters: the operator itself issues UPDATEs to add and remove its
// finalizer, and overwriting the annotation there would replace the tenant's identity with the
// operator's own service account -- which is privileged, so every later re-check would pass
// vacuously. Any spec change is re-recorded, since redirecting a contributor at a new target is
// exactly the escalation the re-check exists to catch.
func (m *PrincipalRecorder) Handle(_ context.Context, req admission.Request) admission.Response {
	op := Operation(req.Operation)
	if op != OperationCreate && op != OperationUpdate {
		return admission.Allowed("")
	}
	if len(req.Object.Raw) == 0 {
		return admission.Allowed("")
	}

	obj := m.New()
	if err := m.Decoder.DecodeRaw(req.Object, obj); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding object: %w", err))
	}

	if !m.shouldRecord(op, req, obj) {
		return admission.Allowed("")
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if annotations[patchv1alpha1.AuthorizedAsAnnotation] == req.UserInfo.Username {
		return admission.Allowed("")
	}
	annotations[patchv1alpha1.AuthorizedAsAnnotation] = req.UserInfo.Username
	obj.SetAnnotations(annotations)

	marshaled, err := json.Marshal(obj)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

// shouldRecord reports whether this request re-establishes who authorized the contribution.
func (m *PrincipalRecorder) shouldRecord(
	op Operation,
	req admission.Request,
	obj patchv1alpha1.Contributor,
) bool {
	if op == OperationCreate {
		return true
	}
	if obj.GetAnnotations()[patchv1alpha1.AuthorizedAsAnnotation] == "" {
		// An object that somehow has no principal gets one, whoever is updating it. A missing
		// annotation disables the re-check entirely, which is the worse outcome.
		return true
	}
	if len(req.OldObject.Raw) == 0 {
		return true
	}
	oldObj := m.New()
	if err := m.Decoder.DecodeRaw(req.OldObject, oldObj); err != nil {
		// Cannot tell whether the spec changed, so assume it did.
		return true
	}
	return !apiequality.Semantic.DeepEqual(oldObj.GetPatchSpec(), obj.GetPatchSpec())
}

// MutatingOperations returns the admission operations the recorder must be registered for.
//
// DELETE is absent: there is nothing left to annotate, and the validator covers the authorization
// check on that path.
func MutatingOperations() []admissionregv1.OperationType {
	return []admissionregv1.OperationType{
		admissionregv1.Create,
		admissionregv1.Update,
	}
}
