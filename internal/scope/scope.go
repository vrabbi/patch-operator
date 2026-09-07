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

// Package scope enforces the containment property that makes this operator safe to hand to
// tenants, and decides which tracker owns a given target.
//
// The containment property: a ResourcePatch can only ever touch objects in its own namespace. That
// is structural rather than a policy check — the target namespace is forced to the contributor's
// namespace and no field expresses anything else — so it holds regardless of how privileged the
// principal creating the contributor is. That matters because a patch emitted by Crossplane or kro
// is admitted as the orchestrator's near-cluster-admin ServiceAccount, which makes every check
// that reasons about the requester weak for exactly the population of patches that matters most.
//
// Validation here runs at admission *and* at reconcile. Admission alone would leave the property
// depending on the webhook being reachable.
package scope

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

// TargetKey identifies one target object. It is the work-queue key that serialises writes, and the
// index key that finds every contributor to a target across both contributor kinds.
type TargetKey struct {
	APIVersion string
	Kind       string
	Namespace  string
	Name       string
}

// String renders a key in a stable form, used as the field-index value.
func (k TargetKey) String() string {
	if k.Namespace == "" {
		return k.APIVersion + "/" + k.Kind + "/" + k.Name
	}
	return k.APIVersion + "/" + k.Kind + "/" + k.Namespace + "/" + k.Name
}

// IsClusterScopedTarget reports whether this key addresses a cluster-scoped object.
func (k TargetKey) IsClusterScopedTarget() bool { return k.Namespace == "" }

// Validate checks a contributor's spec against the rules for its kind.
//
// The table it enforces is DESIGN.md 3.2:
//
//	                          ResourcePatch                     ClusterResourcePatch
//	target kind               namespaced kinds only             namespaced or cluster-scoped
//	target.namespace          own namespace, or unset           required for namespaced kinds
//	namespaceSelector         forbidden                         allowed
//	serviceAccountRef.ns      forbidden                         allowed (gated by an SAR)
//
// isNamespacedKind reports whether the target's kind is namespaced; a nil func skips that check,
// which is what admission does when a RESTMapper lookup is unavailable.
func Validate(c patchv1alpha1.Contributor, isNamespacedKind func(apiVersion, kind string) (bool, error)) field.ErrorList {
	var errs field.ErrorList
	spec := c.GetPatchSpec()
	specPath := field.NewPath("spec")
	targetPath := specPath.Child("target")

	errs = append(errs, validateTargetIdentity(spec, targetPath)...)
	errs = append(errs, validateMode(spec, targetPath)...)
	errs = append(errs, validateLifecycle(spec, specPath)...)
	errs = append(errs, validatePatch(spec, specPath)...)

	if c.IsClusterScoped() {
		errs = append(errs, validateClusterScoped(spec, c, targetPath, specPath, isNamespacedKind)...)
	} else {
		errs = append(errs, validateNamespaced(spec, c, targetPath, specPath, isNamespacedKind)...)
	}

	// A contributor targeting one of this operator's own kinds would reconcile itself. This is
	// loop safety, not a sensitive-kind policy: the operator holds no opinion about which kinds
	// are sensitive, since RBAC already encodes that.
	if isOwnKind(spec.Target.APIVersion, spec.Target.Kind) {
		errs = append(errs, field.Invalid(targetPath.Child("kind"), spec.Target.Kind,
			"a contributor may not target this operator's own kinds; that would reconcile itself"))
	}

	return errs
}

func validateTargetIdentity(spec *patchv1alpha1.ResourcePatchSpec, p *field.Path) field.ErrorList {
	var errs field.ErrorList
	if spec.Target.APIVersion == "" {
		errs = append(errs, field.Required(p.Child("apiVersion"), "the target's apiVersion is required"))
	}
	if spec.Target.Kind == "" {
		errs = append(errs, field.Required(p.Child("kind"), "the target's kind is required"))
	}
	return errs
}

