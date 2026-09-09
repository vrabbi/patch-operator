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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TargetMode selects how a contributor identifies the object(s) it contributes to.
// +kubebuilder:validation:Enum=Single;Selector
type TargetMode string

const (
	// TargetModeSingle resolves to exactly one object, whether or not it currently exists.
	// This is the only mode that supports create-on-missing and reference-counted deletion.
	TargetModeSingle TargetMode = "Single"
	// TargetModeSelector resolves to zero or more existing objects and is patch-only.
	TargetModeSelector TargetMode = "Selector"
)

// OnMissingPolicy decides what happens when the target does not exist.
// +kubebuilder:validation:Enum=Fail;Wait;Create
type OnMissingPolicy string

const (
	// OnMissingFail reports an error when the target is absent.
	OnMissingFail OnMissingPolicy = "Fail"
	// OnMissingWait parks until some other contributor creates the target.
	OnMissingWait OnMissingPolicy = "Wait"
	// OnMissingCreate creates the target from spec.base. Invalid in Selector mode.
	OnMissingCreate OnMissingPolicy = "Create"
)

// OnReleasePolicy decides what happens to this contributor's fields when it is deleted.
// +kubebuilder:validation:Enum=Revert;Orphan;Delete
type OnReleasePolicy string

const (
	// OnReleaseRevert withdraws this contributor's fields, restoring any prior values.
	OnReleaseRevert OnReleasePolicy = "Revert"
	// OnReleaseOrphan leaves this contributor's fields in place.
	OnReleaseOrphan OnReleasePolicy = "Orphan"
	// OnReleaseDelete deletes the whole target once the last contributor releases, but only
	// if this contributor created it. See DESIGN.md 6.2.
	OnReleaseDelete OnReleasePolicy = "Delete"
)

// BaseReconcilePolicy decides whether spec.base is re-asserted after creation.
// +kubebuilder:validation:Enum=CreateOnly;Enforce
type BaseReconcilePolicy string

const (
	// BaseReconcileCreateOnly writes the base once, at creation, and never re-asserts it, so it
	// cannot claw back a field a contributor sets later.
	BaseReconcileCreateOnly BaseReconcilePolicy = "CreateOnly"
	// BaseReconcileEnforce continuously re-applies the base. Reserved; not implemented in v1alpha1.
	BaseReconcileEnforce BaseReconcilePolicy = "Enforce"
)

// ApplyMode selects how the contribution reaches the target.
// +kubebuilder:validation:Enum=ServerSideApply;ClientSideApply
type ApplyMode string

const (
	// ApplyModeServerSideApply lets the API server merge and track field ownership. Preferred.
	ApplyModeServerSideApply ApplyMode = "ServerSideApply"
	// ApplyModeClientSideApply merges in the operator, using spec.patch.mergeKeys for list
	// granularity the target's schema does not declare. Required for atomic lists.
	ApplyModeClientSideApply ApplyMode = "ClientSideApply"
)

// ConflictPolicy decides what happens when two contributors claim the same field path.
// +kubebuilder:validation:Enum=Fail;Priority;Force
type ConflictPolicy string

const (
	// ConflictPolicyFail writes nothing and reports the conflict. Default.
	ConflictPolicyFail ConflictPolicy = "Fail"
	// ConflictPolicyPriority forces only against other patch-operator field managers, never
	// against a foreign controller.
	ConflictPolicyPriority ConflictPolicy = "Priority"
	// ConflictPolicyForce forces unconditionally. Last resort.
	ConflictPolicyForce ConflictPolicy = "Force"
)

// PatchType selects how spec.patch is interpreted.
// +kubebuilder:validation:Enum=StrategicMerge;Merge;JSON6902
type PatchType string

const (
	// PatchTypeStrategicMerge is a strategic merge patch, using mergeKeys under ClientSideApply.
	PatchTypeStrategicMerge PatchType = "StrategicMerge"
	// PatchTypeMerge is an RFC 7386 JSON merge patch.
	PatchTypeMerge PatchType = "Merge"
	// PatchTypeJSON6902 is a list of RFC 6902 JSON patch operations.
	PatchTypeJSON6902 PatchType = "JSON6902"
)

// TargetRef identifies the object(s) a contributor contributes to.
type TargetRef struct {
	// Mode selects single-object or selector-based targeting.
	// +kubebuilder:default=Single
	// +optional
	Mode TargetMode `json:"mode,omitempty"`

	// APIVersion of the target, e.g. "networking.k8s.io/v1".
	// +kubebuilder:validation:MinLength=1
	APIVersion string `json:"apiVersion"`

	// Kind of the target, e.g. "Ingress".
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// Name of the target. Required in Single mode, forbidden in Selector mode.
	// +optional
	Name string `json:"name,omitempty"`

	// Namespace of the target.
	//
	// On a ResourcePatch this is optional and defaults to the ResourcePatch's own namespace; any
	// other value is rejected. On a ClusterResourcePatch it is required for namespaced kinds and
	// forbidden for cluster-scoped ones.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Selector matches targets by label in Selector mode.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// NamespaceSelector restricts Selector mode to matching namespaces. Only valid on
	// ClusterResourcePatch; forbidden on ResourcePatch, which is confined to its own namespace.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// MaxTargets caps how many objects a Selector-mode contributor may resolve to. Exceeding it
	// fails the contributor rather than silently truncating the match set.
	// +kubebuilder:default=100
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxTargets int32 `json:"maxTargets,omitempty"`
}

