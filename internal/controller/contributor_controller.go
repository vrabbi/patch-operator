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

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/scope"
)

// ContributorReconciler resolves a contributor's targets, ensures a tracker exists for each, and
// mirrors the tracker's verdict back onto the contributor's status.
//
// It never writes to a target. That is the tracker's exclusive job, which is what serialises
// writes to one object and makes apply order deterministic (DESIGN.md 4.1).
//
// One implementation serves both ResourcePatch and ClusterResourcePatch via type parameters. The
// scope split is meant to cost a validation table and a tracker-selection rule, not a second copy
// of this logic.
type ContributorReconciler[T patchv1alpha1.Contributor, L client.ObjectList] struct {
	Client client.Client
	Scheme *runtimeScheme

	// APIReader reads straight from the API server, bypassing the informer cache.
	//
	// Promotion needs this. The cache is eventually consistent, and promotion reads a tracker it
	// is about to mutate -- and re-reads a successor it just created. A cached read there returns
	// a stale object or a NotFound for something that demonstrably exists, so the fence gets
	// written against a stale resourceVersion and lost. Correctness of the fence is the whole
	// safety argument for promotion, so it does not get to depend on cache timing.
	APIReader client.Reader

	// New returns a fresh empty object of the reconciled kind.
	New func() T
	// NewList returns a fresh empty list of the reconciled kind.
	NewList func() L

	// ReauthorizeAfter is how often the SubjectAccessReview recorded at admission is re-checked.
	// Admission is point-in-time: a contributor created while its author held broad rights would
	// otherwise keep working forever after those rights were revoked, because nothing ever touches
	// the object again.
	ReauthorizeAfter time.Duration

	// Authorizer re-runs the recorded principal's SubjectAccessReview. Optional; when nil the
	// re-check is skipped and only admission bounds the contributor.
	Authorizer Reauthorizer
}

// runtimeScheme is aliased so the struct field reads naturally without importing runtime here.
type runtimeScheme = scheme

// Reauthorizer re-checks that the identity recorded at admission may still make this change.
type Reauthorizer interface {
	// Authorize reports whether the recorded identity may still perform the writes this
	// contributor implies. A false result carries a human-readable reason.
	//
	// It takes the whole identity rather than a username because RBAC is usually bound to groups:
	// a review carrying only a name denies principals who are in fact still authorized.
	Authorize(
		ctx context.Context,
		user authenticationv1.UserInfo,
		c patchv1alpha1.Contributor,
	) (bool, string, error)
}

