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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/scope"
)

// ContributorValidator validates one contributor kind at admission.
//
// One handler serves both ResourcePatch and ClusterResourcePatch: the checks differ by what the
// kind may reach, not by shape, so the difference lives in scope.Validate and ChecksFor rather
// than in two handlers.
//
// It runs on CREATE, UPDATE and DELETE with failurePolicy: Fail. That is the correct trade for an
// authorization webhook and it makes webhook availability a hard dependency, which the docs say
// plainly.
type ContributorValidator struct {
	// New returns a fresh empty object of the validated kind.
	New func() patchv1alpha1.Contributor

	// Authorizer issues the SubjectAccessReviews. Required.
	Authorizer *Authorizer

	// Decoder decodes admission request objects.
	Decoder admission.Decoder
}

var _ admission.Handler = &ContributorValidator{}

// Handle validates one admission request.
func (v *ContributorValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithValues(
		"operation", req.Operation, "name", req.Name, "namespace", req.Namespace)

	op := Operation(req.Operation)

	obj, oldObj, err := v.decode(req)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	// Spec validation first: a spec that cannot be honoured should be rejected on its own terms,
	// with a message naming the field, rather than producing a confusing authorization denial.
	if op != OperationDelete && obj != nil {
		if errs := scope.Validate(obj, v.Authorizer.IsNamespacedKind); len(errs) > 0 {
			return admission.Denied(errs.ToAggregate().Error())
		}
	}

	checks, err := ChecksFor(op, obj, oldObj, v.Authorizer.ResourceFor)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if len(checks) == 0 {
		// Nothing to authorize: an Orphan release writes nothing to the target.
		return admission.Allowed("")
	}

	denial, err := v.Authorizer.Authorize(ctx, req.UserInfo, checks)
	if err != nil {
		// Fail closed. An authorization check that could not be completed is not an approval.
		logger.Error(err, "SubjectAccessReview failed; denying")
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if denial != nil {
		return admission.Denied(fmt.Sprintf(
			"%s may not %s, so it may not create a contributor that does: %s",
			req.UserInfo.Username, denial.Check, denial.Reason))
	}

	// Record the admitting principal so the controller can re-run this check on a TTL. Admission
	// is point-in-time; without this a contributor whose author later lost those rights would keep
	// writing forever, because nothing ever touches the object again.
	if op == OperationCreate || op == OperationUpdate {
		return v.recordPrincipal(req, obj)
	}
	return admission.Allowed("")
}

// decode extracts the object and, for updates and deletes, the previous object.
func (v *ContributorValidator) decode(req admission.Request) (obj, oldObj patchv1alpha1.Contributor, err error) {
	if len(req.Object.Raw) > 0 {
		decoded := v.New()
		if err := v.Decoder.DecodeRaw(req.Object, decoded); err != nil {
			return nil, nil, fmt.Errorf("decoding object: %w", err)
		}
		obj = decoded
	}

	// A DELETE AdmissionReview carries oldObject and no object. Reading the lifecycle policy from
	// the wrong side would fail open, so the DELETE path in ChecksFor depends on this being right.
	if len(req.OldObject.Raw) > 0 {
		decoded := v.New()
		if err := v.Decoder.DecodeRaw(req.OldObject, decoded); err != nil {
			return nil, nil, fmt.Errorf("decoding oldObject: %w", err)
		}
		oldObj = decoded
	}

	if obj == nil && oldObj == nil {
		return nil, nil, fmt.Errorf("admission request carried neither object nor oldObject")
	}
	return obj, oldObj, nil
}

// recordPrincipal patches the authorized-as annotation onto the admitted object.
func (v *ContributorValidator) recordPrincipal(
	req admission.Request,
	obj patchv1alpha1.Contributor,
) admission.Response {
	existing := obj.GetAnnotations()[patchv1alpha1.AuthorizedAsAnnotation]
	if existing == req.UserInfo.Username {
		return admission.Allowed("")
	}

	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[patchv1alpha1.AuthorizedAsAnnotation] = req.UserInfo.Username
	obj.SetAnnotations(annotations)

	marshaled, err := json.Marshal(obj)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

// Operations returns the admission operations this validator must be registered for.
//
// All three, deliberately. CREATE-only checking is bypassed by admitting a harmless patch and then
// updating it to point at a different target, or by flipping onRelease to Delete and deleting the
// object.
func Operations() []admissionregv1.OperationType {
	return []admissionregv1.OperationType{
		admissionregv1.Create,
		admissionregv1.Update,
		admissionregv1.Delete,
	}
}
