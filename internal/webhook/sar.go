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

// Package webhook validates contributors at admission, and re-checks authorization afterwards.
//
// The question every check asks is the same: could this principal have made this change to the
// target directly? Answering it on CREATE alone would be trivially bypassable — create a harmless
// patch, then update it to point at a different target, or flip onRelease to Delete and delete the
// CR. So UPDATE and DELETE are checked too.
package webhook

import (
	"context"
	"fmt"
	"sort"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/scope"
)

// Verb is a Kubernetes API verb checked against a target.
type Verb string

const (
	VerbGet    Verb = "get"
	VerbCreate Verb = "create"
	VerbUpdate Verb = "update"
	VerbPatch  Verb = "patch"
	VerbDelete Verb = "delete"
	// VerbImpersonate gates serviceAccountRef. Without it, naming a ServiceAccount would be a way
	// to *borrow* privilege rather than drop it.
	VerbImpersonate Verb = "impersonate"
)

// Operation is the admission operation being validated.
type Operation string

const (
	OperationCreate Operation = "CREATE"
	OperationUpdate Operation = "UPDATE"
	OperationDelete Operation = "DELETE"
)

// Check is one authorization question.
type Check struct {
	Verb      Verb
	Group     string
	Resource  string
	Namespace string
	// Name is empty when the check is namespace-wide, which is the correct — and stricter —
	// question for Selector mode, where the matched names are not knowable at admission.
	Name string
}

func (c Check) String() string {
	target := c.Resource
	if c.Group != "" {
		target = c.Resource + "." + c.Group
	}
	switch {
	case c.Namespace != "" && c.Name != "":
		return fmt.Sprintf("%s %s %s/%s", c.Verb, target, c.Namespace, c.Name)
	case c.Namespace != "":
		return fmt.Sprintf("%s %s in namespace %s", c.Verb, target, c.Namespace)
	case c.Name != "":
		return fmt.Sprintf("%s %s %s", c.Verb, target, c.Name)
	default:
		return fmt.Sprintf("%s %s (cluster-wide)", c.Verb, target)
	}
}

// ChecksFor returns the authorization questions an operation on a contributor implies.
//
// The table is DESIGN.md 7.1:
//
//	CREATE  patch and update; plus create if onMissing: Create; plus delete if onRelease: Delete
//	UPDATE  the same set, against both old and new target when spec.target changes
//	DELETE  patch/update for Revert; delete for Delete; nothing for Orphan, which touches nothing
//
// resourceFor maps a target's kind to its resource name and scope; a nil func falls back to a
// naive pluralisation so admission still works without a RESTMapper.
func ChecksFor(
	op Operation,
	c patchv1alpha1.Contributor,
	old patchv1alpha1.Contributor,
	resourceFor func(apiVersion, kind string) (resource string, namespaced bool, err error),
) ([]Check, error) {
	var out []Check

	switch op {
	case OperationCreate, OperationUpdate:
		checks, err := checksForSpec(c, resourceFor)
		if err != nil {
			return nil, err
		}
		out = append(out, checks...)

		// A retargeted contributor must be authorized against the target it is leaving as well as
		// the one it is joining: it will withdraw fields from the old one.
		if op == OperationUpdate && old != nil && targetChanged(old, c) {
			oldChecks, err := checksForSpec(old, resourceFor)
			if err != nil {
				return nil, err
			}
			out = append(out, oldChecks...)
		}

	case OperationDelete:
		// A DELETE AdmissionReview carries oldObject and no object, so the policy that decides
		// which verb to check has to be read from the object being removed. Reading it from the
		// wrong place would fail open.
		subject := c
		if subject == nil {
			subject = old
		}
		if subject == nil {
			return nil, fmt.Errorf("DELETE admission carried neither object nor oldObject")
		}

		spec := subject.GetPatchSpec()
		switch spec.Lifecycle.OnRelease {
		case patchv1alpha1.OnReleaseOrphan:
			// Orphan leaves the fields in place, so the deletion writes nothing to the target and
			// there is nothing to authorize.
			return nil, nil
		case patchv1alpha1.OnReleaseDelete:
			checks, err := targetChecks(subject, resourceFor, VerbDelete)
			if err != nil {
				return nil, err
			}
			out = append(out, checks...)
		default: // Revert
			checks, err := targetChecks(subject, resourceFor, VerbUpdate, VerbPatch)
			if err != nil {
				return nil, err
			}
			out = append(out, checks...)
		}

	default:
		return nil, fmt.Errorf("unsupported operation %q", op)
	}

	return dedupeChecks(out), nil
}

