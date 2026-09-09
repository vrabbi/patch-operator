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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The scope split must not become two APIs. These assertions pin the seam: both contributor kinds
// expose the identical spec, and only their reach differs.
func TestContributorSeam(t *testing.T) {
	rp := &ResourcePatch{}
	crp := &ClusterResourcePatch{}

	if rp.IsClusterScoped() {
		t.Error("ResourcePatch must not be cluster-scoped")
	}
	if !crp.IsClusterScoped() {
		t.Error("ClusterResourcePatch must be cluster-scoped")
	}
	if rp.ContributorKind() != KindResourcePatch || crp.ContributorKind() != KindClusterResourcePatch {
		t.Errorf("kinds = %q, %q", rp.ContributorKind(), crp.ContributorKind())
	}

	// Both must hand back the same spec type, which is what lets one reconciler serve both.
	rp.Spec.Priority = 7
	crp.Spec.Priority = 7
	if rp.GetPatchSpec().Priority != 7 || crp.GetPatchSpec().Priority != 7 {
		t.Error("GetPatchSpec did not expose the underlying spec")
	}
	rp.GetPatchStatus().AppliedGeneration = 3
	if rp.Status.AppliedGeneration != 3 {
		t.Error("GetPatchStatus returned a copy rather than a pointer")
	}
}

func TestTrackerSeam(t *testing.T) {
	sr := &SharedResource{}
	csr := &ClusterSharedResource{}

	if sr.IsClusterScoped() {
		t.Error("SharedResource must not be cluster-scoped")
	}
	if !csr.IsClusterScoped() {
		t.Error("ClusterSharedResource must be cluster-scoped")
	}
	if sr.TrackerKind() != KindSharedResource || csr.TrackerKind() != KindClusterSharedResource {
		t.Errorf("kinds = %q, %q", sr.TrackerKind(), csr.TrackerKind())
	}

	sr.GetTrackerStatus().Phase = TrackerPhaseApplied
	if sr.Status.Phase != TrackerPhaseApplied {
		t.Error("GetTrackerStatus returned a copy rather than a pointer")
	}
	csr.GetTrackerSpec().TargetRef.Name = "x"
	if csr.Spec.TargetRef.Name != "x" {
		t.Error("GetTrackerSpec returned a copy rather than a pointer")
	}
}

// The delete-safety rule -- only the contributor that created a target may delete it -- matches on
// this function. A recreated contributor reusing a name must not inherit the creator's authority,
// so UID has to be authoritative whenever both sides carry one.
func TestSameContributorUIDIsAuthoritative(t *testing.T) {
	original := ContributorRef{Kind: KindResourcePatch, Namespace: "team-a", Name: "checkout", UID: "uid-1"}
	recreated := ContributorRef{Kind: KindResourcePatch, Namespace: "team-a", Name: "checkout", UID: "uid-2"}

	if SameContributor(original, recreated) {
		t.Error("a recreated contributor with the same name was treated as the original")
	}
	if !SameContributor(original, original) {
		t.Error("a ref should match itself")
	}

	// With a UID missing on either side, fall back to identity by kind/namespace/name.
	noUID := ContributorRef{Kind: KindResourcePatch, Namespace: "team-a", Name: "checkout"}
	if !SameContributor(original, noUID) {
		t.Error("expected the name-based fallback to match when a UID is absent")
	}

	// A same-named contributor of the *other* kind is a different object. After a promotion one
	// tracker holds both kinds, so this distinction is load-bearing.
	otherKind := ContributorRef{Kind: KindClusterResourcePatch, Namespace: "team-a", Name: "checkout"}
	if SameContributor(noUID, otherKind) {
		t.Error("contributors of different kinds must not be conflated")
	}
}

func TestContributorRefOf(t *testing.T) {
	rp := &ResourcePatch{}
	rp.Name = "checkout"
	rp.Namespace = "team-a"
	rp.UID = "uid-1"

	ref := ContributorRefOf(rp)
	if ref.Kind != KindResourcePatch || ref.Name != "checkout" || ref.Namespace != "team-a" || ref.UID != "uid-1" {
		t.Errorf("ref = %+v", ref)
	}

	crp := &ClusterResourcePatch{}
	crp.Name = "platform-rule"
	crp.UID = "uid-2"
	cref := ContributorRefOf(crp)
	if cref.Namespace != "" {
		t.Errorf("a cluster-scoped contributor should have no namespace: %+v", cref)
	}
}

func TestContributorRefString(t *testing.T) {
	namespaced := ContributorRef{Kind: KindResourcePatch, Namespace: "team-a", Name: "checkout"}
	if got := namespaced.String(); got != "ResourcePatch/team-a/checkout" {
		t.Errorf("got %q", got)
	}
	clusterScoped := ContributorRef{Kind: KindClusterResourcePatch, Name: "platform-rule"}
	if got := clusterScoped.String(); got != "ClusterResourcePatch/platform-rule" {
		t.Errorf("got %q", got)
	}
}

