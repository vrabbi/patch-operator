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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/scope"
)

// promoteIfNeeded migrates a namespaced tracker to a ClusterSharedResource because a
// ClusterResourcePatch has joined its target.
//
// Rule 3 of DESIGN.md 3.5: a namespaced tracker cannot count a contributor outside its namespace,
// and an incomplete reference count deletes objects that are still in use. So the tracker kind has
// to change — and the migration must never leave two trackers able to write to one object.
//
// The protocol, in order, and the order is the whole safety argument:
//
//  1. Fence the namespaced tracker by committing status.promotedTo. From that commit it writes
//     nothing. This lands BEFORE the successor exists.
//  2. Create the ClusterSharedResource, copying the contributor list with its ownedPaths and
//     priorValues, plus observedTargetUID, observedResourceVersion, createdByOperator,
//     creatorPatchRef and observedBaseHash.
//  3. Mark the promotion adopted.
//  4. Only then release the namespaced tracker's finalizer and delete it.
//
// Every step is idempotent, so a crash resumes rather than corrupts: a fenced tracker with no
// successor unfences and retries; a fenced tracker whose successor exists resumes at step 3.
func (r *ContributorReconciler[T, L]) promoteIfNeeded(ctx context.Context, key scope.TargetKey) error {
	logger := log.FromContext(ctx)

	trackerName := scope.TrackerName(key)
	namespacedKey := client.ObjectKey{Namespace: key.Namespace, Name: trackerName}

	// Step 1: fence, before the successor exists.
	//
	// Read uncached and retry on conflict. A stale read here would write the fence against an old
	// resourceVersion, the update would fail, and the tracker would be left unfenced while the
	// successor was being created -- which is precisely the two-writer window the protocol exists
	// to prevent.
	fenceErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		old := &patchv1alpha1.SharedResource{}
		if err := r.reader().Get(ctx, namespacedKey, old); err != nil {
			return err
		}
		if old.Status.PromotedTo != nil {
			return nil
		}
		logger.Info("fencing the namespaced tracker before promotion",
			"tracker", old.Name, "namespace", old.Namespace, "successor", trackerName)

		old.Status.PromotedTo = &patchv1alpha1.PromotionRef{Name: trackerName}
		old.Status.Phase = patchv1alpha1.TrackerPhasePromoting
		setTrackerCond(&old.Status, patchv1alpha1.ConditionSynced, metav1.ConditionFalse,
			patchv1alpha1.ReasonPromoting,
			"a ClusterResourcePatch joined this target; migrating to a ClusterSharedResource")

		return r.Client.Status().Update(ctx, old)
	})
	if apierrors.IsNotFound(fenceErr) {
		// Nothing to promote: no namespaced tracker owns this target, so the cluster tracker is
		// simply created by the normal path.
		return nil
	}
	if fenceErr != nil {
		return fmt.Errorf("fencing tracker %s: %w", namespacedKey, fenceErr)
	}

	// Step 2: ensure the successor exists and has adopted the state.
	old := &patchv1alpha1.SharedResource{}
	if err := r.reader().Get(ctx, namespacedKey, old); err != nil {
		return client.IgnoreNotFound(err)
	}
	if err := r.createSuccessor(ctx, old, trackerName, key); err != nil {
		return err
	}

	// Step 3: record adoption, again against a fresh read.
	//
	// This flag is what lets the fenced tracker retire. Until it is set, the tracker keeps its
	// finalizer, so a crash anywhere above leaves the state recoverable rather than lost.
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &patchv1alpha1.SharedResource{}
		if err := r.reader().Get(ctx, namespacedKey, current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.Status.PromotedTo == nil {
			// Unfenced underneath us; the next pass re-fences.
			return nil
		}
		if current.Status.PromotedTo.Adopted {
			return nil
		}
		current.Status.PromotedTo.Adopted = true
		return r.Client.Status().Update(ctx, current)
	})
}

