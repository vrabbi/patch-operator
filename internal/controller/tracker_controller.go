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
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/apply"
	"github.com/vrabbi/patch-operator/internal/impersonate"
	"github.com/vrabbi/patch-operator/internal/render"
	"github.com/vrabbi/patch-operator/internal/scope"
)

// TrackerReconciler is the only code path that writes to a target object.
//
// That exclusivity is the architecture (DESIGN.md 4.1): with one writer per target, writes are
// serialised through one work-queue key, apply order is a deterministic sort stable across
// restarts, conflicts are detectable before any write because all contributions are in hand, and
// the reference count is a list length evaluated at a single point.
//
// One implementation serves both SharedResource and ClusterSharedResource.
type TrackerReconciler[T patchv1alpha1.Tracker, L client.ObjectList] struct {
	Client client.Client
	Scheme *runtime.Scheme

	// New returns a fresh empty tracker of the reconciled kind.
	New func() T

	// Impersonation hands out the client each contributor's writes run as.
	Impersonation *impersonate.Factory
}

// Reconcile brings one target up to date with every contributor that claims it.
func (r *TrackerReconciler[T, L]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tracker := r.New()
	if err := r.Client.Get(ctx, req.NamespacedName, tracker); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	status := tracker.GetTrackerStatus()

	// A fenced tracker has been superseded by a promotion and must not write. The fence is
	// committed before the successor exists, which is what guarantees there is never a window with
	// two writers on one object.
	if status.IsFenced() {
		return r.finishPromotion(ctx, tracker)
	}

	if !tracker.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, tracker)
	}

	if !controllerutil.ContainsFinalizer(tracker, patchv1alpha1.TrackerFinalizer) {
		controllerutil.AddFinalizer(tracker, patchv1alpha1.TrackerFinalizer)
		if err := r.Client.Update(ctx, tracker); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Rebuild the contributor list from a live list rather than trusting status. Status is derived
	// state, so a missed watch event or a lost update self-heals here instead of corrupting the
	// reference count — and an incomplete count deletes objects that are still in use.
	contributors, err := r.listContributors(ctx, tracker)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(contributors) == 0 {
		return r.reconcileEmpty(ctx, tracker)
	}

	return r.reconcileContributions(ctx, tracker, contributors)
}

// contributorRecord pairs a live contributor with the tracker's record of it.
type contributorRecord struct {
	obj    patchv1alpha1.Contributor
	status patchv1alpha1.ContributorStatus
}

// listContributors finds every contributor claiming this tracker's target, across both kinds.
func (r *TrackerReconciler[T, L]) listContributors(
	ctx context.Context,
	tracker T,
) ([]contributorRecord, error) {
	ref := tracker.GetTrackerSpec().TargetRef
	key := scope.TargetKey{
		APIVersion: ref.APIVersion,
		Kind:       ref.Kind,
		Namespace:  ref.Namespace,
		Name:       ref.Name,
	}

	var out []contributorRecord
	existing := tracker.GetTrackerStatus()

	// Namespaced contributors. A namespaced tracker looks only in its own namespace, which keeps
	// its lookups cheap; a cluster tracker must look everywhere, since after a promotion it holds
	// namespaced contributors too.
	rpList := &patchv1alpha1.ResourcePatchList{}
	listOpts := []client.ListOption{client.MatchingFields{TargetKeyIndex: key.String()}}
	if !tracker.IsClusterScoped() {
		listOpts = append(listOpts, client.InNamespace(tracker.GetNamespace()))
	}
	if err := r.Client.List(ctx, rpList, listOpts...); err != nil {
		return nil, fmt.Errorf("listing ResourcePatches for %s: %w", key, err)
	}
	for i := range rpList.Items {
		c := &rpList.Items[i]
		out = append(out, contributorRecord{obj: c, status: recordFor(c, existing)})
	}

	// Cluster-scoped contributors only ever register with a cluster tracker: a namespaced tracker
	// cannot honestly count a contributor outside its namespace, which is why rule 3 of
	// DESIGN.md 3.5 promotes instead.
	if tracker.IsClusterScoped() {
		crpList := &patchv1alpha1.ClusterResourcePatchList{}
		if err := r.Client.List(ctx, crpList,
			client.MatchingFields{TargetKeyIndex: key.String()}); err != nil {
			return nil, fmt.Errorf("listing ClusterResourcePatches for %s: %w", key, err)
		}
		for i := range crpList.Items {
			c := &crpList.Items[i]
			out = append(out, contributorRecord{obj: c, status: recordFor(c, existing)})
		}
	}

	// Selector-mode contributors are not in the index, since their match set is not knowable from
	// the spec. Find them by their recorded registrations instead.
	selectorBound, err := r.listSelectorContributors(ctx, tracker, out)
	if err != nil {
		return nil, err
	}
	out = append(out, selectorBound...)

	records := make([]patchv1alpha1.ContributorStatus, 0, len(out))
	for _, c := range out {
		records = append(records, c.status)
	}
	sortContributors(records)

	byKey := map[string]patchv1alpha1.Contributor{}
	for _, c := range out {
		byKey[c.status.PatchRef.String()] = c.obj
	}
	sorted := make([]contributorRecord, 0, len(records))
	for _, rec := range records {
		if obj, ok := byKey[rec.PatchRef.String()]; ok {
			sorted = append(sorted, contributorRecord{obj: obj, status: rec})
		}
	}
	return sorted, nil
}

