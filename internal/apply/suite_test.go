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

package apply

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	clientset *kubernetes.Clientset
)

// TestMain boots a real API server. These tests exist precisely because a fake client cannot
// answer the questions being asked: server-side apply's field ownership, conflict detection and
// ownership release are API server behaviour, not client behaviour.
func TestMain(m *testing.M) {
	code, err := runSuite(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apply suite setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runSuite(m *testing.M) (int, error) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is unset; skipping the apply envtest suite")
		fmt.Fprintln(os.Stderr, "run via `make test`, which resolves envtest binaries with setup-envtest")
		return 0, nil
	}

	testEnv = &envtest.Environment{
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		return 0, fmt.Errorf("starting envtest: %w", err)
	}
	defer func() {
		if stopErr := testEnv.Stop(); stopErr != nil {
			fmt.Fprintf(os.Stderr, "stopping envtest: %v\n", stopErr)
		}
	}()

	k8sClient, err = client.New(cfg, client.Options{})
	if err != nil {
		return 0, fmt.Errorf("building client: %w", err)
	}
	clientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		return 0, fmt.Errorf("building clientset: %w", err)
	}

	return m.Run(), nil
}

// skipWithoutEnvtest lets the package's unit tests run without an API server, while the
// integration-flavoured tests skip rather than fail.
func skipWithoutEnvtest(t *testing.T) {
	t.Helper()
	if k8sClient == nil {
		t.Skip("no envtest API server; set KUBEBUILDER_ASSETS or run via `make test`")
	}
}

// testNamespace creates a namespace scoped to one test, so parallel tests cannot collide on object
// names.
func testNamespace(t *testing.T, ctx context.Context) string {
	t.Helper()
	name := "apply-" + sanitizeForName(t.Name())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace %q: %v", name, err)
	}
	return name
}

func sanitizeForName(s string) string {
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
	// Namespace names are capped at 63 characters.
	if len(out) > 50 {
		out = out[:50]
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// projectRoot locates the repository root from this file, for loading CRDs in other suites.
func projectRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}