// reader returns the uncached reader when one is configured, falling back to the cached client.
//
// The fallback keeps the reconciler usable in tests that construct it without a manager; in the
// operator, main.go always supplies mgr.GetAPIReader().
func (r *ContributorReconciler[T, L]) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// createSuccessor ensures the ClusterSharedResource exists and has adopted the namespaced
// tracker's state.
//
// It must be idempotent for a reason that is easy to miss: the arriving ClusterResourcePatch's
// normal path also creates the cluster tracker, so the successor may already exist by the time
// this runs. An early return on "it exists" would leave the successor with an empty status --
// which the tracker controller would then partly repopulate from a live list, hiding the loss of
// exactly the state that cannot be re-derived.
//
// So adoption is keyed off the fence's Adopted flag, not off the successor's existence, and the
// copy MERGES rather than overwrites: the successor may already have computed its own contributor
// list, and clobbering it would be a regression.
//
// Two parts of the copy are load-bearing rather than incidental:
//
//   - priorValues (per contributor): losing them breaks revert under ClientSideApply. Fields would
//     be deleted on release instead of restored to what they held before.
//   - createdByOperator and creatorPatchRef: losing them breaks the delete-safety rule. The
//     successor would no longer know it created the object or which contributor created it, and
//     "only the creator may delete" would evaporate at exactly the moment the contributor set
//     became cross-namespace.
func (r *ContributorReconciler[T, L]) createSuccessor(
	ctx context.Context,
	old *patchv1alpha1.SharedResource,
	clusterName string,
	key scope.TargetKey,
) error {
	if old.Status.PromotedTo != nil && old.Status.PromotedTo.Adopted {
		return nil
	}

	successor := &patchv1alpha1.ClusterSharedResource{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: clusterName}, successor)

	switch {
	case apierrors.IsNotFound(err):
		successor = &patchv1alpha1.ClusterSharedResource{
			ObjectMeta: metav1.ObjectMeta{Name: clusterName},
			Spec: patchv1alpha1.SharedResourceSpec{
				TargetRef: patchv1alpha1.TrackerTargetRef{
					APIVersion: key.APIVersion,
					Kind:       key.Kind,
					Name:       key.Name,
					// Explicit here: the successor is cluster-scoped, so it can no longer infer
					// the target's namespace from its own.
					Namespace: key.Namespace,
				},
			},
		}
		controllerutil.AddFinalizer(successor, patchv1alpha1.TrackerFinalizer)
		if err := r.Client.Create(ctx, successor); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating successor %s: %w", clusterName, err)
		}
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}

	// Copy the state, re-reading uncached on each attempt. The cached read would not yet see an
	// object created moments ago, and the successor's own controller is writing its status
	// concurrently, so both a NotFound and a conflict are expected here rather than exceptional.
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &patchv1alpha1.ClusterSharedResource{}
		if err := r.reader().Get(ctx, client.ObjectKey{Name: clusterName}, current); err != nil {
			return err
		}

		adoptStatus(&current.Status, &old.Status, old.Namespace, old.Name)

		if err := r.Client.Status().Update(ctx, current); err != nil {
			return err
		}

		log.FromContext(ctx).Info("promotion: successor adopted the tracker's state",
			"successor", clusterName,
			"contributors", len(current.Status.Contributors),
			"createdByOperator", current.Status.CreatedByOperator,
			"creatorRecorded", current.Status.CreatorPatchRef != nil)
		return nil
	})
}

