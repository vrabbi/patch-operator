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

// TrackerPhase is the tracker's overall state.
// +kubebuilder:validation:Enum=Waiting;Applied;Conflicted;Releasing;Promoting
type TrackerPhase string

const (
	// TrackerPhaseWaiting means the target does not exist and no contributor will create it.
	TrackerPhaseWaiting TrackerPhase = "Waiting"
	// TrackerPhaseApplied means every contributor's contribution is on the target.
	TrackerPhaseApplied TrackerPhase = "Applied"
	// TrackerPhaseConflicted means at least one field conflict is unresolved.
	TrackerPhaseConflicted TrackerPhase = "Conflicted"
	// TrackerPhaseReleasing means at least one contributor is being withdrawn.
	TrackerPhaseReleasing TrackerPhase = "Releasing"
	// TrackerPhasePromoting fences a namespaced tracker: it writes nothing to the target while a
	// ClusterSharedResource takes over. See DESIGN.md 6.3.
	TrackerPhasePromoting TrackerPhase = "Promoting"
)

// ContributorState is one contributor's state on a tracker.
// +kubebuilder:validation:Enum=Applied;Superseded;Conflicted;Releasing
type ContributorState string

const (
	// ContributorStateApplied means this contributor's fields are on the target.
	ContributorStateApplied ContributorState = "Applied"
	// ContributorStateSuperseded means a higher-priority contributor won a conflict, so this
	// contributor's fields are genuinely absent. Reported as Ready=False, not hidden.
	ContributorStateSuperseded ContributorState = "Superseded"
	// ContributorStateConflicted means this contributor is party to an unresolved conflict.
	ContributorStateConflicted ContributorState = "Conflicted"
	// ContributorStateReleasing means this contributor's fields are being withdrawn.
	ContributorStateReleasing ContributorState = "Releasing"
)

// ContributorRef identifies a contributor. Kind is carried because after a promotion one tracker
// holds a mix of ResourcePatch and ClusterResourcePatch contributors.
type ContributorRef struct {
	// Kind is ResourcePatch or ClusterResourcePatch.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// UID pins the identity, so a recreated contributor of the same name is not mistaken for the
	// original. The delete-safety rule in DESIGN.md 6.2 matches on this.
	// +optional
	UID string `json:"uid,omitempty"`
}

// ContributorStatus is the tracker's record of one contributor.
type ContributorStatus struct {
	// PatchRef identifies the contributor.
	PatchRef ContributorRef `json:"patchRef"`

	// ObservedGeneration is the contributor spec generation this record reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Priority is copied from the contributor for deterministic ordering without a second read.
	// +optional
	Priority int32 `json:"priority,omitempty"`

	// FieldManager is the SSA field manager this contributor owns its fields under.
	// +optional
	FieldManager string `json:"fieldManager,omitempty"`

	// State is this contributor's state on the target.
	// +optional
	State ContributorState `json:"state,omitempty"`

	// LastAppliedHash lets the tracker skip an apply when nothing has changed.
	// +optional
	LastAppliedHash string `json:"lastAppliedHash,omitempty"`

	// OwnedPaths are the field paths this contributor set. ClientSideApply bookkeeping.
	// +optional
	OwnedPaths []string `json:"ownedPaths,omitempty"`

	// PriorValues holds what those paths contained before this contributor first touched them,
	// for paths that already existed. ClientSideApply revert restores these rather than deleting
	// the field, which is the correct behaviour when a contributor overwrote a pre-existing
	// setting. Losing this on promotion would silently break revert.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	PriorValues *runtime.RawExtension `json:"priorValues,omitempty"`

	// CreationTimestamp is copied for the deterministic sort, which orders by
	// (priority desc, creationTimestamp asc, uid asc).
	// +optional
	CreationTimestamp *metav1.Time `json:"creationTimestamp,omitempty"`
}

// ConflictStatus records one field-path conflict and who is party to it.
type ConflictStatus struct {
	// FieldPath is the contested path.
	FieldPath string `json:"fieldPath"`
	// Claimants are the contributors claiming it, qualified by kind since a namespaced and a
	// cluster-scoped contributor can be party to the same conflict after a promotion.
	// +optional
	Claimants []string `json:"claimants,omitempty"`
	// Holder is the contributor or foreign field manager that currently owns it.
	// +optional
	Holder string `json:"holder,omitempty"`
}

