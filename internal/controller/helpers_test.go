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

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/render"
	"github.com/vrabbi/patch-operator/internal/scope"
)

func ts(sec int) *metav1.Time {
	t := metav1.NewTime(time.Unix(int64(sec), 0))
	return &t
}

func rec(kind, name, uid string, priority int32, created *metav1.Time) patchv1alpha1.ContributorStatus {
	return patchv1alpha1.ContributorStatus{
		PatchRef:          patchv1alpha1.ContributorRef{Kind: kind, Name: name, UID: uid},
		Priority:          priority,
		CreationTimestamp: created,
	}
}

func names(in []patchv1alpha1.ContributorStatus) []string {
	out := make([]string, 0, len(in))
	for _, c := range in {
		out = append(out, c.PatchRef.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Apply order must be a deterministic sort: priority descending, then creation time, then UID.
func TestSortContributorsOrder(t *testing.T) {
	in := []patchv1alpha1.ContributorStatus{
		rec("ResourcePatch", "low", "uid-3", 50, ts(300)),
		rec("ResourcePatch", "high", "uid-1", 200, ts(100)),
		rec("ResourcePatch", "mid", "uid-2", 100, ts(200)),
	}
	sortContributors(in)
	if want := []string{"high", "mid", "low"}; !equalStrings(names(in), want) {
		t.Errorf("order = %v, want %v", names(in), want)
	}
}

func TestSortContributorsCreationTimeBreaksEqualPriority(t *testing.T) {
	in := []patchv1alpha1.ContributorStatus{
		rec("ResourcePatch", "later", "uid-b", 100, ts(200)),
		rec("ResourcePatch", "earlier", "uid-a", 100, ts(100)),
	}
	sortContributors(in)
	if want := []string{"earlier", "later"}; !equalStrings(names(in), want) {
		t.Errorf("order = %v, want %v", names(in), want)
	}
}

// The UID tiebreak is not decoration. Two contributors created in the same clock tick must sort
// identically on every replica and after every restart, or the target flaps as leadership moves.
func TestSortContributorsUIDBreaksIdenticalTimestamps(t *testing.T) {
	sameTime := ts(100)

	first := []patchv1alpha1.ContributorStatus{
		rec("ResourcePatch", "b", "uid-b", 100, sameTime),
		rec("ResourcePatch", "a", "uid-a", 100, sameTime),
	}
	second := []patchv1alpha1.ContributorStatus{
		rec("ResourcePatch", "a", "uid-a", 100, sameTime),
		rec("ResourcePatch", "b", "uid-b", 100, sameTime),
	}

	sortContributors(first)
	sortContributors(second)

	if !equalStrings(names(first), names(second)) {
		t.Errorf("the same set in different input order sorted differently: %v vs %v",
			names(first), names(second))
	}
	if want := []string{"a", "b"}; !equalStrings(names(first), want) {
		t.Errorf("order = %v, want %v", names(first), want)
	}
}

func TestSortContributorsHandlesMissingTimestamps(t *testing.T) {
	in := []patchv1alpha1.ContributorStatus{
		rec("ResourcePatch", "no-time", "uid-b", 100, nil),
		rec("ResourcePatch", "timed", "uid-a", 100, ts(100)),
	}
	sortContributors(in)
	// A record with a timestamp sorts before one without, and neither panics.
	if want := []string{"timed", "no-time"}; !equalStrings(names(in), want) {
		t.Errorf("order = %v, want %v", names(in), want)
	}

	both := []patchv1alpha1.ContributorStatus{
		rec("ResourcePatch", "b", "uid-b", 100, nil),
		rec("ResourcePatch", "a", "uid-a", 100, nil),
	}
	sortContributors(both)
	if want := []string{"a", "b"}; !equalStrings(names(both), want) {
		t.Errorf("with no timestamps the UID should decide: %v", names(both))
	}
}

// A mixed contributor set is what a tracker holds after a promotion, so the sort must handle both
// kinds without conflating same-named contributors.
func TestSortContributorsMixedKinds(t *testing.T) {
	in := []patchv1alpha1.ContributorStatus{
		rec(patchv1alpha1.KindResourcePatch, "same-name", "uid-ns", 100, ts(200)),
		rec(patchv1alpha1.KindClusterResourcePatch, "same-name", "uid-cluster", 200, ts(100)),
	}
	sortContributors(in)
	if in[0].PatchRef.Kind != patchv1alpha1.KindClusterResourcePatch {
		t.Errorf("higher priority should win regardless of kind: %v", in[0].PatchRef)
	}
}

func TestNewTracker(t *testing.T) {
	key := scope.TargetKey{APIVersion: "v1", Kind: "ConfigMap", Namespace: "team-a", Name: "shared"}

	ns, err := newTracker(patchv1alpha1.KindSharedResource, "t", "team-a", key)
	if err != nil {
		t.Fatal(err)
	}
	if ns.GetNamespace() != "team-a" || ns.IsClusterScoped() {
		t.Errorf("namespaced tracker wrong: ns=%q clusterScoped=%v", ns.GetNamespace(), ns.IsClusterScoped())
	}
	// The target namespace is recorded explicitly even on the namespaced tracker, so the reference
	// is complete when copied to a ClusterSharedResource during a promotion.
	if ns.GetTrackerSpec().TargetRef.Namespace != "team-a" {
		t.Error("targetRef.namespace should be recorded explicitly")
	}

	cl, err := newTracker(patchv1alpha1.KindClusterSharedResource, "t", "", key)
	if err != nil {
		t.Fatal(err)
	}
	if cl.GetNamespace() != "" || !cl.IsClusterScoped() {
		t.Errorf("cluster tracker wrong: ns=%q clusterScoped=%v", cl.GetNamespace(), cl.IsClusterScoped())
	}

	if _, err := newTracker("Nope", "t", "", key); err == nil {
		t.Error("an unknown tracker kind should be rejected")
	}
}

func TestEmptyTracker(t *testing.T) {
	for _, kind := range []string{
		patchv1alpha1.KindSharedResource,
		patchv1alpha1.KindClusterSharedResource,
	} {
		got, err := emptyTracker(kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if got.TrackerKind() != kind {
			t.Errorf("got kind %q, want %q", got.TrackerKind(), kind)
		}
	}
	if _, err := emptyTracker("Nope"); err == nil {
		t.Error("an unknown tracker kind should be rejected")
	}
}

// The plain filter drops the operator's own status writes, which would otherwise requeue forever,
// but must always pass deletion and finalizer changes -- a contributor stuck in Terminating with
// its fields still on the target is the failure this prevents.
func TestGenerationOrFinalizerChanged(t *testing.T) {
	p := generationOrFinalizerChanged()

	base := &patchv1alpha1.ResourcePatch{ObjectMeta: metav1.ObjectMeta{Generation: 1}}

	statusOnly := base.DeepCopy()
	statusOnly.Status.AppliedGeneration = 5
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: statusOnly}) {
		t.Error("a status-only update should be filtered out")
	}

	specChange := base.DeepCopy()
	specChange.Generation = 2
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: specChange}) {
		t.Error("a generation change must pass")
	}

	deleting := base.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: deleting}) {
		t.Error("a deletion timestamp appearing must pass")
	}

	finalized := base.DeepCopy()
	finalized.Finalizers = []string{patchv1alpha1.ContributorFinalizer}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: finalized}) {
		t.Error("a finalizer change must pass")
	}
}