// adoptStatus merges the state a successor cannot re-derive from the tracker it replaces.
//
// Fields the successor can work out for itself -- the contributor list, phase, conditions -- are
// left alone if already set. Fields that only the predecessor knows are carried over.
func adoptStatus(dst, src *patchv1alpha1.SharedResourceStatus, srcNamespace, srcName string) {
	// Provenance: only the predecessor knows whether the operator created this target and which
	// contributor did it.
	if !dst.CreatedByOperator && src.CreatedByOperator {
		dst.CreatedByOperator = true
	}
	if dst.CreatorPatchRef == nil && src.CreatorPatchRef != nil {
		dst.CreatorPatchRef = src.CreatorPatchRef.DeepCopy()
	}
	if dst.ObservedBaseHash == "" {
		dst.ObservedBaseHash = src.ObservedBaseHash
	}
	if dst.ObservedTargetUID == "" {
		dst.ObservedTargetUID = src.ObservedTargetUID
	}
	if dst.ObservedResourceVersion == "" {
		dst.ObservedResourceVersion = src.ObservedResourceVersion
	}

	// Per-contributor bookkeeping. A contributor the successor already knows about keeps its own
	// record, but any revert state it is missing is filled in from the predecessor -- that state
	// cannot be recomputed, because re-deriving priorValues would capture the contributor's own
	// value and make revert a no-op.
	for _, srcRec := range src.Contributors {
		existing := dst.FindContributor(srcRec.PatchRef)
		if existing == nil {
			dst.Contributors = append(dst.Contributors, *srcRec.DeepCopy())
			continue
		}
		if existing.PriorValues == nil && srcRec.PriorValues != nil {
			existing.PriorValues = srcRec.PriorValues.DeepCopy()
		}
		if len(existing.OwnedPaths) == 0 {
			existing.OwnedPaths = append([]string(nil), srcRec.OwnedPaths...)
		}
		if existing.LastAppliedHash == "" {
			existing.LastAppliedHash = srcRec.LastAppliedHash
		}
		if existing.FieldManager == "" {
			existing.FieldManager = srcRec.FieldManager
		}
	}
	dst.ContributorCount = int32(len(dst.Contributors))

	// The successor is never itself fenced: promotion is one-way and a ClusterSharedResource is
	// never demoted.
	dst.PromotedTo = nil
	if dst.Phase == "" || dst.Phase == patchv1alpha1.TrackerPhasePromoting {
		dst.Phase = patchv1alpha1.TrackerPhaseApplied
	}
	setTrackerCond(dst, patchv1alpha1.ConditionSynced, metav1.ConditionTrue,
		"AdoptedFromPromotion",
		fmt.Sprintf("adopted %d contributor(s) from SharedResource %s/%s",
			len(dst.Contributors), srcNamespace, srcName))
}

// finishPromotion tears down a fenced tracker once its successor has adopted the state.
//
// A fenced tracker writes nothing to its target, so this never touches the target: the migration
// is a bookkeeping move, and a user watching the object sees no write at all.
func (r *TrackerReconciler[T, L]) finishPromotion(ctx context.Context, tracker T) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	status := tracker.GetTrackerStatus()

	if status.PromotedTo == nil {
		return ctrl.Result{}, nil
	}

	successor := &patchv1alpha1.ClusterSharedResource{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: status.PromotedTo.Name}, successor)

	if apierrors.IsNotFound(err) {
		// The fence landed but the successor was never created — the operator died between steps 1
		// and 2. Unfence and let the next pass retry, rather than leaving the target with no
		// writer at all.
		logger.Info("promotion successor is missing; unfencing so the promotion can be retried",
			"tracker", tracker.GetName(), "successor", status.PromotedTo.Name)
		status.PromotedTo = nil
		status.Phase = patchv1alpha1.TrackerPhaseApplied
		return ctrl.Result{Requeue: true}, r.Client.Status().Update(ctx, tracker)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// The successor exists. Do not release the finalizer until adoption is actually flagged: a
	// crash here would otherwise lose the provenance and revert bookkeeping that only this tracker
	// holds. The flag is set by createSuccessor after the copy lands, so it is the honest signal --
	// comparing contributor counts would pass as soon as the successor recomputed its own list,
	// which it can do without ever having adopted anything.
	if !status.PromotedTo.Adopted {
		logger.V(1).Info("waiting for the successor to flag adoption",
			"successor", successor.Name, "contributors", len(successor.Status.Contributors))
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	logger.Info("promotion complete; retiring the namespaced tracker",
		"tracker", tracker.GetName(), "namespace", tracker.GetNamespace(), "successor", successor.Name)

	if controllerutil.RemoveFinalizer(tracker, patchv1alpha1.TrackerFinalizer) {
		if err := r.Client.Update(ctx, tracker); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	if err := r.Client.Delete(ctx, tracker); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}