func validateMode(spec *patchv1alpha1.ResourcePatchSpec, p *field.Path) field.ErrorList {
	var errs field.ErrorList
	mode := spec.Target.Mode
	if mode == "" {
		mode = patchv1alpha1.TargetModeSingle
	}

	switch mode {
	case patchv1alpha1.TargetModeSingle:
		if spec.Target.Name == "" {
			errs = append(errs, field.Required(p.Child("name"), "name is required in Single mode"))
		}
		if spec.Target.Selector != nil {
			errs = append(errs, field.Forbidden(p.Child("selector"), "selector is only valid in Selector mode"))
		}
		if spec.Target.NamespaceSelector != nil {
			errs = append(errs, field.Forbidden(p.Child("namespaceSelector"),
				"namespaceSelector is only valid in Selector mode"))
		}

	case patchv1alpha1.TargetModeSelector:
		if spec.Target.Name != "" {
			errs = append(errs, field.Forbidden(p.Child("name"), "name is forbidden in Selector mode"))
		}
		if spec.Target.Selector == nil && spec.Target.NamespaceSelector == nil {
			errs = append(errs, field.Required(p.Child("selector"),
				"Selector mode needs a selector or a namespaceSelector"))
		}
		if spec.Target.MaxTargets < 0 {
			errs = append(errs, field.Invalid(p.Child("maxTargets"), spec.Target.MaxTargets,
				"maxTargets must be positive"))
		}

	default:
		errs = append(errs, field.NotSupported(p.Child("mode"), mode,
			[]string{string(patchv1alpha1.TargetModeSingle), string(patchv1alpha1.TargetModeSelector)}))
	}
	return errs
}

func validateLifecycle(spec *patchv1alpha1.ResourcePatchSpec, p *field.Path) field.ErrorList {
	var errs field.ErrorList
	lp := p.Child("lifecycle")

	mode := spec.Target.Mode
	if mode == "" {
		mode = patchv1alpha1.TargetModeSingle
	}

	if mode == patchv1alpha1.TargetModeSelector {
		// "Create N objects matching a selector" is incoherent, and a selector-matched object was
		// by definition not created by this contributor, so it may never delete one.
		if spec.Lifecycle.OnMissing == patchv1alpha1.OnMissingCreate {
			errs = append(errs, field.Forbidden(lp.Child("onMissing"),
				"onMissing: Create is invalid in Selector mode; there is no single object to create"))
		}
		if spec.Lifecycle.OnRelease == patchv1alpha1.OnReleaseDelete {
			errs = append(errs, field.Forbidden(lp.Child("onRelease"),
				"onRelease: Delete is invalid in Selector mode; a matched object was not created by this contributor"))
		}
	}

	// Delete is only ever honoured for the contributor that created the target, so pairing it with
	// a policy that cannot create is a configuration error worth catching early. The runtime rule
	// stands independently, in case a spec reached the cluster without admission.
	if spec.Lifecycle.OnRelease == patchv1alpha1.OnReleaseDelete &&
		spec.Lifecycle.OnMissing != patchv1alpha1.OnMissingCreate {
		errs = append(errs, field.Forbidden(lp.Child("onRelease"),
			"onRelease: Delete requires onMissing: Create; only the contributor that created a target may delete it"))
	}

	if spec.Lifecycle.OnMissing == patchv1alpha1.OnMissingCreate && spec.Base == nil {
		errs = append(errs, field.Required(p.Child("base"),
			"onMissing: Create needs spec.base as the seed manifest"))
	}

	if spec.Lifecycle.BaseReconcile == patchv1alpha1.BaseReconcileEnforce {
		errs = append(errs, field.Forbidden(lp.Child("baseReconcile"),
			"baseReconcile: Enforce is not implemented in v1alpha1; it needs its own priority semantics against contributions"))
	}
	return errs
}