// Trackers are status-driven, so a generation-only filter is wrong for them: the promotion fence is
// committed as a status update by the contributor controller, and dropping it leaves a fenced
// tracker that never retires and a promotion that stalls.
func TestTrackerChangedPassesTheFence(t *testing.T) {
	p := trackerChanged()

	base := &patchv1alpha1.SharedResource{ObjectMeta: metav1.ObjectMeta{Generation: 1}}

	fenced := base.DeepCopy()
	fenced.Status.PromotedTo = &patchv1alpha1.PromotionRef{Name: "successor"}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: fenced}) {
		t.Fatal("the fence appearing must wake the tracker controller")
	}

	adopted := fenced.DeepCopy()
	adopted.Status.PromotedTo.Adopted = true
	if !p.Update(event.UpdateEvent{ObjectOld: fenced, ObjectNew: adopted}) {
		t.Error("adoption being recorded must wake the tracker controller")
	}

	// A contributor count change is the tracker's other real signal.
	counted := base.DeepCopy()
	counted.Status.ContributorCount = 2
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: counted}) {
		t.Error("a contributor count change must pass")
	}

	// But an arbitrary status write must not, or the controller loops on its own output.
	noisy := base.DeepCopy()
	noisy.Status.ObservedResourceVersion = "999"
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: noisy}) {
		t.Error("an unrelated status write should be filtered out")
	}
}