// The fence is what guarantees a promotion never leaves two trackers able to write. Its predicate
// must key on promotedTo being set at all, not on the phase, since the phase is cosmetic status
// that could be overwritten by a later reconcile.
func TestIsFenced(t *testing.T) {
	s := &SharedResourceStatus{}
	if s.IsFenced() {
		t.Error("a fresh tracker must not be fenced")
	}

	s.PromotedTo = &PromotionRef{Name: "csr-x"}
	if !s.IsFenced() {
		t.Error("setting promotedTo must fence the tracker")
	}

	// Still fenced after adoption completes: the tracker is being torn down, not resumed.
	s.PromotedTo.Adopted = true
	if !s.IsFenced() {
		t.Error("an adopted promotion must keep the tracker fenced")
	}

	// A phase alone must not fence, or a status overwrite could silently unfence a tracker
	// mid-promotion and produce two writers.
	unfenced := &SharedResourceStatus{Phase: TrackerPhasePromoting}
	if unfenced.IsFenced() {
		t.Error("the phase alone must not be treated as a fence")
	}
}

func TestFindContributor(t *testing.T) {
	a := ContributorRef{Kind: KindResourcePatch, Namespace: "team-a", Name: "a", UID: "uid-a"}
	b := ContributorRef{Kind: KindClusterResourcePatch, Name: "b", UID: "uid-b"}

	s := &SharedResourceStatus{Contributors: []ContributorStatus{
		{PatchRef: a, Priority: 100},
		{PatchRef: b, Priority: 200},
	}}

	got := s.FindContributor(b)
	if got == nil || got.Priority != 200 {
		t.Fatalf("did not find the cluster-scoped contributor: %+v", got)
	}
	// The returned pointer must alias the slice element, so callers can mutate state in place.
	got.State = ContributorStateApplied
	if s.Contributors[1].State != ContributorStateApplied {
		t.Error("FindContributor returned a copy")
	}

	missing := ContributorRef{Kind: KindResourcePatch, Name: "nope", UID: "uid-x"}
	if s.FindContributor(missing) != nil {
		t.Error("expected nil for an unknown contributor")
	}
}

func TestSetConditionUpsertsAndPreservesTransitionTime(t *testing.T) {
	var conds []metav1.Condition

	SetCondition(&conds, metav1.Condition{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonWaitingForTarget})
	if len(conds) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(conds))
	}
	first := conds[0].LastTransitionTime
	if first.IsZero() {
		t.Fatal("LastTransitionTime should be defaulted")
	}

	// Same status: the transition time must be preserved, or every reconcile would look like a
	// state change to anyone watching.
	time.Sleep(2 * time.Millisecond)
	SetCondition(&conds, metav1.Condition{Type: ConditionReady, Status: metav1.ConditionFalse, Reason: ReasonTargetMissing})
	if len(conds) != 1 {
		t.Fatalf("upsert added a duplicate: %d conditions", len(conds))
	}
	if !conds[0].LastTransitionTime.Equal(&first) {
		t.Error("LastTransitionTime changed although the status did not")
	}
	if conds[0].Reason != ReasonTargetMissing {
		t.Error("the reason was not updated")
	}

	// Changed status: the transition time must move.
	SetCondition(&conds, metav1.Condition{
		Type:               ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonApplied,
		LastTransitionTime: metav1.NewTime(time.Now().Add(time.Second)),
	})
	if conds[0].LastTransitionTime.Equal(&first) {
		t.Error("LastTransitionTime should move when the status changes")
	}

	// A second type appends rather than replacing.
	SetCondition(&conds, metav1.Condition{Type: ConditionSynced, Status: metav1.ConditionTrue})
	if len(conds) != 2 {
		t.Errorf("expected 2 conditions, got %d", len(conds))
	}
}

func TestGetCondition(t *testing.T) {
	conds := []metav1.Condition{
		{Type: ConditionReady, Status: metav1.ConditionTrue},
		{Type: ConditionConflict, Status: metav1.ConditionFalse},
	}
	if got := GetCondition(conds, ConditionConflict); got == nil || got.Status != metav1.ConditionFalse {
		t.Errorf("got %+v", got)
	}
	if GetCondition(conds, "Nope") != nil {
		t.Error("expected nil for an unknown condition type")
	}
	if GetCondition(nil, ConditionReady) != nil {
		t.Error("expected nil for a nil slice")
	}
}

// The field manager prefix is what distinguishes an internal conflict, which priority may
// arbitrate, from a foreign one, which must never be forced. If it were empty, every foreign
// manager would look like ours.
func TestFieldManagerPrefixIsNotEmpty(t *testing.T) {
	if FieldManagerPrefix == "" {
		t.Fatal("FieldManagerPrefix must not be empty")
	}
}