// checksForSpec returns the write checks a contributor's spec implies.
func checksForSpec(
	c patchv1alpha1.Contributor,
	resourceFor func(string, string) (string, bool, error),
) ([]Check, error) {
	verbs := []Verb{VerbUpdate, VerbPatch}

	spec := c.GetPatchSpec()
	if spec.Lifecycle.OnMissing == patchv1alpha1.OnMissingCreate {
		verbs = append(verbs, VerbCreate)
	}
	if spec.Lifecycle.OnRelease == patchv1alpha1.OnReleaseDelete {
		verbs = append(verbs, VerbDelete)
	}

	checks, err := targetChecks(c, resourceFor, verbs...)
	if err != nil {
		return nil, err
	}

	// serviceAccountRef needs its own check. You may only point a contributor at a ServiceAccount
	// you could already impersonate — otherwise the field would let a caller acquire privilege
	// rather than voluntarily drop it.
	if ref := spec.ServiceAccountRef; ref != nil && ref.Name != "" {
		namespace := ref.Namespace
		if !c.IsClusterScoped() {
			namespace = c.GetNamespace()
		}
		checks = append(checks, Check{
			Verb:      VerbImpersonate,
			Group:     "",
			Resource:  "serviceaccounts",
			Namespace: namespace,
			Name:      ref.Name,
		})
	}
	return checks, nil
}

// targetChecks builds one Check per verb against a contributor's target.
func targetChecks(
	c patchv1alpha1.Contributor,
	resourceFor func(string, string) (string, bool, error),
	verbs ...Verb,
) ([]Check, error) {
	spec := c.GetPatchSpec()

	gv, err := schema.ParseGroupVersion(spec.Target.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("parsing target apiVersion %q: %w", spec.Target.APIVersion, err)
	}

	resource := guessResource(spec.Target.Kind)
	namespaced := true
	if resourceFor != nil {
		if r, ns, err := resourceFor(spec.Target.APIVersion, spec.Target.Kind); err == nil {
			resource, namespaced = r, ns
		}
	}

	namespace := ""
	if namespaced {
		namespace = scope.ResolveTargetNamespace(c)
	}

	// In Selector mode the matched names are not known at admission, so the check is issued
	// against the resource and namespace with an empty name. That asks "may this principal patch
	// ANY object of this kind here?", which is the stricter question — and the correct one, since a
	// selector may match objects that do not exist yet.
	name := spec.Target.Name
	if spec.Target.Mode == patchv1alpha1.TargetModeSelector {
		name = ""
	}

	out := make([]Check, 0, len(verbs))
	for _, v := range verbs {
		out = append(out, Check{
			Verb:      v,
			Group:     gv.Group,
			Resource:  resource,
			Namespace: namespace,
			Name:      name,
		})
	}
	return out, nil
}

// targetChanged reports whether an update retargeted the contributor.
func targetChanged(old, new patchv1alpha1.Contributor) bool {
	a, b := old.GetPatchSpec().Target, new.GetPatchSpec().Target
	return a.APIVersion != b.APIVersion ||
		a.Kind != b.Kind ||
		a.Name != b.Name ||
		a.Namespace != b.Namespace ||
		a.Mode != b.Mode
}

// dedupeChecks removes duplicates and orders the result, so a denial message is stable.
func dedupeChecks(in []Check) []Check {
	seen := map[string]Check{}
	for _, c := range in {
		seen[c.String()] = c
	}
	out := make([]Check, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// guessResource pluralises a kind for the case where no RESTMapper is available. It is a fallback,
// not the primary path: a wrong guess makes a SAR fail closed rather than open, because the
// principal will not hold rights on a resource name that does not exist.
func guessResource(kind string) string {
	l := strings.ToLower(kind)
	switch {
	case strings.HasSuffix(l, "s"), strings.HasSuffix(l, "x"), strings.HasSuffix(l, "ch"):
		return l + "es"
	case strings.HasSuffix(l, "y"):
		return strings.TrimSuffix(l, "y") + "ies"
	default:
		return l + "s"
	}
}

// Authorizer answers authorization questions by issuing SubjectAccessReviews.
type Authorizer struct {
	Client client.Client
	Mapper meta.RESTMapper
}

// Denial explains why a request was refused.
type Denial struct {
	Check  Check
	Reason string
}

// Authorize runs every check as the given principal, returning the first denial.
//
// The principal is taken from request.userInfo, which for a patch created by Crossplane or kro is
// the *orchestrator's* ServiceAccount rather than the human behind the XR. That is why this layer
// does comparatively little for composed patches, and why serviceAccountRef exists.
func (a *Authorizer) Authorize(
	ctx context.Context,
	user authenticationv1.UserInfo,
	checks []Check,
) (*Denial, error) {
	for _, c := range checks {
		sar := &authv1.SubjectAccessReview{
			Spec: authv1.SubjectAccessReviewSpec{
				User:   user.Username,
				Groups: user.Groups,
				UID:    user.UID,
				ResourceAttributes: &authv1.ResourceAttributes{
					Verb:      string(c.Verb),
					Group:     c.Group,
					Resource:  c.Resource,
					Namespace: c.Namespace,
					Name:      c.Name,
				},
			},
		}
		if len(user.Extra) > 0 {
			sar.Spec.Extra = map[string]authv1.ExtraValue{}
			for k, v := range user.Extra {
				sar.Spec.Extra[k] = authv1.ExtraValue(v)
			}
		}

		if err := a.Client.Create(ctx, sar); err != nil {
			return nil, fmt.Errorf("creating SubjectAccessReview for %s: %w", c, err)
		}
		if !sar.Status.Allowed || sar.Status.Denied {
			reason := sar.Status.Reason
			if reason == "" {
				reason = "not permitted by RBAC"
			}
			return &Denial{Check: c, Reason: reason}, nil
		}
	}
	return nil, nil
}

// ResourceFor resolves a kind to its resource name and scope via the RESTMapper.
func (a *Authorizer) ResourceFor(apiVersion, kind string) (string, bool, error) {
	if a.Mapper == nil {
		return guessResource(kind), true, fmt.Errorf("no RESTMapper configured")
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return "", false, err
	}
	mapping, err := a.Mapper.RESTMapping(gv.WithKind(kind).GroupKind(), gv.Version)
	if err != nil {
		return "", false, err
	}
	return mapping.Resource.Resource, mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// IsNamespacedKind adapts the RESTMapper for scope.Validate.
func (a *Authorizer) IsNamespacedKind(apiVersion, kind string) (bool, error) {
	_, namespaced, err := a.ResourceFor(apiVersion, kind)
	return namespaced, err
}

// Reauthorizer re-checks, on a TTL, that the principal recorded at admission may still make the
// writes a contributor implies.
//
// This is the defence-in-depth half of DESIGN.md 7.2. Admission is point-in-time: a contributor
// created while its author held broad rights would otherwise keep working forever after those
// rights were revoked, because nothing ever touches the object again.
//
// It deliberately does not revert on failure. Losing authorization is not the same as being
// released, and silently tearing down a tenant's ingress rule because an RBAC binding was
// reorganised would be worse than the exposure.
type Reauthorizer struct {
	Authorizer *Authorizer
}

// Authorize implements the controller's Reauthorizer interface.
//
// The identity comes from the annotations the PrincipalRecorder wrote, groups included. Asking
// with the username alone would deny nearly every real principal: RBAC is bound to groups far more
// often than to names, so a cluster admin authenticating by client certificate (authorized through
// system:masters) or an OIDC user (authorized through their provider's groups) would look
// unauthorized to a review that does not carry them.
func (r *Reauthorizer) Authorize(
	ctx context.Context,
	user authenticationv1.UserInfo,
	c patchv1alpha1.Contributor,
) (bool, string, error) {
	checks, err := ChecksFor(OperationCreate, c, nil, r.Authorizer.ResourceFor)
	if err != nil {
		return false, err.Error(), nil
	}

	// A ServiceAccount recorded before the identity annotation existed carries no groups, so the
	// ones the API server would attach are reconstructed; without them an RBAC binding to
	// system:serviceaccounts would not be honoured.
	if len(user.Groups) == 0 && strings.HasPrefix(user.Username, "system:serviceaccount:") {
		user.Groups = []string{"system:serviceaccounts", "system:authenticated"}
	}

	denial, err := r.Authorizer.Authorize(ctx, user, checks)
	if err != nil {
		return false, "", err
	}
	if denial != nil {
		return false,
			fmt.Sprintf("%s may no longer %s: %s", user.Username, denial.Check, denial.Reason), nil
	}
	return true, "", nil
}