// PromotionRef points at the ClusterSharedResource that took over from a namespaced tracker.
type PromotionRef struct {
	Name string `json:"name"`
	// Adopted is set once the ClusterSharedResource has confirmed it holds the copied state. The
	// namespaced tracker's finalizer is released only after this.
	// +optional
	Adopted bool `json:"adopted,omitempty"`
}

// SharedResourceSpec identifies the target a tracker owns.
type SharedResourceSpec struct {
	// TargetRef identifies the target object.
	TargetRef TrackerTargetRef `json:"targetRef"`
}

// TrackerTargetRef is the concrete, fully resolved target of a tracker.
type TrackerTargetRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	// Namespace is empty for a cluster-scoped target. On a namespaced SharedResource it is
	// implicit in the tracker's own namespace but recorded here too, so the reference is complete
	// when copied to a ClusterSharedResource during promotion.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// SharedResourceStatus is the status shared by SharedResource and ClusterSharedResource.
type SharedResourceStatus struct {
	// Phase is the tracker's overall state.
	// +optional
	Phase TrackerPhase `json:"phase,omitempty"`

	// Conditions follow the same shape as contributors'.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// PromotedTo fences this tracker and names its successor. Only ever set on a namespaced
	// SharedResource; promotion is one-way and a ClusterSharedResource is never demoted.
	// +optional
	PromotedTo *PromotionRef `json:"promotedTo,omitempty"`

	// CreatedByOperator records that this operator created the target, which is half of the
	// precondition for ever deleting it.
	// +optional
	CreatedByOperator bool `json:"createdByOperator,omitempty"`

	// CreatorPatchRef is the contributor that actually created the target. Only it may request
	// deletion. Losing this on promotion would silently break that guarantee.
	// +optional
	CreatorPatchRef *ContributorRef `json:"creatorPatchRef,omitempty"`

	// ObservedBaseHash is the hash of the base the target was created from, used to detect a
	// second contributor supplying a different base.
	// +optional
	ObservedBaseHash string `json:"observedBaseHash,omitempty"`

	// ObservedTargetUID pins the target's identity. A change means the target was replaced out of
	// band, which invalidates all ClientSideApply bookkeeping.
	// +optional
	ObservedTargetUID string `json:"observedTargetUID,omitempty"`

	// ObservedResourceVersion is the target's resourceVersion after the last successful write,
	// used to recognise and drop the echo of that write.
	// +optional
	ObservedResourceVersion string `json:"observedResourceVersion,omitempty"`

	// Contributors is the reference count. It is derived state, rebuilt from a live list on every
	// reconcile, so a missed event self-heals rather than corrupting the count.
	// +optional
	Contributors []ContributorStatus `json:"contributors,omitempty"`

	// ContributorCount is len(Contributors), surfaced as its own field only so it can be a printer
	// column: "who is writing to this object?" should be answerable in one command.
	// +optional
	ContributorCount int32 `json:"contributorCount,omitempty"`

	// Conflicts records unresolved field-path conflicts.
	// +optional
	Conflicts []ConflictStatus `json:"conflicts,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sr
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.kind`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Contributors",type=integer,JSONPath=`.status.contributorCount`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SharedResource tracks one namespaced target whose contributors all live in this namespace.
//
// Operator-owned: users do not create these. It is the only writer to its target, which is what
// makes writes serialised, apply order deterministic, and the reference count evaluable at a
// single point.
type SharedResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SharedResourceSpec   `json:"spec,omitempty"`
	Status SharedResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SharedResourceList contains a list of SharedResource.
type SharedResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SharedResource `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=csr
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.kind`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.targetRef.namespace`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Contributors",type=integer,JSONPath=`.status.contributorCount`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterSharedResource tracks every cluster-scoped target, and every namespaced target that has
// at least one ClusterResourcePatch contributor.
//
// It has no promotedTo: promotion is one-way and a ClusterSharedResource is never demoted.
type ClusterSharedResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SharedResourceSpec   `json:"spec,omitempty"`
	Status SharedResourceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterSharedResourceList contains a list of ClusterSharedResource.
type ClusterSharedResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterSharedResource `json:"items"`
}