// LifecycleSpec declares create and release behaviour.
type LifecycleSpec struct {
	// OnMissing decides what happens when the target does not exist.
	// +kubebuilder:default=Wait
	// +optional
	OnMissing OnMissingPolicy `json:"onMissing,omitempty"`

	// OnRelease decides what happens to this contributor's fields when it is deleted.
	// +kubebuilder:default=Revert
	// +optional
	OnRelease OnReleasePolicy `json:"onRelease,omitempty"`

	// AdoptExisting allows OnMissing=Create to adopt an object that already exists rather than
	// failing on the create.
	// +kubebuilder:default=true
	// +optional
	AdoptExisting *bool `json:"adoptExisting,omitempty"`

	// BaseReconcile decides whether spec.base is re-asserted after creation.
	// +kubebuilder:default=CreateOnly
	// +optional
	BaseReconcile BaseReconcilePolicy `json:"baseReconcile,omitempty"`
}

// ApplySpec declares how the contribution reaches the target.
type ApplySpec struct {
	// Mode selects server-side or client-side apply.
	// +kubebuilder:default=ServerSideApply
	// +optional
	Mode ApplyMode `json:"mode,omitempty"`

	// FieldManager overrides the derived field manager name. Defaults to
	// "patch-operator/<namespace>/<name>", hashed if that would exceed the API server's limit.
	// +optional
	FieldManager string `json:"fieldManager,omitempty"`

	// ConflictPolicy decides what happens when another manager owns a field this contributor
	// claims.
	// +kubebuilder:default=Fail
	// +optional
	ConflictPolicy ConflictPolicy `json:"conflictPolicy,omitempty"`
}