// Reconcile brings one contributor's registration up to date.
func (r *ContributorReconciler[T, L]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.New()
	if err := r.Client.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !obj.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, obj)
	}

	// Validate at reconcile as well as at admission. The containment property must not depend on
	// the webhook having been reachable when the object was created.
	if errs := scope.Validate(obj, r.isNamespacedKind); len(errs) > 0 {
		msg := errs.ToAggregate().Error()
		logger.Info("contributor spec is invalid", "error", msg)
		return ctrl.Result{}, r.setInvalid(ctx, obj, msg)
	}

	if !controllerutil.ContainsFinalizer(obj, patchv1alpha1.ContributorFinalizer) {
		// The finalizer must land before any tracker registration, or a contributor deleted in the
		// window would leave fields on a target with nothing left to withdraw them.
		controllerutil.AddFinalizer(obj, patchv1alpha1.ContributorFinalizer)
		if err := r.Client.Update(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	requeue, err := r.reauthorize(ctx, obj)
	if err != nil {
		return ctrl.Result{}, err
	}

	keys, err := r.resolveTargets(ctx, obj)
	if err != nil {
		return ctrl.Result{}, r.setStatusError(ctx, obj, patchv1alpha1.ReasonInvalidSpec, err.Error())
	}

	trackers := make([]patchv1alpha1.TrackerRef, 0, len(keys))
	for _, key := range keys {
		ref, err := r.ensureTracker(ctx, obj, key)
		if err != nil {
			return ctrl.Result{}, err
		}
		trackers = append(trackers, ref)
	}

	if err := r.syncStatus(ctx, obj, keys, trackers); err != nil {
		return ctrl.Result{}, err
	}

	// Enqueue each tracker so it re-renders with this contributor included. The tracker is the
	// only writer, so registration alone changes nothing on the target until it reconciles.
	for _, ref := range trackers {
		r.enqueueTracker(ctx, ref)
	}

	return ctrl.Result{RequeueAfter: requeue}, nil
}

// reconcileDelete waits for the tracker to confirm the release, then drops the finalizer.
//
// The ordering is the point: a contributor that vanished before its fields were withdrawn would
// leave fields nobody owns and nobody can find. So this does not revert anything itself — it asks
// the tracker to, and holds the finalizer until the tracker says the contributor is gone from its
// list.
func (r *ContributorReconciler[T, L]) reconcileDelete(ctx context.Context, obj T) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(obj, patchv1alpha1.ContributorFinalizer) {
		return ctrl.Result{}, nil
	}

	ref := patchv1alpha1.ContributorRefOf(obj)

	// Ask every tracker this contributor is registered with, from status rather than by
	// re-resolving: a Selector-mode contributor's match set may have changed since it applied, and
	// the recorded registrations are what actually hold its fields.
	stillRegistered := false
	for _, tr := range obj.GetPatchStatus().SharedResourceRefs {
		tracker, err := r.getTracker(ctx, tr)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		if tracker.GetTrackerStatus().FindContributor(ref) != nil {
			stillRegistered = true
			r.enqueueTrackerObject(ctx, tracker)
		}
	}

	if stillRegistered {
		// Not an error: the tracker is mid-release. Come back and check.
		logger.V(1).Info("waiting for the tracker to withdraw this contributor's fields")
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	controllerutil.RemoveFinalizer(obj, patchv1alpha1.ContributorFinalizer)
	if err := r.Client.Update(ctx, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// resolveTargets turns spec.target into concrete target keys.
func (r *ContributorReconciler[T, L]) resolveTargets(ctx context.Context, obj T) ([]scope.TargetKey, error) {
	spec := obj.GetPatchSpec()
	mode := spec.Target.Mode
	if mode == "" {
		mode = patchv1alpha1.TargetModeSingle
	}

	if mode == patchv1alpha1.TargetModeSingle {
		// scope.TargetKeyFor forces a namespaced contributor's target into its own namespace, so
		// containment holds here without re-reading the spec's namespace.
		return []scope.TargetKey{scope.TargetKeyFor(obj)}, nil
	}
	return r.resolveSelector(ctx, obj)
}

// resolveSelector lists the objects a Selector-mode contributor currently matches.
func (r *ContributorReconciler[T, L]) resolveSelector(ctx context.Context, obj T) ([]scope.TargetKey, error) {
	spec := obj.GetPatchSpec()

	gv, err := schema.ParseGroupVersion(spec.Target.APIVersion)
	if err != nil {
		return nil, fmt.Errorf("parsing target apiVersion: %w", err)
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gv.WithKind(spec.Target.Kind + "List"))

	opts := []client.ListOption{}
	if spec.Target.Selector != nil {
		sel, err := metav1.LabelSelectorAsSelector(spec.Target.Selector)
		if err != nil {
			return nil, fmt.Errorf("parsing target selector: %w", err)
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}

	// A namespaced contributor's fan-out is confined to its own namespace. This is the containment
	// property applied to Selector mode, and it is why namespaceSelector is rejected for this kind.
	namespaces, err := r.selectorNamespaces(ctx, obj)
	if err != nil {
		return nil, err
	}

	var keys []scope.TargetKey
	maxTargets := int(spec.Target.MaxTargets)
	if maxTargets <= 0 {
		maxTargets = 100
	}

	for _, ns := range namespaces {
		scoped := opts
		if ns != "" {
			scoped = append(append([]client.ListOption{}, opts...), client.InNamespace(ns))
		}
		if err := r.Client.List(ctx, list, scoped...); err != nil {
			return nil, fmt.Errorf("listing targets: %w", err)
		}
		for i := range list.Items {
			item := &list.Items[i]
			keys = append(keys, scope.TargetKey{
				APIVersion: spec.Target.APIVersion,
				Kind:       spec.Target.Kind,
				Namespace:  item.GetNamespace(),
				Name:       item.GetName(),
			})
			// Exceeding the cap fails the contributor rather than silently truncating: a
			// half-applied fan-out is worse than a reported one.
			if len(keys) > maxTargets {
				return nil, fmt.Errorf(
					"selector matched more than maxTargets=%d objects; narrow the selector or raise the cap",
					maxTargets)
			}
		}
	}
	return keys, nil
}

// selectorNamespaces returns the namespaces a Selector-mode contributor may fan out across.
func (r *ContributorReconciler[T, L]) selectorNamespaces(ctx context.Context, obj T) ([]string, error) {
	if !obj.IsClusterScoped() {
		return []string{obj.GetNamespace()}, nil
	}

	spec := obj.GetPatchSpec()
	if spec.Target.NamespaceSelector == nil {
		// Every namespace, and for a cluster-scoped kind the empty namespace means cluster scope.
		return []string{""}, nil
	}

	sel, err := metav1.LabelSelectorAsSelector(spec.Target.NamespaceSelector)
	if err != nil {
		return nil, fmt.Errorf("parsing namespaceSelector: %w", err)
	}
	nsList := &unstructured.UnstructuredList{}
	nsList.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "NamespaceList"})
	if err := r.Client.List(ctx, nsList, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}

	out := make([]string, 0, len(nsList.Items))
	for i := range nsList.Items {
		out = append(out, nsList.Items[i].GetName())
	}
	return out, nil
}

// ensureTracker creates the tracker for a target if it is absent, and returns a reference to it.
//
// The tracker kind follows the rule in DESIGN.md 3.5 rather than whoever arrived first, so two
// contributors racing reach the same answer. A cluster-scoped contributor arriving at a namespaced
// tracker starts a promotion instead of creating a second writer.
func (r *ContributorReconciler[T, L]) ensureTracker(
	ctx context.Context,
	obj T,
	key scope.TargetKey,
) (patchv1alpha1.TrackerRef, error) {
	// A cluster-scoped contributor always needs the cluster tracker, and if a namespaced one
	// already owns this target it must be promoted first.
	if obj.IsClusterScoped() && !key.IsClusterScopedTarget() {
		if err := r.promoteIfNeeded(ctx, key); err != nil {
			return patchv1alpha1.TrackerRef{}, err
		}
	}

	clusterRef := patchv1alpha1.TrackerRef{
		Kind: patchv1alpha1.KindClusterSharedResource,
		Name: scope.TrackerName(key),
	}

	// A namespaced contributor must join an existing ClusterSharedResource rather than stand up a
	// namespaced tracker beside it.
	//
	// This is rule 3 of DESIGN.md 3.5 seen from the namespaced side: once any cluster-scoped
	// contributor exists, the cluster tracker owns the target for everyone. Skipping this check
	// would let a namespaced contributor recreate a retired namespaced tracker the moment
	// promotion deleted it, resurrecting the second writer the promotion existed to remove.
	//
	// Read uncached: a cached miss here is indistinguishable from "no cluster tracker exists", and
	// guessing wrong recreates the writer.
	if !obj.IsClusterScoped() && !key.IsClusterScopedTarget() {
		existing := &patchv1alpha1.ClusterSharedResource{}
		err := r.reader().Get(ctx, client.ObjectKey{Name: clusterRef.Name}, existing)
		switch {
		case err == nil:
			return clusterRef, nil
		case !apierrors.IsNotFound(err):
			return patchv1alpha1.TrackerRef{}, err
		}
	}

	kind := scope.TrackerKindFor(key, obj.IsClusterScoped())
	name := scope.TrackerName(key)
	namespace := scope.TrackerNamespace(kind, key)

	ref := patchv1alpha1.TrackerRef{Kind: kind, Name: name, Namespace: namespace}

	tracker, err := r.getTracker(ctx, ref)
	if err == nil {
		// A fenced namespaced tracker has been superseded; follow the pointer rather than
		// registering with something that will not write.
		if st := tracker.GetTrackerStatus(); st.IsFenced() && st.PromotedTo != nil {
			return patchv1alpha1.TrackerRef{
				Kind: patchv1alpha1.KindClusterSharedResource,
				Name: st.PromotedTo.Name,
			}, nil
		}
		return ref, nil
	}
	if !apierrors.IsNotFound(err) {
		return patchv1alpha1.TrackerRef{}, err
	}

	created, err := newTracker(kind, name, namespace, key)
	if err != nil {
		return patchv1alpha1.TrackerRef{}, err
	}
	controllerutil.AddFinalizer(created, patchv1alpha1.TrackerFinalizer)

	if err := r.Client.Create(ctx, created); err != nil {
		// AlreadyExists is the expected outcome of a create race, not an error: the names are
		// deterministic, so the loser simply uses what the winner made.
		if apierrors.IsAlreadyExists(err) {
			return ref, nil
		}
		return patchv1alpha1.TrackerRef{}, err
	}
	return ref, nil
}

// syncStatus records the resolved targets and tracker registrations, and mirrors each tracker's
// verdict for this contributor back onto its conditions.
func (r *ContributorReconciler[T, L]) syncStatus(
	ctx context.Context,
	obj T,
	keys []scope.TargetKey,
	trackers []patchv1alpha1.TrackerRef,
) error {
	status := obj.GetPatchStatus()
	ref := patchv1alpha1.ContributorRefOf(obj)

	observed := make([]patchv1alpha1.TargetStatus, 0, len(keys))
	applied, conflicted, superseded, waiting := 0, 0, 0, 0

	for i, key := range keys {
		ts := patchv1alpha1.TargetStatus{
			APIVersion: key.APIVersion,
			Kind:       key.Kind,
			Name:       key.Name,
			Namespace:  key.Namespace,
		}

		if i < len(trackers) {
			if tracker, err := r.getTracker(ctx, trackers[i]); err == nil {
				st := tracker.GetTrackerStatus()
				ts.UID = st.ObservedTargetUID
				if rec := st.FindContributor(ref); rec != nil {
					ts.State = rec.State
					switch rec.State {
					case patchv1alpha1.ContributorStateApplied:
						applied++
					case patchv1alpha1.ContributorStateConflicted:
						conflicted++
					case patchv1alpha1.ContributorStateSuperseded:
						superseded++
					default:
						waiting++
					}
				} else {
					waiting++
				}
			} else if !apierrors.IsNotFound(err) {
				return err
			} else {
				waiting++
			}
		}
		observed = append(observed, ts)
	}

	status.ObservedTargets = observed
	status.SharedResourceRefs = trackers

	// Ready is true only when every resolved target carries this contribution and this contributor
	// is not party to an unresolved conflict. A superseded contributor reports NotReady rather than
	// quietly losing: its fields are genuinely absent, and an XR should not report ready.
	switch {
	case len(keys) == 0:
		setCond(status, patchv1alpha1.ConditionReady, metav1.ConditionFalse,
			patchv1alpha1.ReasonWaitingForTarget, "no targets resolved")
	case conflicted > 0:
		setCond(status, patchv1alpha1.ConditionReady, metav1.ConditionFalse,
			patchv1alpha1.ReasonConflict,
			fmt.Sprintf("%d of %d targets have an unresolved field conflict", conflicted, len(keys)))
	case superseded > 0:
		setCond(status, patchv1alpha1.ConditionReady, metav1.ConditionFalse,
			patchv1alpha1.ReasonSuperseded,
			fmt.Sprintf("a higher-priority contributor holds contested fields on %d of %d targets; "+
				"this contribution is not present", superseded, len(keys)))
	case waiting > 0:
		setCond(status, patchv1alpha1.ConditionReady, metav1.ConditionFalse,
			patchv1alpha1.ReasonWaitingForTarget,
			fmt.Sprintf("%d of %d targets not yet applied", waiting, len(keys)))
	default:
		setCond(status, patchv1alpha1.ConditionReady, metav1.ConditionTrue,
			patchv1alpha1.ReasonApplied,
			fmt.Sprintf("applied to %d target(s)", applied))
		status.AppliedGeneration = obj.GetGeneration()
	}

	setCond(status, patchv1alpha1.ConditionConflict,
		boolCondition(conflicted > 0), patchv1alpha1.ReasonConflict, "")
	setCond(status, patchv1alpha1.ConditionSynced, metav1.ConditionTrue, "ReconcileSuccess", "")

	return r.Client.Status().Update(ctx, obj)
}

// reauthorize re-runs the SubjectAccessReview recorded at admission, on a TTL.
//
// When it fails the operator stops writing but does *not* revert. Losing authorization is not the
// same as being released: silently tearing down a tenant's ingress rule because an RBAC binding
// was reorganised would be worse than the exposure.
func (r *ContributorReconciler[T, L]) reauthorize(ctx context.Context, obj T) (time.Duration, error) {
	if r.Authorizer == nil || r.ReauthorizeAfter <= 0 {
		return 0, nil
	}

	status := obj.GetPatchStatus()
	user, recorded := patchv1alpha1.IdentityFromAnnotations(obj)
	if !recorded {
		// No recorded principal means the object predates the webhook or bypassed it. Nothing to
		// re-check against, so leave the condition alone rather than asserting authorization.
		return r.ReauthorizeAfter, nil
	}
	principal := user.Username

	if status.LastAuthorizedTime != nil &&
		time.Since(status.LastAuthorizedTime.Time) < r.ReauthorizeAfter {
		return r.ReauthorizeAfter - time.Since(status.LastAuthorizedTime.Time), nil
	}

	allowed, reason, err := r.Authorizer.Authorize(ctx, user, obj)
	if err != nil {
		return r.ReauthorizeAfter, err
	}

	now := metav1.Now()
	status.AuthorizedAs = principal
	if allowed {
		status.LastAuthorizedTime = &now
		setCond(status, patchv1alpha1.ConditionAuthorized, metav1.ConditionTrue, "Authorized", "")
	} else {
		setCond(status, patchv1alpha1.ConditionAuthorized, metav1.ConditionFalse,
			patchv1alpha1.ReasonUnauthorized, reason)
	}
	return r.ReauthorizeAfter, r.Client.Status().Update(ctx, obj)
}

func (r *ContributorReconciler[T, L]) setInvalid(ctx context.Context, obj T, msg string) error {
	return r.setStatusError(ctx, obj, patchv1alpha1.ReasonInvalidSpec, msg)
}

func (r *ContributorReconciler[T, L]) setStatusError(ctx context.Context, obj T, reason, msg string) error {
	status := obj.GetPatchStatus()
	setCond(status, patchv1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg)
	setCond(status, patchv1alpha1.ConditionSynced, metav1.ConditionFalse, reason, msg)
	return r.Client.Status().Update(ctx, obj)
}

// isNamespacedKind answers the RESTMapper question scope.Validate asks.
func (r *ContributorReconciler[T, L]) isNamespacedKind(apiVersion, kind string) (bool, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false, err
	}
	mapping, err := r.Client.RESTMapper().RESTMapping(gv.WithKind(kind).GroupKind(), gv.Version)
	if err != nil {
		return false, err
	}
	return mapping.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// SetupWithManager wires the controller.
//
// Watching trackers matters as much as watching contributors: a contributor's Ready condition is
// the tracker's verdict, so a tracker changing its mind has to wake the contributors it names.
func (r *ContributorReconciler[T, L]) SetupWithManager(mgr ctrl.Manager, forObj client.Object) error {
	name := fmt.Sprintf("contributor-%T", forObj)

	return ctrl.NewControllerManagedBy(mgr).
		Named(sanitizeControllerName(name)).
		For(forObj, builder.WithPredicates(generationOrFinalizerChanged())).
		Watches(&patchv1alpha1.SharedResource{},
			handler.EnqueueRequestsFromMapFunc(r.contributorsOfTracker)).
		Watches(&patchv1alpha1.ClusterSharedResource{},
			handler.EnqueueRequestsFromMapFunc(r.contributorsOfTracker)).
		Complete(r)
}

// contributorsOfTracker maps a tracker to the contributors it holds, filtered to the kind this
// reconciler serves.
func (r *ContributorReconciler[T, L]) contributorsOfTracker(_ context.Context, obj client.Object) []reconcile.Request {
	tracker, ok := obj.(patchv1alpha1.Tracker)
	if !ok {
		return nil
	}
	wantKind := r.New().ContributorKind()

	contributors := tracker.GetTrackerStatus().Contributors
	out := make([]reconcile.Request, 0, len(contributors))
	for _, c := range contributors {
		if c.PatchRef.Kind != wantKind {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: c.PatchRef.Namespace,
			Name:      c.PatchRef.Name,
		}})
	}
	return out
}
