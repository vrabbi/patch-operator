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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Contributor is the seam that lets one reconciler serve both ResourcePatch and
// ClusterResourcePatch. The scope split costs a validation table and a tracker-selection rule, not
// a second copy of the controller logic.
//
// +kubebuilder:object:generate=false
type Contributor interface {
	client.Object

	// GetPatchSpec returns the shared spec. Both kinds carry the identical shape.
	GetPatchSpec() *ResourcePatchSpec
	// GetPatchStatus returns the shared status.
	GetPatchStatus() *ResourcePatchStatus
	// IsClusterScoped reports whether this contributor may reach outside a single namespace.
	// It is what decides the tracker kind and what the validation webhook keys off.
	IsClusterScoped() bool
	// ContributorKind returns "ResourcePatch" or "ClusterResourcePatch", recorded on tracker
	// references so a promoted tracker can hold a mix of both.
	ContributorKind() string
}

// Tracker is the seam that lets one reconciler serve both SharedResource and
// ClusterSharedResource.
//
// +kubebuilder:object:generate=false
type Tracker interface {
	client.Object

	// GetTrackerSpec returns the shared spec.
	GetTrackerSpec() *SharedResourceSpec
	// GetTrackerStatus returns the shared status.
	GetTrackerStatus() *SharedResourceStatus
	// IsClusterScoped reports whether this tracker is the cluster-scoped variant.
	IsClusterScoped() bool
	// TrackerKind returns "SharedResource" or "ClusterSharedResource".
	TrackerKind() string
}

// Kind names, used where a string is recorded in status rather than a typed reference.
const (
	KindResourcePatch        = "ResourcePatch"
	KindClusterResourcePatch = "ClusterResourcePatch"
	KindSharedResource       = "SharedResource"
	KindClusterSharedResource = "ClusterSharedResource"
)

// --- ResourcePatch ---

var _ Contributor = &ResourcePatch{}

func (r *ResourcePatch) GetPatchSpec() *ResourcePatchSpec     { return &r.Spec }
func (r *ResourcePatch) GetPatchStatus() *ResourcePatchStatus { return &r.Status }
func (r *ResourcePatch) IsClusterScoped() bool                { return false }
func (r *ResourcePatch) ContributorKind() string              { return KindResourcePatch }

// --- ClusterResourcePatch ---

var _ Contributor = &ClusterResourcePatch{}

func (r *ClusterResourcePatch) GetPatchSpec() *ResourcePatchSpec     { return &r.Spec }
func (r *ClusterResourcePatch) GetPatchStatus() *ResourcePatchStatus { return &r.Status }
func (r *ClusterResourcePatch) IsClusterScoped() bool                { return true }
func (r *ClusterResourcePatch) ContributorKind() string              { return KindClusterResourcePatch }

// --- SharedResource ---

var _ Tracker = &SharedResource{}

func (s *SharedResource) GetTrackerSpec() *SharedResourceSpec     { return &s.Spec }
func (s *SharedResource) GetTrackerStatus() *SharedResourceStatus { return &s.Status }
func (s *SharedResource) IsClusterScoped() bool                   { return false }
func (s *SharedResource) TrackerKind() string                     { return KindSharedResource }

// --- ClusterSharedResource ---

var _ Tracker = &ClusterSharedResource{}

func (s *ClusterSharedResource) GetTrackerSpec() *SharedResourceSpec     { return &s.Spec }
func (s *ClusterSharedResource) GetTrackerStatus() *SharedResourceStatus { return &s.Status }
func (s *ClusterSharedResource) IsClusterScoped() bool                   { return true }
func (s *ClusterSharedResource) TrackerKind() string                     { return KindClusterSharedResource }

// ContributorRefOf builds a ContributorRef pinned by UID, so a recreated contributor of the same
// name is never mistaken for the original.
func ContributorRefOf(c Contributor) ContributorRef {
	return ContributorRef{
		Kind:      c.ContributorKind(),
		Name:      c.GetName(),
		Namespace: c.GetNamespace(),
		UID:       string(c.GetUID()),
	}
}

// SameContributor reports whether two refs identify the same object. UID is authoritative when
// both sides have one; the delete-safety rule in DESIGN.md 6.2 depends on this being exact.
func SameContributor(a, b ContributorRef) bool {
	if a.UID != "" && b.UID != "" {
		return a.UID == b.UID
	}
	return a.Kind == b.Kind && a.Name == b.Name && a.Namespace == b.Namespace
}

// String renders a ref as "kind/namespace/name", suitable for a conflict claimant list.
func (r ContributorRef) String() string {
	if r.Namespace == "" {
		return r.Kind + "/" + r.Name
	}
	return r.Kind + "/" + r.Namespace + "/" + r.Name
}

// IsFenced reports whether this tracker has been fenced by a promotion and must not write to its
// target. The fence is committed before the successor is created, which is what guarantees there
// is never a window with two writers.
func (s *SharedResourceStatus) IsFenced() bool {
	return s.PromotedTo != nil
}

// FindContributor returns the record for a ref, or nil.
func (s *SharedResourceStatus) FindContributor(ref ContributorRef) *ContributorStatus {
	for i := range s.Contributors {
		if SameContributor(s.Contributors[i].PatchRef, ref) {
			return &s.Contributors[i]
		}
	}
	return nil
}

// SetCondition upserts a condition, preserving LastTransitionTime when the status is unchanged.
func SetCondition(conds *[]metav1.Condition, c metav1.Condition) {
	if c.LastTransitionTime.IsZero() {
		c.LastTransitionTime = metav1.Now()
	}
	for i := range *conds {
		if (*conds)[i].Type != c.Type {
			continue
		}
		if (*conds)[i].Status == c.Status {
			c.LastTransitionTime = (*conds)[i].LastTransitionTime
		}
		(*conds)[i] = c
		return
	}
	*conds = append(*conds, c)
}

// GetCondition returns a condition by type, or nil.
func GetCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