// listSelectorContributors finds Selector-mode contributors registered with this tracker, by
// reading their own status rather than the index.
func (r *TrackerReconciler[T, L]) listSelectorContributors(
	ctx context.Context,
	tracker T,
	already []contributorRecord,
) ([]contributorRecord, error) {
	seen := map[string]bool{}
	for _, c := range already {
		seen[c.status.PatchRef.String()] = true
	}

	trackerRef := patchv1alpha1.TrackerRef{
		Kind:      tracker.TrackerKind(),
		Name:      tracker.GetName(),
		Namespace: tracker.GetNamespace(),
	}
	existing := tracker.GetTrackerStatus()

	var out []contributorRecord

	add := func(c patchv1alpha1.Contributor) {
		if seen[patchv1alpha1.ContributorRefOf(c).String()] {
			return
		}
		if c.GetPatchSpec().Target.Mode != patchv1alpha1.TargetModeSelector {
			return
		}
		for _, ref := range c.GetPatchStatus().SharedResourceRefs {
			if ref == trackerRef {
				out = append(out, contributorRecord{obj: c, status: recordFor(c, existing)})
				return
			}
		}
	}

	rpList := &patchv1alpha1.ResourcePatchList{}
	opts := []client.ListOption{}
	if !tracker.IsClusterScoped() {
		opts = append(opts, client.InNamespace(tracker.GetNamespace()))
	}
	if err := r.Client.List(ctx, rpList, opts...); err != nil {
		return nil, err
	}
	for i := range rpList.Items {
		add(&rpList.Items[i])
	}

	if tracker.IsClusterScoped() {
		crpList := &patchv1alpha1.ClusterResourcePatchList{}
		if err := r.Client.List(ctx, crpList); err != nil {
			return nil, err
		}
		for i := range crpList.Items {
			add(&crpList.Items[i])
		}
	}
	return out, nil
}

