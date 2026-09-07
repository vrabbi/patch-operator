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

// Package targetcache watches target objects so drift on a shared resource is corrected.
//
// Target GVKs are not known at compile time, so informers are created lazily on first use. That is
// the main scalability hazard in the operator: an informer over every Secret or ConfigMap in a
// large cluster is expensive, so the number of distinct watched GVKs is capped and exceeding the
// cap is reported rather than silently degrading.
package targetcache

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// MapFunc maps a changed target object to the trackers that own it. It is typed on
// *unstructured.Unstructured because target kinds are not known at compile time.
type MapFunc = handler.TypedMapFunc[*unstructured.Unstructured, reconcile.Request]

// DefaultMaxGVKs caps how many distinct target kinds are watched at once.
const DefaultMaxGVKs = 50

// Manager starts and tracks per-GVK informers for target objects.
type Manager struct {
	// Cluster provides the shared cache the informers are created in.
	Cluster manager.Manager

	// MaxGVKs caps distinct watched kinds. Zero means DefaultMaxGVKs.
	MaxGVKs int

	// Disabled kinds fall back to periodic resync instead of a watch, for kinds where a
	// cluster-wide informer is not affordable. Correctness is unchanged; drift correction is just
	// slower.
	Disabled map[schema.GroupVersionKind]bool

	// watchFn registers the watch. Injectable so the bookkeeping -- the per-owner keying, the cap,
	// and the retry-after-failure behaviour -- can be tested without standing up a manager and its
	// cache, which are not what those rules are about.
	watchFn func(controller.Controller, schema.GroupVersionKind, MapFunc) error

	mu sync.Mutex
	// watched is keyed by (owner, gvk) rather than by gvk alone.
	//
	// This matters because the Manager is shared between the SharedResource and
	// ClusterSharedResource controllers, and a watch registration belongs to one controller. Keying
	// on the GVK alone meant whichever controller asked first got the watch and the other silently
	// got none -- so a promoted target ended up with a tracker that never saw its object change.
	watched map[watchKey]bool
	// atCap records that the cap has been reported once, so the log does not repeat per reconcile.
	atCap bool
}

// watchKey identifies one controller's watch on one kind.
type watchKey struct {
	owner string
	gvk   schema.GroupVersionKind
}

// New builds a Manager.
func New(mgr manager.Manager, maxGVKs int, disabled []schema.GroupVersionKind) *Manager {
	d := make(map[schema.GroupVersionKind]bool, len(disabled))
	for _, gvk := range disabled {
		d[gvk] = true
	}
	if maxGVKs <= 0 {
		maxGVKs = DefaultMaxGVKs
	}
	m := &Manager{
		Cluster:  mgr,
		MaxGVKs:  maxGVKs,
		Disabled: d,
		watched:  map[watchKey]bool{},
	}
	m.watchFn = m.watch
	return m
}

// watch is the real registration: an informer for the kind, from the manager's shared cache.
func (m *Manager) watch(
	ctrl controller.Controller,
	gvk schema.GroupVersionKind,
	mapFn MapFunc,
) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	return ctrl.Watch(source.Kind(m.Cluster.GetCache(), obj,
		handler.TypedEnqueueRequestsFromMapFunc(mapFn)))
}

// Ensure starts watching a target GVK if it is not already watched.
//
// It is safe to call on every reconcile: the second and later calls for a GVK are a map lookup
// under a mutex.
//
// Known limitation: controller-runtime has no clean way to stop watching a source once a
// controller has started, so a GVK watched for a target that later disappears keeps its informer
// until the process restarts. The cap is what bounds that, and it is documented rather than
// papered over.
// owner distinguishes the calling controller, since each needs its own watch registration.
func (m *Manager) Ensure(
	ctx context.Context,
	owner string,
	ctrl controller.Controller,
	gvk schema.GroupVersionKind,
	mapFn MapFunc,
) error {
	logger := log.FromContext(ctx)

	if m.Disabled[gvk] {
		logger.V(1).Info("target watch disabled for this kind; relying on periodic resync",
			"gvk", gvk.String())
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	key := watchKey{owner: owner, gvk: gvk}
	if m.watched[key] {
		return nil
	}
	if m.distinctGVKsLocked() >= m.MaxGVKs && !m.gvkWatchedLocked(gvk) {
		if !m.atCap {
			m.atCap = true
			logger.Info("watched-GVK cap reached; further target kinds rely on periodic resync",
				"cap", m.MaxGVKs, "gvk", gvk.String())
		}
		return fmt.Errorf("watched-GVK cap of %d reached; cannot watch %s", m.MaxGVKs, gvk)
	}

	// A failed registration is deliberately not recorded, so the kind is retried on the next
	// reconcile rather than being silently left unwatched forever.
	if err := m.watchFn(ctrl, gvk, mapFn); err != nil {
		return fmt.Errorf("watching %s: %w", gvk, err)
	}

	m.watched[key] = true
	logger.Info("watching target kind",
		"gvk", gvk.String(), "owner", owner, "distinctKinds", m.distinctGVKsLocked())
	return nil
}

// distinctGVKsLocked counts distinct kinds, not registrations. The cap bounds informers, and two
// controllers watching the same kind share one informer from the manager's cache.
func (m *Manager) distinctGVKsLocked() int {
	seen := map[schema.GroupVersionKind]bool{}
	for k := range m.watched {
		seen[k.gvk] = true
	}
	return len(seen)
}

func (m *Manager) gvkWatchedLocked(gvk schema.GroupVersionKind) bool {
	for k := range m.watched {
		if k.gvk == gvk {
			return true
		}
	}
	return false
}

// Watched returns the distinct GVKs currently watched, for metrics and tests.
func (m *Manager) Watched() []schema.GroupVersionKind {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[schema.GroupVersionKind]bool{}
	out := make([]schema.GroupVersionKind, 0, len(m.watched))
	for k := range m.watched {
		if seen[k.gvk] {
			continue
		}
		seen[k.gvk] = true
		out = append(out, k.gvk)
	}
	return out
}

// Count returns how many distinct GVKs are watched.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.distinctGVKsLocked()
}