func TestSanitizeControllerName(t *testing.T) {
	got := sanitizeControllerName("tracker-*v1alpha1.SharedResource")
	if got == "" || got != "v1alpha1-sharedresource" {
		t.Errorf("got %q, want v1alpha1-sharedresource", got)
	}
}

// Same path, same value is redundancy rather than a conflict -- two XRs asking for the same
// ingress class is normal, so it must not be reported as a fight.
func TestDetectPathConflictsIgnoresIdenticalValues(t *testing.T) {
	target := render.Target{APIVersion: "v1", Kind: "ConfigMap", Name: "shared", Namespace: "team-a"}
	same := &patchv1alpha1.PatchSpec{Value: &runtime.RawExtension{Raw: []byte(`{"data":{"k":"same"}}`)}}

	a, err := render.Render(same, target)
	if err != nil {
		t.Fatal(err)
	}
	b, err := render.Render(same, target)
	if err != nil {
		t.Fatal(err)
	}

	if got := detectPathConflicts(nil, []*render.Contribution{a, b}); len(got) != 0 {
		t.Errorf("identical values were reported as a conflict: %v", got)
	}
}

func TestDetectPathConflictsFindsDifferingValues(t *testing.T) {
	target := render.Target{APIVersion: "v1", Kind: "ConfigMap", Name: "shared", Namespace: "team-a"}

	a, err := render.Render(&patchv1alpha1.PatchSpec{
		Value: &runtime.RawExtension{Raw: []byte(`{"data":{"k":"from-a"}}`)}}, target)
	if err != nil {
		t.Fatal(err)
	}
	b, err := render.Render(&patchv1alpha1.PatchSpec{
		Value: &runtime.RawExtension{Raw: []byte(`{"data":{"k":"from-b"}}`)}}, target)
	if err != nil {
		t.Fatal(err)
	}

	got := detectPathConflicts(nil, []*render.Contribution{a, b})
	if len(got) != 1 {
		t.Fatalf("expected one contested path, got %v", got)
	}
	if claimants, ok := got["data.k"]; !ok || len(claimants) != 2 {
		t.Errorf("expected both contributors as claimants of data.k, got %v", got)
	}
}

// Disjoint contributions are the normal case and must never register as a conflict.
func TestDetectPathConflictsIgnoresDisjointPaths(t *testing.T) {
	target := render.Target{APIVersion: "v1", Kind: "ConfigMap", Name: "shared", Namespace: "team-a"}

	a, _ := render.Render(&patchv1alpha1.PatchSpec{
		Value: &runtime.RawExtension{Raw: []byte(`{"data":{"key-a":"a"}}`)}}, target)
	b, _ := render.Render(&patchv1alpha1.PatchSpec{
		Value: &runtime.RawExtension{Raw: []byte(`{"data":{"key-b":"b"}}`)}}, target)

	if got := detectPathConflicts(nil, []*render.Contribution{a, b}); len(got) != 0 {
		t.Errorf("disjoint contributions were reported as a conflict: %v", got)
	}
}

