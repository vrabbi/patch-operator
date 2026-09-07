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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	namespacedName := scope.TrackerName(key)
	old := &patchv1alpha1.SharedResource{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: namespacedName}, old)
	if apierrors.IsNotFound(err) {
		// Nothing to promote: the cluster tracker will simply be created.
		return nil
	}
	if err != nil {
		return err
	}

	clusterName := scope.TrackerName(key)

	// Step 1: fence, before the successor exists.
	if old.Status.PromotedTo == nil {
		logger.Info("fencing the namespaced tracker before promotion",
			"tracker", old.Name, "namespace", old.Namespace, "successor", clusterName)

		old.Status.PromotedTo = &patchv1alpha1.PromotionRef{Name: clusterName}
		old.Status.Phase = patchv1alpha1.TrackerPhasePromoting
		setTrackerCond(&old.Status, patchv1alpha1.ConditionSynced, metav1.ConditionFalse,
			patchv1alpha1.ReasonPromoting,
			"a ClusterResourcePatch joined this target; migrating to a ClusterSharedResource")

		if err := r.Client.Status().Update(ctx, old); err != nil {
			return fmt.Errorf("fencing tracker %s/%s: %w", old.Namespace, old.Name, err)
		}
	}

	// Step 2: create the successor, carrying the state forward.
	if err := r.createSuccessor(ctx, old, clusterName, key); err != nil {
		return err
	}

	// Step 3: record adoption.
	if !old.Status.PromotedTo.Adopted {
		old.Status.PromotedTo.Adopted = true
		if err := r.Client.Status().Update(ctx, old); err != nil {
			return fmt.Errorf("marking promotion adopted for %s/%s: %w", old.Namespace, old.Name, err)
		}
	}

	// Step 4 happens in the tracker controller's finishPromotion, so the tracker that holds the
	// finalizer is the one that releases it.
	return nil
}

// createSuccessor creates the ClusterSharedResource, copying the namespaced tracker's state.
//
// Two parts of the copy are load-bearing rather than incidental:
//
//   - priorValues: losing them breaks revert for every existing contributor under ClientSideApply.
//     Fields would be deleted on release instead of restored to what they held before.
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
	successor := &patchv1alpha1.ClusterSharedResource{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: clusterName}, successor)
	if err == nil {
		// Already created, by us on a previous pass or by a concurrent reconcile. Idempotent.
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	successor = &patchv1alpha1.ClusterSharedResource{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName},
		Spec: patchv1alpha1.SharedResourceSpec{
			TargetRef: patchv1alpha1.TrackerTargetRef{
				APIVersion: key.APIVersion,
				Kind:       key.Kind,
				Name:       key.Name,
				// The namespace must be explicit here: the successor is cluster-scoped, so it can
				// no longer infer the target's namespace from its own.
				Namespace: key.Namespace,
			},
		},
	}
	controllerutil.AddFinalizer(successor, patchv1alpha1.TrackerFinalizer)

	if err := r.Client.Create(ctx, successor); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating successor %s: %w", clusterName, err)
	}

	// The status has to be written after the create, since Create does not persist a status
	// subresource.
	successor.Status = *old.Status.DeepCopy()
	// The successor is not itself fenced: promotion is one-way and a ClusterSharedResource is
	// never demoted.
	successor.Status.PromotedTo = nil
	successor.Status.Phase = patchv1alpha1.TrackerPhaseApplied
	setTrackerCond(&successor.Status, patchv1alpha1.ConditionSynced, metav1.ConditionTrue,
		"AdoptedFromPromotion",
		fmt.Sprintf("adopted %d contributor(s) from SharedResource %s/%s",
			len(successor.Status.Contributors), old.Namespace, old.Name))

	if err := r.Client.Status().Update(ctx, successor); err != nil {
		return fmt.Errorf("copying state to successor %s: %w", clusterName, err)
	}

	log.FromContext(ctx).Info("promotion: successor adopted the tracker's state",
		"successor", clusterName,
		"contributors", len(successor.Status.Contributors),
		"createdByOperator", successor.Status.CreatedByOperator)
	return nil
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

	// The successor exists. Do not release the finalizer until it actually holds the state, or a
	// crash here would lose the contributor list along with its revert bookkeeping.
	if len(successor.Status.Contributors) < len(status.Contributors) {
		logger.V(1).Info("waiting for the successor to finish adopting state",
			"successor", successor.Name,
			"adopted", len(successor.Status.Contributors),
			"expected", len(status.Contributors))
		return ctrl.Result{Requeue: true}, nil
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
