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

// Package e2e exercises the operator as deployed: a real image in a real cluster, with real RBAC
// and the admission webhooks actually serving.
//
// It covers what the envtest integration suite structurally cannot. envtest's API server
// authorizes permissively, so a SubjectAccessReview there returns allowed unconditionally and a
// denial test would pass for the wrong reason. Here RBAC is genuine, so the authorization model --
// the part of this design that matters most -- is verified rather than assumed.
//
// Requires a cluster with the operator deployed; see .github/workflows/e2e.yaml. It is skipped
// unless a kubeconfig is reachable.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	operatorNamespace = "patch-operator-system"
	eventually        = 90 * time.Second
	tick              = 2 * time.Second
)

func TestMain(m *testing.M) {
	if !clusterReachable() {
		// CI sets E2E_REQUIRE_CLUSTER, because there the skip path is indistinguishable from a
		// green run and would report success while testing nothing.
		if os.Getenv("E2E_REQUIRE_CLUSTER") != "" {
			fmt.Fprintln(os.Stderr, "E2E_REQUIRE_CLUSTER is set but no cluster is reachable")
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "no reachable cluster; skipping the e2e suite")
		fmt.Fprintln(os.Stderr, "see .github/workflows/e2e.yaml for the expected setup")
		os.Exit(0)
	}
	if err := waitForOperator(); err != nil {
		fmt.Fprintf(os.Stderr, "operator is not ready: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// --- kubectl helpers ---

// kubectl runs a command and returns its *stdout* only.
//
// Keeping stderr out of the return value is not tidiness. `kubectl get` writes "No resources
// found" to stderr and still exits 0, so a combined-output helper makes an empty result
// indistinguishable from a one-line result, and any caller checking for emptiness or parsing JSON
// silently misreads it. stderr is folded into the error instead, where it is still visible when a
// command genuinely fails.
func kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(stdout)),
			fmt.Errorf("kubectl %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(stdout)), nil
}

func apply(t *testing.T, manifest string) {
	t.Helper()
	if out, err := kubectlWithStdin(manifest, "apply", "-f", "-"); err != nil {
		t.Fatalf("apply failed: %v\n%s", err, out)
	}
}

// applyExpectingRejection applies a manifest that admission should refuse, returning the message.
func applyExpectingRejection(t *testing.T, manifest string) string {
	t.Helper()
	out, err := kubectlWithStdin(manifest, "apply", "-f", "-")
	if err == nil {
		t.Fatalf("admission accepted a manifest it should have rejected:\n%s", manifest)
	}
	return out
}

func kubectlWithStdin(stdin string, args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// deleteContributor removes a contributor without blocking on its finalizer.
//
// kubectl's default wait blocks indefinitely if a finalizer never releases, which turns a product
// bug into an opaque CI hang. Every caller already waits on the effect the deletion should have,
// and that assertion names what it was waiting for.
func deleteContributor(t *testing.T, kind, ns, name string) {
	t.Helper()
	args := []string{"delete", kind, name, "--wait=false"}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	if out, err := kubectl(args...); err != nil {
		t.Fatalf("deleting %s %s: %v\n%s", kind, name, err, out)
	}
}

// requireGone asserts a contributor's finalizer was released and the object actually disappeared.
func requireGone(t *testing.T, kind, ns, name string) {
	t.Helper()
	waitFor(t, kind+" "+name+" to be fully deleted (its finalizer released)", func() bool {
		return !objectExists(kind, ns, name)
	})
}

func clusterReachable() bool {
	_, err := kubectl("version", "--request-timeout=10s")
	return err == nil
}

func waitForOperator() error {
	_, err := kubectl("rollout", "status",
		"-n", operatorNamespace, "deploy/patch-operator-controller-manager", "--timeout=5m")
	return err
}

// waitFor polls until cond returns true.
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

// newNamespace creates a namespace and registers its deletion.
func newNamespace(t *testing.T) string {
	t.Helper()
	name := "e2e-" + strings.ToLower(strings.ReplaceAll(t.Name(), "_", "-"))
	name = strings.ReplaceAll(name, "/", "-")
	if len(name) > 60 {
		name = name[:60]
	}
	if _, err := kubectl("create", "namespace", name); err != nil &&
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("creating namespace: %v", err)
	}
	t.Cleanup(func() {
		// Contributors in this namespace hold finalizers, so a waiting delete could block on the
		// operator; the namespace is reclaimed asynchronously either way.
		_, _ = kubectl("delete", "namespace", name, "--wait=false", "--ignore-not-found")
	})
	return name
}

// configMapData reads a ConfigMap's data, or nil if it does not exist.
func configMapData(ns, name string) map[string]string {
	out, err := kubectl("get", "configmap", name, "-n", ns, "-o", "json")
	if err != nil {
		return nil
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &cm); err != nil {
		return nil
	}
	if cm.Data == nil {
		return map[string]string{}
	}
	return cm.Data
}

// countItems returns how many objects of a kind exist, or -1 if the list could not be read.
//
// Counting decoded items rather than matching lines of `-o name` output: a name substring can
// appear in an unrelated object's name, and "no items" and "one item" are then a single-character
// difference in a string comparison.
func countItems(kind, ns string) int {
	args := []string{"get", kind, "-o", "json"}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	out, err := kubectl(args...)
	if err != nil {
		return -1
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return -1
	}
	return len(list.Items)
}

// getJSON reads an object as JSON and decodes it into out.
func getJSON(kind, ns, name string, out any) error {
	args := []string{"get", kind, name, "-o", "json"}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	raw, err := kubectl(args...)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), out)
}

// annotations returns an object's annotations.
func annotations(kind, ns, name string) map[string]string {
	var obj struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := getJSON(kind, ns, name, &obj); err != nil {
		return nil
	}
	return obj.Metadata.Annotations
}

// conditionStatus returns the status of one condition on a contributor.
//
// Read from JSON rather than a kubectl jsonpath filter: a filter expression with quoted strings is
// awkward to get right through exec, and a quoting bug there is indistinguishable from a real
// product failure.
func conditionStatus(kind, ns, name, condType string) string {
	var obj struct {
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := getJSON(kind, ns, name, &obj); err != nil {
		return ""
	}
	for _, c := range obj.Status.Conditions {
		if c.Type == condType {
			return c.Status
		}
	}
	return ""
}

// clusterTrackersForNamespace returns the ClusterSharedResources whose target is in ns.
func clusterTrackersForNamespace(ns string) []string {
	out, err := kubectl("get", "clustersharedresources", "-o", "json")
	if err != nil {
		return nil
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				TargetRef struct {
					Namespace string `json:"namespace"`
				} `json:"targetRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil
	}
	var names []string
	for _, item := range list.Items {
		if item.Spec.TargetRef.Namespace == ns {
			names = append(names, item.Metadata.Name)
		}
	}
	return names
}

func objectExists(kind, ns, name string) bool {
	args := []string{"get", kind, name, "-o", "name"}
	if ns != "" {
		args = append(args, "-n", ns)
	}
	_, err := kubectl(args...)
	return err == nil
}

func ingressHosts(ns, name string) map[string]bool {
	var obj struct {
		Spec struct {
			Rules []struct {
				Host string `json:"host"`
			} `json:"rules"`
		} `json:"spec"`
	}
	hosts := map[string]bool{}
	if err := getJSON("ingress", ns, name, &obj); err != nil {
		return hosts
	}
	for _, r := range obj.Spec.Rules {
		hosts[r.Host] = true
	}
	return hosts
}

func contributorReady(kind, ns, name string) bool {
	return conditionStatus(kind, ns, name, "Ready") == "True"
}

// --- manifests ---

func cmContributor(ns, name, target, key, value string, create bool) string {
	lifecycle := "    onMissing: Wait\n    onRelease: Revert"
	base := ""
	if create {
		lifecycle = "    onMissing: Create\n    onRelease: Revert"
		base = fmt.Sprintf(`
  base:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: %s`, target)
	}
	return fmt.Sprintf(`
apiVersion: terasky.com/v1alpha1
kind: ResourcePatch
metadata:
  name: %s
  namespace: %s
spec:
  target:
    mode: Single
    apiVersion: v1
    kind: ConfigMap
    name: %s
  lifecycle:
%s
  apply:
    mode: ServerSideApply
    conflictPolicy: Fail
  priority: 100%s
  patch:
    type: StrategicMerge
    value:
      data:
        %s: "%s"
`, name, ns, target, lifecycle, base, key, value)
}

// --- tests ---

// Appendix A.1, end to end against a deployed operator: two contributors in one namespace share a
// ConfigMap, one of them creating it.
func TestSharedTargetWithTwoContributors(t *testing.T) {
	ns := newNamespace(t)

	apply(t, cmContributor(ns, "creator", "shared", "key-a", "value-a", true))
	apply(t, cmContributor(ns, "patcher", "shared", "key-b", "value-b", false))

	waitFor(t, "both contributions to land", func() bool {
		d := configMapData(ns, "shared")
		return d["key-a"] == "value-a" && d["key-b"] == "value-b"
	})

	for _, name := range []string{"creator", "patcher"} {
		waitFor(t, name+" to report Ready", func() bool {
			return contributorReady("resourcepatch", ns, name)
		})
	}

	// One tracker, in the contributors' namespace.
	waitFor(t, "a SharedResource to track the target", func() bool {
		return countItems("sharedresources", ns) == 1
	})
}

// Appendix A.2: the creator leaves first and the object must survive for the remaining
// contributor. This is where lead/follower designs break.
func TestObjectSurvivesItsCreator(t *testing.T) {
	ns := newNamespace(t)

	apply(t, cmContributor(ns, "creator", "shared", "key-a", "value-a", true))
	apply(t, cmContributor(ns, "patcher", "shared", "key-b", "value-b", false))
	waitFor(t, "both contributions to land", func() bool {
		d := configMapData(ns, "shared")
		return d["key-a"] == "value-a" && d["key-b"] == "value-b"
	})

	deleteContributor(t, "resourcepatch", ns, "creator")

	waitFor(t, "the creator's field to be withdrawn", func() bool {
		_, present := configMapData(ns, "shared")["key-a"]
		return !present
	})

	if !objectExists("configmap", ns, "shared") {
		t.Fatal("the object was deleted when its creator left, while another contributor remained")
	}
	if d := configMapData(ns, "shared"); d["key-b"] != "value-b" {
		t.Errorf("the remaining contribution was disturbed: %#v", d)
	}
	// The finalizer must actually have been released, not merely the field withdrawn.
	requireGone(t, "resourcepatch", ns, "creator")
	consistently(t, 10*time.Second, "the object to keep existing", func() bool {
		return objectExists("configmap", ns, "shared")
	})
}

// Appendix A.3, the case that justifies ClientSideApply: two contributors both append to
// Ingress.spec.rules, which is atomic and cannot be co-owned under server-side apply.
func TestAtomicListSharedUnderClientSideApply(t *testing.T) {
	ns := newNamespace(t)

	rule := func(name, host, svc string, create bool) string {
		lifecycle := "    onMissing: Wait\n    onRelease: Revert"
		base := ""
		if create {
			lifecycle = "    onMissing: Create\n    onRelease: Revert"
			base = `
  base:
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: shared-ingress
    spec:
      ingressClassName: nginx`
		}
		return fmt.Sprintf(`
apiVersion: terasky.com/v1alpha1
kind: ResourcePatch
metadata:
  name: %s
  namespace: %s
spec:
  target:
    mode: Single
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    name: shared-ingress
  lifecycle:
%s
  apply:
    mode: ClientSideApply
    conflictPolicy: Fail
  priority: 100%s
  patch:
    type: StrategicMerge
    mergeKeys:
      - path: spec.rules
        key: host
    value:
      spec:
        rules:
          - host: %s
            http:
              paths:
                - path: /
                  pathType: Prefix
                  backend:
                    service:
                      name: %s
                      port:
                        number: 80
`, name, ns, lifecycle, base, host, svc)
	}

	apply(t, rule("rule-a", "a.example.com", "svc-a", true))
	apply(t, rule("rule-b", "b.example.com", "svc-b", false))

	waitFor(t, "both rules to land on the atomic list", func() bool {
		h := ingressHosts(ns, "shared-ingress")
		return h["a.example.com"] && h["b.example.com"]
	})

	// A withdraws; only its rule may go.
	deleteContributor(t, "resourcepatch", ns, "rule-a")
	waitFor(t, "A's rule to be withdrawn", func() bool {
		return !ingressHosts(ns, "shared-ingress")["a.example.com"]
	})
	if !ingressHosts(ns, "shared-ingress")["b.example.com"] {
		t.Error("A's revert took the whole list, removing B's rule")
	}
	requireGone(t, "resourcepatch", ns, "rule-a")
}

// Appendix A.4: a namespaced target gains a cluster-scoped contributor and is promoted.
func TestPromotionOnClusterContributor(t *testing.T) {
	ns := newNamespace(t)

	apply(t, cmContributor(ns, "namespaced", "shared", "key-a", "value-a", true))
	waitFor(t, "the namespaced contribution to land", func() bool {
		return configMapData(ns, "shared")["key-a"] == "value-a"
	})

	clusterPatch := fmt.Sprintf(`
apiVersion: terasky.com/v1alpha1
kind: ClusterResourcePatch
metadata:
  name: e2e-promotion-%s
spec:
  target:
    mode: Single
    apiVersion: v1
    kind: ConfigMap
    name: shared
    namespace: %s
  lifecycle:
    onMissing: Wait
    onRelease: Revert
  apply:
    mode: ServerSideApply
  priority: 100
  patch:
    type: StrategicMerge
    value:
      data:
        key-c: value-c
`, ns, ns)
	apply(t, clusterPatch)
	t.Cleanup(func() {
		_, _ = kubectl("delete", "clusterresourcepatch", "e2e-promotion-"+ns,
			"--ignore-not-found", "--wait=false")
	})

	waitFor(t, "the cluster-scoped contribution to land", func() bool {
		return configMapData(ns, "shared")["key-c"] == "value-c"
	})

	// The namespaced tracker must be retired, leaving exactly one writer.
	waitFor(t, "the namespaced tracker to be retired", func() bool {
		return countItems("sharedresources", ns) == 0
	})
	waitFor(t, "a ClusterSharedResource to own the target", func() bool {
		return len(clusterTrackersForNamespace(ns)) == 1
	})

	// The existing contribution survived the migration untouched.
	if d := configMapData(ns, "shared"); d["key-a"] != "value-a" {
		t.Errorf("the migration disturbed the existing contribution: %#v", d)
	}
}

// The webhooks are actually serving, and reject a spec the API cannot honour. envtest cannot show
// this: without the webhook the reconciler reports the same thing in status instead.
func TestAdmissionRejectsCrossNamespaceResourcePatch(t *testing.T) {
	ns := newNamespace(t)

	manifest := fmt.Sprintf(`
apiVersion: terasky.com/v1alpha1
kind: ResourcePatch
metadata:
  name: cross-namespace
  namespace: %s
spec:
  target:
    mode: Single
    apiVersion: v1
    kind: ConfigMap
    name: shared
    namespace: kube-system
  lifecycle:
    onMissing: Wait
    onRelease: Revert
  patch:
    type: StrategicMerge
    value:
      data:
        injected: "yes"
`, ns)

	out := applyExpectingRejection(t, manifest)
	if !strings.Contains(out, "own namespace") {
		t.Errorf("the rejection should explain the containment rule, got:\n%s", out)
	}
	// And nothing was written to the namespace it tried to reach.
	if configMapData("kube-system", "shared") != nil {
		t.Error("a rejected cross-namespace ResourcePatch still reached kube-system")
	}
}

// onRelease: Delete without onMissing: Create is the configuration that would let a patch-only
// contributor attach to a production object and remove it on the way out.
func TestAdmissionRejectsDeleteWithoutCreate(t *testing.T) {
	ns := newNamespace(t)

	manifest := fmt.Sprintf(`
apiVersion: terasky.com/v1alpha1
kind: ResourcePatch
metadata:
  name: delete-without-create
  namespace: %s
spec:
  target:
    mode: Single
    apiVersion: v1
    kind: ConfigMap
    name: shared
  lifecycle:
    onMissing: Wait
    onRelease: Delete
  patch:
    type: StrategicMerge
    value:
      data:
        injected: "yes"
`, ns)

	out := applyExpectingRejection(t, manifest)
	if !strings.Contains(out, "onMissing: Create") {
		t.Errorf("the rejection should name the required pairing, got:\n%s", out)
	}
}

// The admitting principal is recorded so the controller can re-run its SubjectAccessReview on a
// TTL. Admission is point-in-time; without this a contributor nothing ever touches again would
// keep writing after its author lost the rights.
func TestAdmissionRecordsAuthorizedPrincipal(t *testing.T) {
	ns := newNamespace(t)

	apply(t, cmContributor(ns, "recorded", "shared", "k", "v", true))
	waitFor(t, "the authorized-as annotation to be recorded", func() bool {
		return annotations("resourcepatch", ns, "recorded")["terasky.com/authorized-as"] != ""
	})
}

// Both CRD pairs must be installed and correctly scoped in a real cluster.
func TestCRDsInstalledWithCorrectScopes(t *testing.T) {
	tests := map[string]string{
		"resourcepatches.terasky.com":        "Namespaced",
		"sharedresources.terasky.com":        "Namespaced",
		"clusterresourcepatches.terasky.com": "Cluster",
		"clustersharedresources.terasky.com": "Cluster",
	}
	for name, wantScope := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := kubectl("get", "crd", name, "-o", "jsonpath={.spec.scope}")
			if err != nil {
				t.Fatalf("CRD %s is not installed: %v", name, err)
			}
			if got != wantScope {
				t.Errorf("scope = %q, want %q", got, wantScope)
			}
		})
	}
}