func TestFirstOr(t *testing.T) {
	if got := firstOr(nil, "fallback"); got != "fallback" {
		t.Errorf("got %q", got)
	}
	if got := firstOr([]string{"a", "b"}, "fallback"); got != "a" {
		t.Errorf("got %q", got)
	}
}

func TestPriorValuesRoundTrip(t *testing.T) {
	in := map[string]any{"data.k": "original", "spec.replicas": float64(3)}

	encoded, err := encodePriorValues(in)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodePriorValues(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded["data.k"] != "original" || decoded["spec.replicas"] != float64(3) {
		t.Errorf("round trip lost data: %#v", decoded)
	}

	// Empty encodes to nil, so an unused field is absent from status rather than an empty object.
	if got, err := encodePriorValues(nil); err != nil || got != nil {
		t.Errorf("empty should encode to nil: %v %v", got, err)
	}
	if got, err := decodePriorValues(nil); err != nil || len(got) != 0 {
		t.Errorf("nil should decode to an empty map: %v %v", got, err)
	}
	if _, err := decodePriorValues(&runtime.RawExtension{Raw: []byte(`{`)}); err == nil {
		t.Error("malformed prior values should error rather than be silently dropped")
	}
}

func TestBaseFor(t *testing.T) {
	rp := &patchv1alpha1.ResourcePatch{}
	got, err := baseFor(rp)
	if err != nil || got != nil {
		t.Errorf("a contributor with no base should yield nil: %v %v", got, err)
	}

	rp.Spec.Base = &runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap"}`)}
	got, err = baseFor(rp)
	if err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "ConfigMap" {
		t.Errorf("got %#v", got)
	}
}

// splitReleasing must put a terminating contributor on the releasing side, since its fields have to
// be withdrawn before the rest are applied.
func TestSplitReleasing(t *testing.T) {
	now := metav1.Now()
	live := &patchv1alpha1.ResourcePatch{ObjectMeta: metav1.ObjectMeta{Name: "live"}}
	dying := &patchv1alpha1.ResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: "dying", DeletionTimestamp: &now,
			Finalizers: []string{patchv1alpha1.ContributorFinalizer}},
	}

	releasing, active := splitReleasing([]contributorRecord{
		{obj: live, status: rec("ResourcePatch", "live", "uid-1", 100, nil)},
		{obj: dying, status: rec("ResourcePatch", "dying", "uid-2", 100, nil)},
	})

	if len(releasing) != 1 || releasing[0].status.PatchRef.Name != "dying" {
		t.Errorf("releasing = %v", releasing)
	}
	if len(active) != 1 || active[0].status.PatchRef.Name != "live" {
		t.Errorf("active = %v", active)
	}
}

