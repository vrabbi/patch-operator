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

package targetcache

import (
	"context"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// fakeController records the sources a controller was asked to watch.
type fakeController struct {
	controller.Controller
	mu      sync.Mutex
	watches int
	err     error
}

func (c *fakeController) Watch(_ source.TypedSource[reconcile.Request]) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.watches++
	return nil
}

func (c *fakeController) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watches
}

func cm() schema.GroupVersionKind {
	return schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
}

func ingress() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress"}
}

// newTestManager builds a Manager whose watch registration is redirected to the fake controller,
// so these tests exercise the bookkeeping rather than controller-runtime's cache.
func newTestManager(maxGVKs int, disabled []schema.GroupVersionKind) *Manager {
	m := New(nil, maxGVKs, disabled)
	m.watchFn = func(ctrl controller.Controller, _ schema.GroupVersionKind, _ MapFunc) error {
		return ctrl.Watch(nil)
	}
	return m
}

func TestNewDefaultsTheCap(t *testing.T) {
	if got := New(nil, 0, nil).MaxGVKs; got != DefaultMaxGVKs {
		t.Errorf("MaxGVKs = %d, want the default %d", got, DefaultMaxGVKs)
	}
	if got := New(nil, -5, nil).MaxGVKs; got != DefaultMaxGVKs {
		t.Errorf("a negative cap should fall back to the default, got %d", got)
	}
	if got := New(nil, 7, nil).MaxGVKs; got != 7 {
		t.Errorf("MaxGVKs = %d, want 7", got)
	}
}

// A disabled kind is skipped without error: the tracker still converges on its requeue interval,
// so this degrades responsiveness rather than correctness.
func TestEnsureSkipsDisabledKinds(t *testing.T) {
	m := newTestManager(10, []schema.GroupVersionKind{cm()})
	ctrl := &fakeController{}

	if err := m.Ensure(context.Background(), "SharedResource", ctrl, cm(), nil); err != nil {
		t.Fatalf("a disabled kind should be skipped, not error: %v", err)
	}
	if ctrl.count() != 0 {
		t.Error("a disabled kind should not be watched")
	}
	if m.Count() != 0 {
		t.Errorf("Count() = %d, want 0", m.Count())
	}
}