// MergeKey declares the identifying key for a list field, supplying the granularity a target's
// schema does not declare. Used under ClientSideApply.
type MergeKey struct {
	// Path to the list field, in dotted notation, e.g. "spec.rules".
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// Key is the field within each list element that identifies it, e.g. "host".
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`
}

// PatchSpec is what this contributor adds to the target.
type PatchSpec struct {
	// Type selects how Value or Ops is interpreted.
	// +kubebuilder:default=StrategicMerge
	// +optional
	Type PatchType `json:"type,omitempty"`

	// Value is the partial object to merge, for StrategicMerge and Merge types.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Value *runtime.RawExtension `json:"value,omitempty"`

	// Ops is the list of RFC 6902 operations, for the JSON6902 type.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Ops *runtime.RawExtension `json:"ops,omitempty"`

	// MergeKeys declares identifying keys for list fields under ClientSideApply.
	// +optional
	MergeKeys []MergeKey `json:"mergeKeys,omitempty"`
}

// ServiceAccountRef names a ServiceAccount the operator impersonates for target writes.
type ServiceAccountRef struct {
	// Name of the ServiceAccount.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the ServiceAccount. Only settable on ClusterResourcePatch, and gated by an
	// "impersonate" SubjectAccessReview against the requesting principal. On ResourcePatch the
	// ServiceAccount is always resolved in the ResourcePatch's own namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ResourcePatchSpec is the spec shared by ResourcePatch and ClusterResourcePatch. The two kinds
// differ only in what Target may reach, not in shape.
type ResourcePatchSpec struct {
	// Target identifies the object(s) this contributor contributes to.
	Target TargetRef `json:"target"`

	// Lifecycle declares create and release behaviour.
	// +optional
	Lifecycle LifecycleSpec `json:"lifecycle,omitempty"`

	// Apply declares how the contribution reaches the target.
	// +optional
	Apply ApplySpec `json:"apply,omitempty"`

	// Priority orders contributors deterministically and breaks conflicts under
	// ConflictPolicy=Priority. Higher wins.
	// +kubebuilder:default=100
	// +optional
	Priority int32 `json:"priority,omitempty"`

	// ServiceAccountRef makes the operator impersonate a ServiceAccount for every target write,
	// so the API server enforces that identity's RBAC continuously rather than once at admission.
	// +optional
	ServiceAccountRef *ServiceAccountRef `json:"serviceAccountRef,omitempty"`

	// Base is the seed manifest, consulted only when Lifecycle.OnMissing is Create. Under the
	// default BaseReconcile=CreateOnly it is written once and never re-asserted.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Base *runtime.RawExtension `json:"base,omitempty"`

	// Patch is what this contributor always contributes.
	Patch PatchSpec `json:"patch"`
}

// TargetStatus reports the state of one resolved target.
type TargetStatus struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	UID string `json:"uid,omitempty"`
	// State mirrors this contributor's ContributorState on the tracker for this target.
	// +optional
	State ContributorState `json:"state,omitempty"`
}

// TrackerRef points at the tracker that owns a target this contributor contributes to.
type TrackerRef struct {
	// Kind is SharedResource or ClusterSharedResource.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ResourcePatchStatus is the status shared by ResourcePatch and ClusterResourcePatch.
type ResourcePatchStatus struct {
	// Conditions follow the Crossplane condition shape so an XR readiness check or a kro status
	// roll-up consumes them without an adapter.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedTargets lists the targets this contributor currently resolves to.
	// +optional
	ObservedTargets []TargetStatus `json:"observedTargets,omitempty"`

	// SharedResourceRefs points at the trackers that own those targets.
	// +optional
	SharedResourceRefs []TrackerRef `json:"sharedResourceRefs,omitempty"`

	// AppliedGeneration is the spec generation last successfully applied to every target.
	// +optional
	AppliedGeneration int64 `json:"appliedGeneration,omitempty"`

	// AuthorizedAs records the principal the admission webhook validated this contributor for, so
	// the controller can re-run its SubjectAccessReview on a TTL. Admission is point-in-time;
	// this closes the revoked-RBAC gap.
	// +optional
	AuthorizedAs string `json:"authorizedAs,omitempty"`

	// LastAuthorizedTime is when the SubjectAccessReview last succeeded.
	// +optional
	LastAuthorizedTime *metav1.Time `json:"lastAuthorizedTime,omitempty"`
}

// Condition types published on contributors and trackers.
const (
	// ConditionReady is true only when the contribution is applied to every resolved target and
	// the contributor is not party to an unresolved conflict.
	ConditionReady = "Ready"
	// ConditionSynced is true when the last reconcile completed without error.
	ConditionSynced = "Synced"
	// ConditionTargetFound is true when every resolved target exists.
	ConditionTargetFound = "TargetFound"
	// ConditionApplied is true when the contribution reached every resolved target.
	ConditionApplied = "Applied"
	// ConditionConflict is true when this contributor is party to a field conflict.
	ConditionConflict = "Conflict"
	// ConditionAuthorized is false when a SubjectAccessReview re-check has failed. The operator
	// then stops writing but does not revert: losing authorization is not being released.
	ConditionAuthorized = "Authorized"
)

// Condition reasons.
const (
	ReasonApplied              = "Applied"
	ReasonWaitingForTarget     = "WaitingForTarget"
	ReasonTargetMissing        = "TargetMissing"
	ReasonConflict             = "Conflict"
	ReasonSuperseded           = "Superseded"
	ReasonBaseMismatch         = "BaseMismatch"
	ReasonListMergeUnsupported = "ListMergeUnsupported"
	ReasonUnauthorized         = "Unauthorized"
	ReasonInvalidSpec          = "InvalidSpec"
	ReasonReleasing            = "Releasing"
	ReasonPromoting            = "Promoting"
	ReasonTargetReplaced       = "TargetReplaced"
	ReasonApplyFailed          = "ApplyFailed"
	ReasonTooManyTargets       = "TooManyTargets"
)

// Finalizers held by the operator.
const (
	// ContributorFinalizer is released only after the contributor's fields have been withdrawn.
	ContributorFinalizer = "terasky.com/contributor"
	// TrackerFinalizer guards a tracker while it still has lifecycle work to do.
	TrackerFinalizer = "terasky.com/shared-resource"
)

// AuthorizedAsAnnotation records the admitting principal.
//
// Written by the PrincipalRecorder mutating webhook rather than the validator: a validating
// webhook's patch is discarded by the API server, so it cannot annotate what it admits.
const AuthorizedAsAnnotation = "terasky.com/authorized-as"

// AuthorizedIdentityAnnotation records the admitting principal's full identity as JSON: username,
// UID, groups and extras.
//
// The username alone is not enough to re-run the SubjectAccessReview. RBAC is usually bound to
// groups, not to names -- a cluster admin authenticating by client certificate is authorized
// through system:masters and an OIDC user through whatever groups their provider asserts -- so a
// review carrying only the username denies a principal who is in fact still fully authorized. The
// re-check has to ask the same question admission asked, with the same inputs.
const AuthorizedIdentityAnnotation = "terasky.com/authorized-identity"

// FieldManagerPrefix prefixes every field manager this operator uses, so a conflict can be
// recognised as internal (arbitrable by priority) rather than foreign (never forced).
const FieldManagerPrefix = "patch-operator/"