// adoptStatus carries forward exactly the state a successor cannot re-derive, and must not clobber
// what the successor already computed.
func TestAdoptStatusCarriesProvenanceAndPriorValues(t *testing.T) {
	creator := patchv1alpha1.ContributorRef{
		Kind: patchv1alpha1.KindResourcePatch, Namespace: "team-a", Name: "creator", UID: "uid-1",
	}
	src := &patchv1alpha1.SharedResourceStatus{
		CreatedByOperator: true,
		CreatorPatchRef:   &creator,
		ObservedBaseHash:  "sha256:base",
		ObservedTargetUID: "uid-target",
		Contributors: []patchv1alpha1.ContributorStatus{{
			PatchRef:    creator,
			OwnedPaths:  []string{"data.k"},
			PriorValues: &runtime.RawExtension{Raw: []byte(`{"data.k":"original"}`)},
		}},
	}

	// The successor already computed its own contributor list, without the revert bookkeeping.
	dst := &patchv1alpha1.SharedResourceStatus{
		Contributors: []patchv1alpha1.ContributorStatus{{PatchRef: creator}},
	}

	adoptStatus(dst, src, "team-a", "old-tracker")

	if !dst.CreatedByOperator {
		t.Error("createdByOperator was not carried over")
	}
	if dst.CreatorPatchRef == nil || dst.CreatorPatchRef.UID != "uid-1" {
		t.Fatal("creatorPatchRef was not carried over; delete-safety would no longer hold")
	}
	if dst.ObservedBaseHash != "sha256:base" || dst.ObservedTargetUID != "uid-target" {
		t.Errorf("observed state not carried over: %+v", dst)
	}

	// The existing record keeps its identity but gains the bookkeeping it lacked.
	if len(dst.Contributors) != 1 {
		t.Fatalf("the successor's contributor list was clobbered: %v", dst.Contributors)
	}
	got := dst.Contributors[0]
	if got.PriorValues == nil {
		t.Error("priorValues were not filled in; revert would delete instead of restore")
	}
	// Recorded values imply the capture happened, even on a predecessor written before the flag
	// existed. Without carrying that, the successor re-captures and revert becomes a no-op.
	if !got.PriorValuesCaptured {
		t.Error("priorValuesCaptured was not carried over; the successor would re-capture")
	}
	if len(got.OwnedPaths) != 1 || got.OwnedPaths[0] != "data.k" {
		t.Errorf("ownedPaths not filled in: %v", got.OwnedPaths)
	}

	// The successor is never itself fenced: promotion is one-way.
	if dst.PromotedTo != nil {
		t.Error("the successor must not be fenced")
	}

	// A predecessor whose contributor's paths pre-existed nowhere captured no values, but the
	// capture still happened. That fact is the thing the successor must not lose.
	emptySrc := &patchv1alpha1.SharedResourceStatus{
		Contributors: []patchv1alpha1.ContributorStatus{{
			PatchRef:            creator,
			OwnedPaths:          []string{"data.k"},
			PriorValuesCaptured: true,
		}},
	}
	emptyDst := &patchv1alpha1.SharedResourceStatus{
		Contributors: []patchv1alpha1.ContributorStatus{{PatchRef: creator}},
	}
	adoptStatus(emptyDst, emptySrc, "team-b", "old-tracker")
	if !emptyDst.Contributors[0].PriorValuesCaptured {
		t.Error("an empty-but-complete capture was not carried over")
	}
	if dst.ContributorCount != 1 {
		t.Errorf("contributorCount = %d, want 1", dst.ContributorCount)
	}
}

// A contributor the successor does not know about must be added, not dropped.
func TestAdoptStatusAddsUnknownContributors(t *testing.T) {
	ref := patchv1alpha1.ContributorRef{
		Kind: patchv1alpha1.KindResourcePatch, Namespace: "team-a", Name: "forgotten", UID: "uid-9",
	}
	src := &patchv1alpha1.SharedResourceStatus{
		Contributors: []patchv1alpha1.ContributorStatus{{PatchRef: ref, OwnedPaths: []string{"data.x"}}},
	}
	dst := &patchv1alpha1.SharedResourceStatus{}

	adoptStatus(dst, src, "team-a", "old")

	if len(dst.Contributors) != 1 || dst.Contributors[0].PatchRef.UID != "uid-9" {
		t.Errorf("an unknown contributor was dropped: %v", dst.Contributors)
	}
}

// A successor that already recorded provenance must keep its own: adoption fills gaps, it does not
// overwrite.
func TestAdoptStatusDoesNotOverwriteExistingProvenance(t *testing.T) {
	theirs := patchv1alpha1.ContributorRef{Kind: patchv1alpha1.KindResourcePatch, Name: "theirs", UID: "uid-a"}
	ours := patchv1alpha1.ContributorRef{Kind: patchv1alpha1.KindResourcePatch, Name: "ours", UID: "uid-b"}

	src := &patchv1alpha1.SharedResourceStatus{CreatedByOperator: true, CreatorPatchRef: &theirs}
	dst := &patchv1alpha1.SharedResourceStatus{CreatedByOperator: true, CreatorPatchRef: &ours}

	adoptStatus(dst, src, "team-a", "old")

	if dst.CreatorPatchRef.UID != "uid-b" {
		t.Errorf("adoption overwrote the successor's own creator: %+v", dst.CreatorPatchRef)
	}
}
