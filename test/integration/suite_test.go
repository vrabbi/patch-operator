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

// Package integration runs the real controllers against a real API server.
//
// These tests exist for the behaviours a fake client cannot answer: finalizer ordering, the
// contributor create race, promotion's fence, and the interaction between the operator's own
// writes and the watches they provoke. Anything provable without an API server lives in a unit
// test next to the code instead.
package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/controller"
	"github.com/vrabbi/patch-operator/internal/impersonate"
	"github.com/vrabbi/patch-operator/internal/targetcache"
)

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	scheme    = k8sruntime.NewScheme()

	cancelManager context.CancelFunc
)

// Timeouts. Generous enough for a loaded CI runner, short enough that a genuine hang is a failure
// rather than a 10-minute wait.
const (
	eventually = 20 * time.Second
	tick       = 100 * time.Millisecond
)

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration suite setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is unset; skipping the integration suite")
		fmt.Fprintln(os.Stderr, "run via `make test`, which resolves envtest binaries with setup-envtest")
		return 0, nil
	}

	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(patchv1alpha1.AddToScheme(scheme))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(projectRoot(), "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		return 0, fmt.Errorf("starting envtest: %w", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", err)
		}
	}()

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return 0, fmt.Errorf("building client: %w", err)
	}

	stop, err := startManager()
	if err != nil {
		return 0, fmt.Errorf("starting manager: %w", err)
	}
	cancelManager = stop
	defer stop()

	return m.Run(), nil
}

// startManager runs the real controllers, so these tests exercise the shipped reconcile logic
// rather than a reimplementation of it.
//
// The webhooks are not registered here: envtest's API server authorizes permissively, so a
// SubjectAccessReview would return allowed unconditionally and a denial test would pass for the
// wrong reason. Admission logic is unit-tested against a fake authorizer instead, and the real
// wiring is covered by the kind-based e2e suite where RBAC is genuine. Saying so here is more
// useful than a test that looks green and proves nothing.
func startManager() (context.CancelFunc, error) {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	if err := controller.SetupIndexes(ctx, mgr); err != nil {
		cancel()
		return nil, err
	}

	impersonation := impersonate.NewFactory(cfg,
		client.Options{Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper()}, mgr.GetClient())

	// The same target cache the operator ships with, so "a target appears after a contributor has
	// been waiting" is exercised through the real watch path rather than only the requeue.
	targets := targetcache.New(mgr, targetcache.DefaultMaxGVKs, nil)

	rp := &controller.ContributorReconciler[*patchv1alpha1.ResourcePatch, *patchv1alpha1.ResourcePatchList]{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		New:       func() *patchv1alpha1.ResourcePatch { return &patchv1alpha1.ResourcePatch{} },
		NewList:   func() *patchv1alpha1.ResourcePatchList { return &patchv1alpha1.ResourcePatchList{} },
	}
	if err := rp.SetupWithManager(mgr, &patchv1alpha1.ResourcePatch{}); err != nil {
		cancel()
		return nil, err
	}

	crp := &controller.ContributorReconciler[*patchv1alpha1.ClusterResourcePatch, *patchv1alpha1.ClusterResourcePatchList]{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		New:       func() *patchv1alpha1.ClusterResourcePatch { return &patchv1alpha1.ClusterResourcePatch{} },
		NewList:   func() *patchv1alpha1.ClusterResourcePatchList { return &patchv1alpha1.ClusterResourcePatchList{} },
	}
	if err := crp.SetupWithManager(mgr, &patchv1alpha1.ClusterResourcePatch{}); err != nil {
		cancel()
		return nil, err
	}

	sr := &controller.TrackerReconciler[*patchv1alpha1.SharedResource, *patchv1alpha1.SharedResourceList]{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		New:           func() *patchv1alpha1.SharedResource { return &patchv1alpha1.SharedResource{} },
		Impersonation: impersonation,
		TargetCache:   targets,
	}
	if err := sr.SetupWithManager(mgr, &patchv1alpha1.SharedResource{}); err != nil {
		cancel()
		return nil, err
	}

	csr := &controller.TrackerReconciler[*patchv1alpha1.ClusterSharedResource, *patchv1alpha1.ClusterSharedResourceList]{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		New:           func() *patchv1alpha1.ClusterSharedResource { return &patchv1alpha1.ClusterSharedResource{} },
		Impersonation: impersonation,
		TargetCache:   targets,
	}
	if err := csr.SetupWithManager(mgr, &patchv1alpha1.ClusterSharedResource{}); err != nil {
		cancel()
		return nil, err
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "manager stopped: %v\n", err)
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		return nil, fmt.Errorf("caches did not sync")
	}
	return cancel, nil
}