// recordFor builds a contributor's record, carrying forward bookkeeping the tracker already holds.
//
// Carrying forward matters most for PriorValues: re-deriving them would capture this contributor's
// own value and make revert a no-op.
func recordFor(
	c patchv1alpha1.Contributor,
	existing *patchv1alpha1.SharedResourceStatus,
) patchv1alpha1.ContributorStatus {
	ref := patchv1alpha1.ContributorRefOf(c)
	created := c.GetCreationTimestamp()

	rec := patchv1alpha1.ContributorStatus{
		PatchRef:           ref,
		ObservedGeneration: c.GetGeneration(),
		Priority:           c.GetPatchSpec().Priority,
		CreationTimestamp:  &created,
	}
	if prev := existing.FindContributor(ref); prev != nil {
		rec.FieldManager = prev.FieldManager
		rec.LastAppliedHash = prev.LastAppliedHash
		rec.OwnedPaths = prev.OwnedPaths
		rec.PriorValues = prev.PriorValues
		rec.State = prev.State
	}
	if rec.FieldManager == "" {
		rec.FieldManager = apply.FieldManagerFor(c.ContributorKind(), c.GetNamespace(), c.GetName())
	}
	return rec
}

// reconcileContributions renders every contribution, resolves conflicts, and writes.
func (r *TrackerReconciler[T, L]) reconcileContributions(
	ctx context.Context,
	tracker T,
	contributors []contributorRecord,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	status := tracker.GetTrackerStatus()
	ref := tracker.GetTrackerSpec().TargetRef

	target := render.Target{
		APIVersion: ref.APIVersion,
		Kind:       ref.Kind,
		Name:       ref.Name,
		Namespace:  ref.Namespace,
	}

	// Releasing contributors first: withdrawing a departing contributor's fields before applying
	// the rest keeps a released path from being immediately re-set by a stale record.
	releasing, active := splitReleasing(contributors)
	for _, c := range releasing {
		if err := r.releaseContributor(ctx, tracker, c, target); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Re-list is unnecessary: the released contributors are dropped from the records below.
	records := make([]patchv1alpha1.ContributorStatus, 0, len(active))
	conflicts := []patchv1alpha1.ConflictStatus{}
	anyConflict := false
	targetExists := true

	rendered, renderErr := r.renderAll(active, target)
	if renderErr != nil {
		setTrackerCond(status, patchv1alpha1.ConditionSynced, metav1.ConditionFalse,
			patchv1alpha1.ReasonInvalidSpec, renderErr.Error())
		status.Phase = patchv1alpha1.TrackerPhaseWaiting
		return ctrl.Result{}, r.writeStatus(ctx, tracker, records, conflicts)
	}

	// Detect conflicts across all contributions before writing anything, which is only possible
	// because every contribution is in hand at once.
	pathClaims := detectPathConflicts(active, rendered)

	for i, c := range active {
		rec := c.status
		contribution := rendered[i]

		applier, err := r.applierFor(c.obj)
		if err != nil {
			rec.State = patchv1alpha1.ContributorStateConflicted
			records = append(records, rec)
			continue
		}

		// A path contested by a higher-priority contributor is not applied by the loser. Under
		// ConflictPolicy Fail nobody writes; under Priority the highest-priority claimant wins and
		// the loser is marked Superseded rather than quietly dropped.
		if loser, holder := isSupersededOn(c, active, pathClaims); loser {
			rec.State = patchv1alpha1.ContributorStateSuperseded
			records = append(records, rec)
			anyConflict = true
			conflicts = appendConflict(conflicts, pathClaims, c, holder)
			continue
		}

		previous, err := render.ParsePaths(rec.OwnedPaths)
		if err != nil {
			logger.Error(err, "stored owned paths are unparseable; treating the contributor as fresh",
				"contributor", rec.PatchRef.String())
			previous = nil
		}

		priors, err := decodePriorValues(rec.PriorValues)
		if err != nil {
			return ctrl.Result{}, err
		}

		base, err := baseFor(c.obj)
		if err != nil {
			rec.State = patchv1alpha1.ContributorStateConflicted
			records = append(records, rec)
			continue
		}

		spec := c.obj.GetPatchSpec()
		result, err := applier.Apply(ctx, apply.Request{
			Target:         target,
			Contribution:   contribution,
			FieldManager:   rec.FieldManager,
			ConflictPolicy: spec.Apply.ConflictPolicy,
			MergeKeys:      spec.Patch.MergeKeys,
			PreviousPaths:  previous,
			PriorValues:    priors,
			Base:           base,
			AllowCreate:    spec.Lifecycle.OnMissing == patchv1alpha1.OnMissingCreate,
		})

		switch {
		case apierrors.IsNotFound(err):
			// onMissing: Wait or Fail with an absent target. Not an error to retry hard.
			rec.State = ""
			targetExists = false
			records = append(records, rec)
			continue

		case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
			// A concurrent creator or writer. Requeue and converge.
			logger.V(1).Info("target changed under us; requeueing", "error", err.Error())
			records = append(records, rec)
			if statusErr := r.writeStatus(ctx, tracker, records, conflicts); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil

		case err != nil:
			rec.State = patchv1alpha1.ContributorStateConflicted
			records = append(records, rec)
			setTrackerCond(status, patchv1alpha1.ConditionSynced, metav1.ConditionFalse,
				patchv1alpha1.ReasonApplyFailed, err.Error())
			if statusErr := r.writeStatus(ctx, tracker, records, conflicts); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{}, err
		}

		if result.Conflict != nil {
			rec.State = patchv1alpha1.ContributorStateConflicted
			anyConflict = true
			conflicts = append(conflicts, patchv1alpha1.ConflictStatus{
				FieldPath: firstOr(result.Conflict.Fields, "<unknown>"),
				Claimants: []string{rec.PatchRef.String()},
				Holder:    firstOr(result.Conflict.Managers, "<unknown>"),
			})
			records = append(records, rec)
			continue
		}

		// The creator is recorded once and never overwritten. Only this contributor may ever
		// delete the target, and losing the record would quietly retire that protection.
		if result.Created && status.CreatorPatchRef == nil {
			created := rec.PatchRef
			status.CreatedByOperator = true
			status.CreatorPatchRef = &created
			if base != nil {
				if h, err := render.HashObject(base); err == nil {
					status.ObservedBaseHash = h
				}
			}
		}

		rec.State = patchv1alpha1.ContributorStateApplied
		rec.LastAppliedHash = contribution.Hash
		rec.OwnedPaths = render.PathStrings(result.OwnedPaths)
		if result.PriorValues != nil {
			encoded, err := encodePriorValues(result.PriorValues)
			if err != nil {
				return ctrl.Result{}, err
			}
			rec.PriorValues = encoded
		}
		if result.Live != nil {
			status.ObservedResourceVersion = result.Live.GetResourceVersion()
			if uid := string(result.Live.GetUID()); uid != "" {
				status.ObservedTargetUID = uid
			}
		}
		records = append(records, rec)
	}

	switch {
	case !targetExists:
		status.Phase = patchv1alpha1.TrackerPhaseWaiting
		setTrackerCond(status, patchv1alpha1.ConditionTargetFound, metav1.ConditionFalse,
			patchv1alpha1.ReasonTargetMissing, "the target does not exist and no contributor creates it")
	case anyConflict:
		status.Phase = patchv1alpha1.TrackerPhaseConflicted
		setTrackerCond(status, patchv1alpha1.ConditionConflict, metav1.ConditionTrue,
			patchv1alpha1.ReasonConflict, "contributors disagree on at least one field")
	default:
		status.Phase = patchv1alpha1.TrackerPhaseApplied
		setTrackerCond(status, patchv1alpha1.ConditionTargetFound, metav1.ConditionTrue, "Found", "")
		setTrackerCond(status, patchv1alpha1.ConditionConflict, metav1.ConditionFalse, "NoConflict", "")
		setTrackerCond(status, patchv1alpha1.ConditionApplied, metav1.ConditionTrue,
			patchv1alpha1.ReasonApplied, "")
	}
	setTrackerCond(status, patchv1alpha1.ConditionSynced, metav1.ConditionTrue, "ReconcileSuccess", "")

	return ctrl.Result{}, r.writeStatus(ctx, tracker, records, conflicts)
}

// renderAll renders every active contribution.
func (r *TrackerReconciler[T, L]) renderAll(
	active []contributorRecord,
	target render.Target,
) ([]*render.Contribution, error) {
	out := make([]*render.Contribution, 0, len(active))
	for _, c := range active {
		spec := c.obj.GetPatchSpec()
		contribution, err := render.Render(&spec.Patch, target)
		if err != nil {
			return nil, fmt.Errorf("rendering %s: %w", c.status.PatchRef.String(), err)
		}
		out = append(out, contribution)
	}
	return out, nil
}

// releaseContributor withdraws one contributor's fields, per its onRelease policy.
func (r *TrackerReconciler[T, L]) releaseContributor(
	ctx context.Context,
	tracker T,
	c contributorRecord,
	target render.Target,
) error {
	spec := c.obj.GetPatchSpec()

	if spec.Lifecycle.OnRelease == patchv1alpha1.OnReleaseOrphan {
		// Leave the fields exactly as they are.
		return nil
	}

	applier, err := r.applierFor(c.obj)
	if err != nil {
		return err
	}

	paths, err := render.ParsePaths(c.status.OwnedPaths)
	if err != nil {
		return fmt.Errorf("parsing owned paths for %s: %w", c.status.PatchRef.String(), err)
	}
	priors, err := decodePriorValues(c.status.PriorValues)
	if err != nil {
		return err
	}

	return applier.Revert(ctx, apply.RevertRequest{
		Target:       target,
		FieldManager: c.status.FieldManager,
		OwnedPaths:   paths,
		PriorValues:  priors,
	})
}

// reconcileEmpty handles the last contributor leaving.
func (r *TrackerReconciler[T, L]) reconcileEmpty(ctx context.Context, tracker T) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	status := tracker.GetTrackerStatus()

	// Nothing references this target any more. The object-level action was decided when the
	// creator released (see releaseAndMaybeDelete); by here the tracker's own job is done.
	if len(status.Contributors) == 0 && status.Phase != patchv1alpha1.TrackerPhaseReleasing {
		logger.V(1).Info("tracker has no contributors; deleting it")
		controllerutil.RemoveFinalizer(tracker, patchv1alpha1.TrackerFinalizer)
		if err := r.Client.Update(ctx, tracker); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if err := r.Client.Delete(ctx, tracker); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	status.Phase = patchv1alpha1.TrackerPhaseReleasing
	return ctrl.Result{}, r.writeStatus(ctx, tracker, nil, nil)
}

// reconcileDelete withdraws every remaining contributor and applies the object-level policy.
func (r *TrackerReconciler[T, L]) reconcileDelete(ctx context.Context, tracker T) (ctrl.Result, error) {
	status := tracker.GetTrackerStatus()
	ref := tracker.GetTrackerSpec().TargetRef
	target := render.Target{
		APIVersion: ref.APIVersion, Kind: ref.Kind, Name: ref.Name, Namespace: ref.Namespace,
	}

	contributors, err := r.listContributors(ctx, tracker)
	if err != nil {
		return ctrl.Result{}, err
	}
	for _, c := range contributors {
		if err := r.releaseContributor(ctx, tracker, c, target); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.maybeDeleteTarget(ctx, tracker, contributors, target); err != nil {
		return ctrl.Result{}, err
	}

	if controllerutil.RemoveFinalizer(tracker, patchv1alpha1.TrackerFinalizer) {
		if err := r.Client.Update(ctx, tracker); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	_ = status
	return ctrl.Result{}, nil
}

// maybeDeleteTarget deletes the target only when the design's two preconditions both hold.
//
// Without the creator check, any patch-only contributor could set onRelease: Delete, attach itself
// to a pre-existing production Deployment, and delete it on the way out. So: the operator must
// have created the object, and the request must come from the contributor that created it, matched
// by UID.
func (r *TrackerReconciler[T, L]) maybeDeleteTarget(
	ctx context.Context,
	tracker T,
	contributors []contributorRecord,
	target render.Target,
) error {
	status := tracker.GetTrackerStatus()

	if !status.CreatedByOperator || status.CreatorPatchRef == nil {
		return nil
	}

	wantsDelete := false
	for _, c := range contributors {
		if c.obj.GetPatchSpec().Lifecycle.OnRelease != patchv1alpha1.OnReleaseDelete {
			continue
		}
		if patchv1alpha1.SameContributor(c.status.PatchRef, *status.CreatorPatchRef) {
			wantsDelete = true
			break
		}
	}
	if !wantsDelete {
		return nil
	}

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(target.APIVersion)
	obj.SetKind(target.Kind)
	obj.SetName(target.Name)
	if target.Namespace != "" {
		obj.SetNamespace(target.Namespace)
	}
	if err := r.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting target %s: %w", target.Name, err)
	}
	return nil
}

// applierFor returns the applier for a contributor, writing as its impersonated identity.
func (r *TrackerReconciler[T, L]) applierFor(c patchv1alpha1.Contributor) (apply.Applier, error) {
	writer := r.Client
	if r.Impersonation != nil {
		impersonated, _, err := r.Impersonation.For(c)
		if err != nil {
			return nil, err
		}
		writer = impersonated
	}
	return apply.For(c.GetPatchSpec().Apply.Mode, writer)
}

// writeStatus persists the tracker's derived state.
func (r *TrackerReconciler[T, L]) writeStatus(
	ctx context.Context,
	tracker T,
	records []patchv1alpha1.ContributorStatus,
	conflicts []patchv1alpha1.ConflictStatus,
) error {
	status := tracker.GetTrackerStatus()
	if records != nil || len(status.Contributors) > 0 {
		status.Contributors = records
	}
	status.ContributorCount = int32(len(status.Contributors))
	status.Conflicts = conflicts
	return r.Client.Status().Update(ctx, tracker)
}

// SetupWithManager wires the controller. It watches both contributor kinds, since a cluster
// tracker holds both after a promotion.
func (r *TrackerReconciler[T, L]) SetupWithManager(mgr ctrl.Manager, forObj client.Object) error {
	name := fmt.Sprintf("tracker-%T", forObj)

	return ctrl.NewControllerManagedBy(mgr).
		Named(sanitizeControllerName(name)).
		For(forObj, builder.WithPredicates(generationOrFinalizerChanged())).
		Watches(&patchv1alpha1.ResourcePatch{},
			handler.EnqueueRequestsFromMapFunc(r.trackersOfContributor)).
		Watches(&patchv1alpha1.ClusterResourcePatch{},
			handler.EnqueueRequestsFromMapFunc(r.trackersOfContributor)).
		Complete(r)
}

// trackersOfContributor maps a contributor to the trackers it is registered with, filtered to the
// tracker kind this reconciler serves.
func (r *TrackerReconciler[T, L]) trackersOfContributor(_ context.Context, obj client.Object) []reconcile.Request {
	c, ok := obj.(patchv1alpha1.Contributor)
	if !ok {
		return nil
	}
	wantKind := r.New().TrackerKind()

	var out []reconcile.Request
	for _, ref := range c.GetPatchStatus().SharedResourceRefs {
		if ref.Kind != wantKind {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: ref.Namespace, Name: ref.Name,
		}})
	}

	// A contributor with no recorded registration yet still needs its tracker woken, or the first
	// reconcile after creation would wait for the resync period.
	if len(out) == 0 && c.GetPatchSpec().Target.Mode != patchv1alpha1.TargetModeSelector {
		key := scope.TargetKeyFor(c)
		kind := scope.TrackerKindFor(key, c.IsClusterScoped())
		if kind == wantKind {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: scope.TrackerNamespace(kind, key),
				Name:      scope.TrackerName(key),
			}})
		}
	}
	return out
}