func validatePatch(spec *patchv1alpha1.ResourcePatchSpec, p *field.Path) field.ErrorList {
	var errs field.ErrorList
	pp := p.Child("patch")

	patchType := spec.Patch.Type
	if patchType == "" {
		patchType = patchv1alpha1.PatchTypeStrategicMerge
	}

	switch patchType {
	case patchv1alpha1.PatchTypeStrategicMerge, patchv1alpha1.PatchTypeMerge:
		if spec.Patch.Value == nil || len(spec.Patch.Value.Raw) == 0 {
			errs = append(errs, field.Required(pp.Child("value"),
				fmt.Sprintf("value is required for type %s", patchType)))
		}
		if spec.Patch.Ops != nil {
			errs = append(errs, field.Forbidden(pp.Child("ops"),
				fmt.Sprintf("ops is only valid for type %s", patchv1alpha1.PatchTypeJSON6902)))
		}

	case patchv1alpha1.PatchTypeJSON6902:
		if spec.Patch.Ops == nil || len(spec.Patch.Ops.Raw) == 0 {
			errs = append(errs, field.Required(pp.Child("ops"), "ops is required for type JSON6902"))
		}
		if spec.Patch.Value != nil {
			errs = append(errs, field.Forbidden(pp.Child("value"), "value is not used for type JSON6902"))
		}

	default:
		errs = append(errs, field.NotSupported(pp.Child("type"), patchType, []string{
			string(patchv1alpha1.PatchTypeStrategicMerge),
			string(patchv1alpha1.PatchTypeMerge),
			string(patchv1alpha1.PatchTypeJSON6902),
		}))
	}

	// mergeKeys only mean anything to the client-side merge; under SSA the API server decides list
	// semantics from the schema. Accepting them silently would imply they take effect.
	if len(spec.Patch.MergeKeys) > 0 && spec.Apply.Mode == patchv1alpha1.ApplyModeServerSideApply {
		errs = append(errs, field.Forbidden(pp.Child("mergeKeys"),
			"mergeKeys only apply under apply.mode: ClientSideApply; server-side apply takes list semantics from the target's schema"))
	}
	for i, mk := range spec.Patch.MergeKeys {
		if mk.Path == "" {
			errs = append(errs, field.Required(pp.Child("mergeKeys").Index(i).Child("path"), "path is required"))
		}
		if mk.Key == "" {
			errs = append(errs, field.Required(pp.Child("mergeKeys").Index(i).Child("key"), "key is required"))
		}
	}
	return errs
}

// validateNamespaced enforces the containment property. Everything here exists so that a
// ResourcePatch cannot reach outside its own namespace whatever its spec says.
func validateNamespaced(
	spec *patchv1alpha1.ResourcePatchSpec,
	c patchv1alpha1.Contributor,
	targetPath, specPath *field.Path,
	isNamespacedKind func(string, string) (bool, error),
) field.ErrorList {
	var errs field.ErrorList
	own := c.GetNamespace()

	// The target namespace may be omitted, but never point elsewhere.
	if spec.Target.Namespace != "" && spec.Target.Namespace != own {
		errs = append(errs, field.Invalid(targetPath.Child("namespace"), spec.Target.Namespace,
			fmt.Sprintf("a ResourcePatch may only target its own namespace (%q); use a ClusterResourcePatch to cross a namespace boundary", own)))
	}

	// A cluster-scoped target has no namespace to be contained by.
	if isNamespacedKind != nil {
		namespaced, err := isNamespacedKind(spec.Target.APIVersion, spec.Target.Kind)
		if err == nil && !namespaced {
			errs = append(errs, field.Invalid(targetPath.Child("kind"), spec.Target.Kind,
				"a ResourcePatch may only target namespaced kinds; use a ClusterResourcePatch for a cluster-scoped target"))
		}
	}

	if spec.Target.NamespaceSelector != nil {
		errs = append(errs, field.Forbidden(targetPath.Child("namespaceSelector"),
			"namespaceSelector is forbidden on a ResourcePatch, which is confined to its own namespace"))
	}

	// Containment extends to the identity the write runs as: a namespaced contributor may not
	// borrow a ServiceAccount from another namespace.
	if spec.ServiceAccountRef != nil && spec.ServiceAccountRef.Namespace != "" &&
		spec.ServiceAccountRef.Namespace != own {
		errs = append(errs, field.Invalid(specPath.Child("serviceAccountRef", "namespace"),
			spec.ServiceAccountRef.Namespace,
			"a ResourcePatch may only impersonate a ServiceAccount in its own namespace"))
	}
	return errs
}

func validateClusterScoped(
	spec *patchv1alpha1.ResourcePatchSpec,
	_ patchv1alpha1.Contributor,
	targetPath, _ *field.Path,
	isNamespacedKind func(string, string) (bool, error),
) field.ErrorList {
	var errs field.ErrorList

	if isNamespacedKind == nil {
		return errs
	}
	namespaced, err := isNamespacedKind(spec.Target.APIVersion, spec.Target.Kind)
	if err != nil {
		return errs
	}

	mode := spec.Target.Mode
	if mode == "" {
		mode = patchv1alpha1.TargetModeSingle
	}

	switch {
	case namespaced && mode == patchv1alpha1.TargetModeSingle && spec.Target.Namespace == "":
		errs = append(errs, field.Required(targetPath.Child("namespace"),
			fmt.Sprintf("namespace is required for the namespaced kind %q", spec.Target.Kind)))
	case !namespaced && spec.Target.Namespace != "":
		errs = append(errs, field.Forbidden(targetPath.Child("namespace"),
			fmt.Sprintf("namespace is forbidden for the cluster-scoped kind %q", spec.Target.Kind)))
	}
	return errs
}