// --- helpers ---

func skipWithoutEnvtest(t *testing.T) {
	t.Helper()
	if k8sClient == nil {
		t.Skip("no envtest API server; run via `make test`")
	}
}

// dumpOnFailure prints the operator's view of the world when a test fails, so a timeout says what
// the controllers actually converged to rather than only that they did not.
func dumpOnFailure(t *testing.T, ctx context.Context, ns string) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		srList := &patchv1alpha1.SharedResourceList{}
		if err := k8sClient.List(ctx, srList, client.InNamespace(ns)); err == nil {
			for _, sr := range srList.Items {
				t.Logf("SharedResource %s/%s: phase=%q count=%d fenced=%v creator=%v",
					sr.Namespace, sr.Name, sr.Status.Phase, sr.Status.ContributorCount,
					sr.Status.IsFenced(), sr.Status.CreatorPatchRef)
				for _, c := range sr.Status.Contributors {
					t.Logf("  contributor %s state=%q paths=%v", c.PatchRef, c.State, c.OwnedPaths)
				}
			}
		}
		csrList := &patchv1alpha1.ClusterSharedResourceList{}
		if err := k8sClient.List(ctx, csrList); err == nil {
			for _, csr := range csrList.Items {
				if csr.Spec.TargetRef.Namespace != ns {
					continue
				}
				t.Logf("ClusterSharedResource %s: phase=%q count=%d creator=%v",
					csr.Name, csr.Status.Phase, csr.Status.ContributorCount, csr.Status.CreatorPatchRef)
				for _, c := range csr.Status.Contributors {
					t.Logf("  contributor %s state=%q", c.PatchRef, c.State)
				}
			}
		}
		rpList := &patchv1alpha1.ResourcePatchList{}
		if err := k8sClient.List(ctx, rpList, client.InNamespace(ns)); err == nil {
			for _, rp := range rpList.Items {
				ready := patchv1alpha1.GetCondition(rp.Status.Conditions, patchv1alpha1.ConditionReady)
				t.Logf("ResourcePatch %s/%s: refs=%v ready=%v deleting=%v",
					rp.Namespace, rp.Name, rp.Status.SharedResourceRefs, ready,
					!rp.GetDeletionTimestamp().IsZero())
			}
		}
	})
}

// newNamespace creates a namespace named after the test.
func newNamespace(t *testing.T, ctx context.Context) string {
	t.Helper()
	name := "it-" + sanitize(t.Name())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace %q: %v", name, err)
	}
	return name
}

func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		default:
			if len(out) > 0 && out[len(out)-1] != '-' {
				out = append(out, '-')
			}
		}
	}
	if len(out) > 50 {
		out = out[:50]
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

func projectRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

// waitFor polls until cond returns true, failing the test with msg on timeout.
//
// Polling rather than watching keeps the assertions readable, and the failure message names what
// was being waited for rather than just "timed out".
func waitFor(t *testing.T, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(eventually)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(tick)
	}
	t.Fatalf("timed out after %s waiting for: %s", eventually, msg)
}

// consistently checks that cond holds for a short window, for asserting something does NOT happen.
func consistently(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("condition stopped holding: %s", msg)
		}
		time.Sleep(tick)
	}
}

// getConfigMap reads a ConfigMap's data, or nil if it does not exist.
func getConfigMap(t *testing.T, ctx context.Context, ns, name string) map[string]string {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cm); err != nil {
		return nil
	}
	return cm.Data
}

func configMapExists(ctx context.Context, ns, name string) bool {
	cm := &corev1.ConfigMap{}
	return k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cm) == nil
}