// --- helpers ---

// splitReleasing separates contributors being deleted from active ones.
func splitReleasing(in []contributorRecord) (releasing, active []contributorRecord) {
	for _, c := range in {
		if !c.obj.GetDeletionTimestamp().IsZero() {
			releasing = append(releasing, c)
			continue
		}
		active = append(active, c)
	}
	return releasing, active
}

// detectPathConflicts maps each claimed path to the contributors claiming it with differing
// values.
//
// Same path with the same value is not a conflict — it is redundancy, and it is allowed, because
// two XRs asking for the same ingress class is normal.
func detectPathConflicts(
	active []contributorRecord,
	rendered []*render.Contribution,
) map[string][]int {
	type claim struct {
		idx  int
		hash string
	}
	claims := map[string][]claim{}

	for i, contribution := range rendered {
		for _, p := range contribution.Paths {
			value, _ := render.Get(contribution.Object, p)
			h, err := render.HashObject(value)
			if err != nil {
				h = fmt.Sprintf("%v", value)
			}
			key := p.String()
			claims[key] = append(claims[key], claim{idx: i, hash: h})
		}
	}

	out := map[string][]int{}
	for path, cs := range claims {
		if len(cs) < 2 {
			continue
		}
		distinct := map[string]bool{}
		for _, c := range cs {
			distinct[c.hash] = true
		}
		if len(distinct) < 2 {
			continue // redundant, not contested
		}
		idxs := make([]int, 0, len(cs))
		for _, c := range cs {
			idxs = append(idxs, c.idx)
		}
		out[path] = idxs
	}
	_ = active
	return out
}