// isOwnKind reports whether a target names one of this operator's kinds.
func isOwnKind(apiVersion, kind string) bool {
	if !strings.HasPrefix(apiVersion, patchv1alpha1.GroupVersion.Group+"/") &&
		apiVersion != patchv1alpha1.GroupVersion.Group {
		return false
	}
	switch kind {
	case patchv1alpha1.KindResourcePatch,
		patchv1alpha1.KindClusterResourcePatch,
		patchv1alpha1.KindSharedResource,
		patchv1alpha1.KindClusterSharedResource:
		return true
	}
	return false
}

// ResolveTargetNamespace returns the namespace a contributor's Single-mode target resolves in.
//
// For a ResourcePatch this is always the contributor's own namespace, whatever the spec says —
// which is why the containment property survives a spec that reached the cluster without passing
// admission.
func ResolveTargetNamespace(c patchv1alpha1.Contributor) string {
	if !c.IsClusterScoped() {
		return c.GetNamespace()
	}
	return c.GetPatchSpec().Target.Namespace
}

// TargetKeyFor builds the key for a contributor's Single-mode target.
func TargetKeyFor(c patchv1alpha1.Contributor) TargetKey {
	spec := c.GetPatchSpec()
	return TargetKey{
		APIVersion: spec.Target.APIVersion,
		Kind:       spec.Target.Kind,
		Namespace:  ResolveTargetNamespace(c),
		Name:       spec.Target.Name,
	}
}

// TrackerKindFor decides which tracker owns a target, per DESIGN.md 3.5.
//
//  1. A cluster-scoped target always uses a ClusterSharedResource.
//  2. A namespaced target whose contributors are all ResourcePatches uses a SharedResource in that
//     namespace.
//  3. A namespaced target with at least one ClusterResourcePatch uses a ClusterSharedResource,
//     reached by promotion if a SharedResource was already tracking it.
//
// Rule 3 exists because a namespaced tracker cannot honestly count a contributor outside its
// namespace, and an incomplete reference count deletes objects that are still in use. The decision
// is a function of the contributor set, never of arrival order, so two contributors racing to
// create a tracker reach the same answer whoever wins.
func TrackerKindFor(key TargetKey, hasClusterScopedContributor bool) string {
	if key.IsClusterScopedTarget() || hasClusterScopedContributor {
		return patchv1alpha1.KindClusterSharedResource
	}
	return patchv1alpha1.KindSharedResource
}

// TrackerName derives a tracker's name deterministically from its target.
//
// Determinism is what makes a create race benign: two contributors racing produce the same name,
// so one wins and the other sees AlreadyExists, which is a normal outcome. A hash suffix keeps the
// name unique after the readable part is truncated to fit DNS-1123 and 253 characters.
func TrackerName(key TargetKey) string {
	group := ""
	if idx := strings.Index(key.APIVersion, "/"); idx >= 0 {
		group = key.APIVersion[:idx]
	}

	readable := strings.ToLower(key.Kind)
	if group != "" {
		readable += "." + group
	}
	if key.Namespace != "" {
		readable += "-" + key.Namespace
	}
	readable += "-" + key.Name

	readable = sanitizeDNS1123(readable)

	// The hash covers the full key, so two targets that sanitise or truncate to the same readable
	// form still get distinct names.
	sum := sha256.Sum256([]byte(key.String()))
	suffix := hex.EncodeToString(sum[:])[:10]

	maxReadable := validation.DNS1123SubdomainMaxLength - len(suffix) - 1
	if len(readable) > maxReadable {
		readable = readable[:maxReadable]
	}
	readable = strings.Trim(readable, "-.")
	if readable == "" {
		readable = "target"
	}
	return readable + "-" + suffix
}

// sanitizeDNS1123 maps arbitrary text into the DNS-1123 subdomain alphabet.
func sanitizeDNS1123(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	// A leading or trailing separator is invalid in a DNS-1123 subdomain.
	return strings.Trim(string(out), "-.")
}

// TrackerNamespace returns the namespace a tracker lives in: the target's namespace for a
// namespaced tracker, empty for a cluster-scoped one.
func TrackerNamespace(trackerKind string, key TargetKey) string {
	if trackerKind == patchv1alpha1.KindSharedResource {
		return key.Namespace
	}
	return ""
}
