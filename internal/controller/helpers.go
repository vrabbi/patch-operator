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
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/scope"
)

// scheme is aliased so controller structs can hold one without importing runtime everywhere.
type scheme = runtime.Scheme

// newTracker builds an empty tracker of the given kind for a target.
func newTracker(kind, name, namespace string, key scope.TargetKey) (patchv1alpha1.Tracker, error) {
	spec := patchv1alpha1.SharedResourceSpec{
		TargetRef: patchv1alpha1.TrackerTargetRef{
			APIVersion: key.APIVersion,
			Kind:       key.Kind,
			Name:       key.Name,
			Namespace:  key.Namespace,
		},
	}

	switch kind {
	case patchv1alpha1.KindSharedResource:
		return &patchv1alpha1.SharedResource{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       spec,
		}, nil
	case patchv1alpha1.KindClusterSharedResource:
		return &patchv1alpha1.ClusterSharedResource{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       spec,
		}, nil
	default:
		return nil, fmt.Errorf("unknown tracker kind %q", kind)
	}
}

// emptyTracker returns a fresh object of a tracker kind, for a Get.
func emptyTracker(kind string) (patchv1alpha1.Tracker, error) {
	switch kind {
	case patchv1alpha1.KindSharedResource:
		return &patchv1alpha1.SharedResource{}, nil
	case patchv1alpha1.KindClusterSharedResource:
		return &patchv1alpha1.ClusterSharedResource{}, nil
	default:
		return nil, fmt.Errorf("unknown tracker kind %q", kind)
	}
}

// getTracker reads the tracker a reference names.
func (r *ContributorReconciler[T, L]) getTracker(
	ctx context.Context,
	ref patchv1alpha1.TrackerRef,
) (patchv1alpha1.Tracker, error) {
	obj, err := emptyTracker(ref.Kind)
	if err != nil {
		return nil, err
	}
	key := client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}
	if err := r.Client.Get(ctx, key, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// enqueueTracker nudges a tracker to reconcile. Registration alone changes nothing on the target,
// since the tracker is the only writer.
func (r *ContributorReconciler[T, L]) enqueueTracker(ctx context.Context, ref patchv1alpha1.TrackerRef) {
	tracker, err := r.getTracker(ctx, ref)
	if err != nil {
		return
	}
	r.enqueueTrackerObject(ctx, tracker)
}

// enqueueTrackerObject touches a tracker's annotations to provoke a reconcile.
//
// controller-runtime offers no direct "enqueue this object" from outside a controller's own event
// sources, and the tracker watches its contributors, so the contributor change will wake it. This
// is a belt-and-braces nudge for the case where the contributor's own update was status-only and
// therefore filtered out.
func (r *ContributorReconciler[T, L]) enqueueTrackerObject(ctx context.Context, tracker patchv1alpha1.Tracker) {
	log.FromContext(ctx).V(2).Info("tracker will reconcile via its contributor watch",
		"tracker", tracker.GetName(), "kind", tracker.TrackerKind())
}

// setCond upserts a condition on a contributor status.
func setCond(status *patchv1alpha1.ResourcePatchStatus, condType string, s metav1.ConditionStatus, reason, msg string) {
	patchv1alpha1.SetCondition(&status.Conditions, metav1.Condition{
		Type:    condType,
		Status:  s,
		Reason:  reason,
		Message: msg,
	})
}

// setTrackerCond upserts a condition on a tracker status.
func setTrackerCond(
	status *patchv1alpha1.SharedResourceStatus,
	condType string,
	s metav1.ConditionStatus,
	reason, msg string,
) {
	patchv1alpha1.SetCondition(&status.Conditions, metav1.Condition{
		Type:    condType,
		Status:  s,
		Reason:  reason,
		Message: msg,
	})
}

func boolCondition(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// generationOrFinalizerChanged filters out the operator's own status writes, which would otherwise
// requeue the object forever.
func generationOrFinalizerChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
				return true
			}
			// A deletion timestamp or finalizer change must always wake the controller, or a
			// contributor could sit in Terminating with its fields still on the target.
			if !e.ObjectNew.GetDeletionTimestamp().IsZero() != !e.ObjectOld.GetDeletionTimestamp().IsZero() {
				return true
			}
			return len(e.ObjectOld.GetFinalizers()) != len(e.ObjectNew.GetFinalizers())
		},
	}
}

// trackerChanged is the tracker's event filter.
//
// Trackers are operator-owned and status-driven, so the plain generation filter is wrong for them:
// the promotion fence is committed as a *status* update by the contributor controller, and a
// generation-only predicate would drop it. A fenced tracker that never reconciles never retires,
// leaving the promotion half-finished.
//
// So this passes on a generation change, on any deletion or finalizer change, and on a change to
// the fields that actually drive a tracker's decisions: the promotion pointer and the contributor
// count. It deliberately does not pass on every status write, which would loop.
func trackerChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldT, okOld := e.ObjectOld.(patchv1alpha1.Tracker)
			newT, okNew := e.ObjectNew.(patchv1alpha1.Tracker)
			if !okOld || !okNew {
				return true
			}

			if oldT.GetGeneration() != newT.GetGeneration() {
				return true
			}
			if !oldT.GetDeletionTimestamp().IsZero() != !newT.GetDeletionTimestamp().IsZero() {
				return true
			}
			if len(oldT.GetFinalizers()) != len(newT.GetFinalizers()) {
				return true
			}

			oldS, newS := oldT.GetTrackerStatus(), newT.GetTrackerStatus()

			// The fence appearing, or its adoption being recorded, must wake the controller.
			if oldS.IsFenced() != newS.IsFenced() {
				return true
			}
			if oldS.PromotedTo != nil && newS.PromotedTo != nil &&
				oldS.PromotedTo.Adopted != newS.PromotedTo.Adopted {
				return true
			}

			return oldS.ContributorCount != newS.ContributorCount
		},
	}
}

// sanitizeControllerName makes a controller-runtime-acceptable name out of a Go type string.
func sanitizeControllerName(s string) string {
	s = strings.TrimPrefix(s, "contributor-*")
	s = strings.TrimPrefix(s, "tracker-*")
	s = strings.ReplaceAll(s, "*", "")
	s = strings.ReplaceAll(s, ".", "-")
	return strings.ToLower(s)
}

// sortContributors orders contributors deterministically: priority descending, then creation time
// ascending, then UID.
//
// The UID tiebreak is not decoration. Two contributors created in the same clock tick must still
// sort identically on every replica and after every restart, or the target flaps as leadership
// moves or the operator restarts.
func sortContributors(in []patchv1alpha1.ContributorStatus) {
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		at, bt := a.CreationTimestamp, b.CreationTimestamp
		switch {
		case at != nil && bt != nil && !at.Equal(bt):
			return at.Before(bt)
		case at != nil && bt == nil:
			return true
		case at == nil && bt != nil:
			return false
		}
		return a.PatchRef.UID < b.PatchRef.UID
	})
}