// isSupersededOn reports whether c loses a contested path to a higher-priority contributor.
//
// Only ConflictPolicy Priority produces a loser. Under Fail nobody writes, so the conflict is
// reported by the applier instead and both contributors stay NotReady.
func isSupersededOn(
	c contributorRecord,
	active []contributorRecord,
	pathClaims map[string][]int,
) (bool, string) {
	if c.obj.GetPatchSpec().Apply.ConflictPolicy != patchv1alpha1.ConflictPolicyPriority {
		return false, ""
	}

	myIdx := -1
	for i := range active {
		if patchv1alpha1.SameContributor(active[i].status.PatchRef, c.status.PatchRef) {
			myIdx = i
			break
		}
	}
	if myIdx < 0 {
		return false, ""
	}

	for _, claimants := range pathClaims {
		mine := false
		for _, idx := range claimants {
			if idx == myIdx {
				mine = true
				break
			}
		}
		if !mine {
			continue
		}
		for _, idx := range claimants {
			if idx == myIdx {
				continue
			}
			other := active[idx]
			if other.status.Priority > c.status.Priority {
				return true, other.status.PatchRef.String()
			}
		}
	}
	return false, ""
}

func appendConflict(
	conflicts []patchv1alpha1.ConflictStatus,
	pathClaims map[string][]int,
	c contributorRecord,
	holder string,
) []patchv1alpha1.ConflictStatus {
	for path := range pathClaims {
		conflicts = append(conflicts, patchv1alpha1.ConflictStatus{
			FieldPath: path,
			Claimants: []string{c.status.PatchRef.String(), holder},
			Holder:    holder,
		})
		break
	}
	return conflicts
}

// baseFor decodes a contributor's base manifest, if it has one.
func baseFor(c patchv1alpha1.Contributor) (map[string]any, error) {
	spec := c.GetPatchSpec()
	if spec.Base == nil || len(spec.Base.Raw) == 0 {
		return nil, nil
	}
	return render.DecodeBase(spec.Base)
}

func encodePriorValues(in map[string]any) (*runtime.RawExtension, error) {
	if len(in) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encoding prior values: %w", err)
	}
	return &runtime.RawExtension{Raw: b}, nil
}

func decodePriorValues(in *runtime.RawExtension) (map[string]any, error) {
	if in == nil || len(in.Raw) == 0 {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(in.Raw, &out); err != nil {
		return nil, fmt.Errorf("decoding prior values: %w", err)
	}
	return out, nil
}

func firstOr(in []string, fallback string) string {
	if len(in) == 0 {
		return fallback
	}
	return in[0]
}