// The second call for the same (owner, kind) is a no-op, so calling Ensure on every reconcile is
// safe.
func TestEnsureIsIdempotentPerOwner(t *testing.T) {
	m := newTestManager(10, nil)
	ctrl := &fakeController{}

	for i := 0; i < 5; i++ {
		if err := m.Ensure(context.Background(), "SharedResource", ctrl, cm(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if ctrl.count() != 1 {
		t.Errorf("registered %d watches for repeated calls, want 1", ctrl.count())
	}
}

// The regression this keying exists for: the Manager is shared between the two tracker
// controllers, and a watch registration belongs to one controller. Keying on the kind alone meant
// whichever controller asked first got the watch and the other silently got none -- so a promoted
// target ended up with a tracker that never saw its object change.
func TestEachOwnerGetsItsOwnWatch(t *testing.T) {
	m := newTestManager(10, nil)
	sharedCtrl := &fakeController{}
	clusterCtrl := &fakeController{}

	if err := m.Ensure(context.Background(), "SharedResource", sharedCtrl, cm(), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(context.Background(), "ClusterSharedResource", clusterCtrl, cm(), nil); err != nil {
		t.Fatal(err)
	}

	if sharedCtrl.count() != 1 {
		t.Errorf("SharedResource controller registered %d watches, want 1", sharedCtrl.count())
	}
	if clusterCtrl.count() != 1 {
		t.Errorf("ClusterSharedResource controller registered %d watches, want 1; "+
			"keying on the kind alone would leave it with none", clusterCtrl.count())
	}
	// One kind, so one informer, even though two controllers watch it.
	if m.Count() != 1 {
		t.Errorf("Count() = %d; the cap counts distinct kinds, not registrations", m.Count())
	}
}

// The cap bounds informers, and two controllers watching one kind share an informer from the
// manager's cache. So a second owner on an already-watched kind must not be refused even at the
// cap.
func TestCapCountsDistinctKindsNotRegistrations(t *testing.T) {
	m := newTestManager(1, nil)
	a := &fakeController{}
	b := &fakeController{}

	if err := m.Ensure(context.Background(), "SharedResource", a, cm(), nil); err != nil {
		t.Fatal(err)
	}
	// Same kind, different owner: allowed at the cap, since it reuses the informer.
	if err := m.Ensure(context.Background(), "ClusterSharedResource", b, cm(), nil); err != nil {
		t.Errorf("a second owner of an already-watched kind should be allowed at the cap: %v", err)
	}
	// A different kind would need a new informer, so the cap applies.
	if err := m.Ensure(context.Background(), "SharedResource", a, ingress(), nil); err == nil {
		t.Error("a new kind beyond the cap should be refused")
	}
}

func TestEnsureReportsCapOnce(t *testing.T) {
	m := newTestManager(1, nil)
	ctrl := &fakeController{}

	if err := m.Ensure(context.Background(), "SharedResource", ctrl, cm(), nil); err != nil {
		t.Fatal(err)
	}
	first := m.Ensure(context.Background(), "SharedResource", ctrl, ingress(), nil)
	second := m.Ensure(context.Background(), "SharedResource", ctrl, ingress(), nil)
	if first == nil || second == nil {
		t.Fatal("both calls beyond the cap should report an error")
	}
	if m.Count() != 1 {
		t.Errorf("Count() = %d, want 1", m.Count())
	}
}

// A failed Watch must not be recorded as watched, or the kind would never be retried.
func TestFailedWatchIsNotRecorded(t *testing.T) {
	m := newTestManager(10, nil)
	ctrl := &fakeController{err: errFake}

	if err := m.Ensure(context.Background(), "SharedResource", ctrl, cm(), nil); err == nil {
		t.Fatal("expected the Watch error to propagate")
	}
	if m.Count() != 0 {
		t.Errorf("a failed watch was recorded as watched: Count() = %d", m.Count())
	}

	// It must be retryable.
	ctrl.err = nil
	if err := m.Ensure(context.Background(), "SharedResource", ctrl, cm(), nil); err != nil {
		t.Errorf("a previously failed kind should be retryable: %v", err)
	}
	if m.Count() != 1 {
		t.Errorf("Count() = %d, want 1 after the retry", m.Count())
	}
}

func TestWatchedReturnsDistinctKinds(t *testing.T) {
	m := newTestManager(10, nil)
	a := &fakeController{}
	b := &fakeController{}

	for _, gvk := range []schema.GroupVersionKind{cm(), ingress()} {
		if err := m.Ensure(context.Background(), "SharedResource", a, gvk, nil); err != nil {
			t.Fatal(err)
		}
	}
	// A second owner of an already-watched kind must not duplicate it in the listing.
	if err := m.Ensure(context.Background(), "ClusterSharedResource", b, cm(), nil); err != nil {
		t.Fatal(err)
	}

	got := m.Watched()
	if len(got) != 2 {
		t.Fatalf("Watched() = %v, want 2 distinct kinds", got)
	}
	seen := map[schema.GroupVersionKind]bool{}
	for _, gvk := range got {
		if seen[gvk] {
			t.Errorf("duplicate kind in Watched(): %v", gvk)
		}
		seen[gvk] = true
	}
}

// Ensure is called from reconcile, which runs concurrently across targets.
func TestEnsureIsSafeUnderConcurrency(t *testing.T) {
	m := newTestManager(10, nil)
	ctrl := &fakeController{}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Ensure(context.Background(), "SharedResource", ctrl, cm(), nil)
			_ = m.Count()
			_ = m.Watched()
		}()
	}
	wg.Wait()

	if ctrl.count() != 1 {
		t.Errorf("concurrent callers registered %d watches, want exactly 1", ctrl.count())
	}
}

var errFake = fakeErr("watch failed")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }
