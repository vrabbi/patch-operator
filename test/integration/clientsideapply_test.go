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

package integration

import (
	"context"
	"fmt"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
	"github.com/vrabbi/patch-operator/internal/scope"
)

// ClientSideApply exists for exactly this case, so it needs coverage against a real API server.
//
// Ingress.spec.rules is an atomic list: server-side apply gives whole-list ownership to one field
// manager, so two contributors cannot each own a rule. Client-side apply merges on a declared key
// instead, and revert removes only the keyed element. Every property here is about that seam --
// which is why the whole suite ran green while the kind e2e job found the revert path broken.

// ingressPatch builds a ClientSideApply ResourcePatch contributing one keyed rule.
func ingressPatch(ns, name, targetName, host, svc string, create bool) *patchv1alpha1.ResourcePatch {
	rp := &patchv1alpha1.ResourcePatch{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: patchv1alpha1.ResourcePatchSpec{
			Target: patchv1alpha1.TargetRef{
				Mode:       patchv1alpha1.TargetModeSingle,
				APIVersion: "networking.k8s.io/v1",
				Kind:       "Ingress",
				Name:       targetName,
			},
			Lifecycle: patchv1alpha1.LifecycleSpec{
				OnMissing:     patchv1alpha1.OnMissingWait,
				OnRelease:     patchv1alpha1.OnReleaseRevert,
				AdoptExisting: ptrTrue(),
				BaseReconcile: patchv1alpha1.BaseReconcileCreateOnly,
			},
			Apply: patchv1alpha1.ApplySpec{
				Mode:           patchv1alpha1.ApplyModeClientSideApply,
				ConflictPolicy: patchv1alpha1.ConflictPolicyFail,
			},
			Priority: 100,
			Patch: patchv1alpha1.PatchSpec{
				Type:      patchv1alpha1.PatchTypeStrategicMerge,
				MergeKeys: []patchv1alpha1.MergeKey{{Path: "spec.rules", Key: "host"}},
				Value:     raw(ingressRuleJSON(host, svc)),
			},
		},
	}
	if create {
		rp.Spec.Lifecycle.OnMissing = patchv1alpha1.OnMissingCreate
		rp.Spec.Base = raw(fmt.Sprintf(
			`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":%q},`+
				`"spec":{"ingressClassName":"nginx"}}`, targetName))
	}
	return rp
}

func ingressRuleJSON(host, svc string) string {
	return fmt.Sprintf(`{"spec":{"rules":[{"host":%q,"http":{"paths":[{"path":"/",`+
		`"pathType":"Prefix","backend":{"service":{"name":%q,"port":{"number":80}}}}]}}]}}`,
		host, svc)
}

func ingressTracker(ns, targetName string) client.ObjectKey {
	key := scope.TargetKey{
		APIVersion: "networking.k8s.io/v1",
		Kind:       "Ingress",
		Namespace:  ns,
		Name:       targetName,
	}
	return client.ObjectKey{Namespace: ns, Name: scope.TrackerName(key)}
}

// ingressHosts returns the hosts present on the target's rules.
func ingressHosts(ctx context.Context, ns, name string) map[string]bool {
	hosts := map[string]bool{}
	ing := &networkingv1.Ingress{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, ing); err != nil {
		return hosts
	}
	for _, r := range ing.Spec.Rules {
		hosts[r.Host] = true
	}
	return hosts
}

// Appendix A.3: two contributors append to an atomic list, then one withdraws. Only its own rule
// may go, and the object must not be rewritten out from under the other.
func TestClientSideApplySharesAnAtomicList(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)
	dumpOnFailure(t, ctx, ns)

	ruleA := ingressPatch(ns, "rule-a", "shared-ingress", "a.example.com", "svc-a", true)
	ruleB := ingressPatch(ns, "rule-b", "shared-ingress", "b.example.com", "svc-b", false)

	if err := k8sClient.Create(ctx, ruleA); err != nil {
		t.Fatalf("creating rule-a: %v", err)
	}
	if err := k8sClient.Create(ctx, ruleB); err != nil {
		t.Fatalf("creating rule-b: %v", err)
	}

	waitFor(t, "both rules to land on the atomic list", func() bool {
		h := ingressHosts(ctx, ns, "shared-ingress")
		return h["a.example.com"] && h["b.example.com"]
	})

	// Both must be recorded, or the reference count is wrong and a release deletes too much.
	waitFor(t, "the tracker to record both contributors", func() bool {
		sr := &patchv1alpha1.SharedResource{}
		if err := k8sClient.Get(ctx, ingressTracker(ns, "shared-ingress"), sr); err != nil {
			return false
		}
		return sr.Status.ContributorCount == 2
	})

	if err := k8sClient.Delete(ctx, ruleA); err != nil {
		t.Fatalf("deleting rule-a: %v", err)
	}

	waitFor(t, "rule-a's rule to be withdrawn from the list", func() bool {
		return !ingressHosts(ctx, ns, "shared-ingress")["a.example.com"]
	})
	if !ingressHosts(ctx, ns, "shared-ingress")["b.example.com"] {
		t.Error("reverting one contributor took the whole atomic list, removing the other's rule")
	}

	// The finalizer must be released, not merely the field withdrawn: a contributor that cannot be
	// deleted after its revert succeeded is stuck forever.
	waitFor(t, "rule-a to be fully deleted, its finalizer released", func() bool {
		rp := &patchv1alpha1.ResourcePatch{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "rule-a"}, rp)
		return apierrors.IsNotFound(err)
	})

	// The object survives its creator, exactly as under server-side apply.
	ing := &networkingv1.Ingress{}
	if err := k8sClient.Get(ctx,
		client.ObjectKey{Namespace: ns, Name: "shared-ingress"}, ing); err != nil {
		t.Fatalf("the target was deleted when its creator withdrew: %v", err)
	}
}

// A rule an operator added by hand before any contributor arrived must be restored on revert, not
// deleted: that is what priorValues is for, and it is the difference between a revert and a wipe.
func TestClientSideApplyRestoresPriorValueOnRevert(t *testing.T) {
	skipWithoutEnvtest(t)
	ctx := context.Background()
	ns := newNamespace(t, ctx)
	dumpOnFailure(t, ctx, ns)

	// A pre-existing Ingress with a rule the operator does not own.
	existing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "adopted", Namespace: ns},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: "pre-existing.example.com"}},
		},
	}
	if err := k8sClient.Create(ctx, existing); err != nil {
		t.Fatalf("creating the pre-existing Ingress: %v", err)
	}

	contributor := ingressPatch(ns, "adder", "adopted", "added.example.com", "svc-added", false)
	if err := k8sClient.Create(ctx, contributor); err != nil {
		t.Fatalf("creating the contributor: %v", err)
	}

	waitFor(t, "the contribution to be merged into the existing list", func() bool {
		h := ingressHosts(ctx, ns, "adopted")
		return h["added.example.com"] && h["pre-existing.example.com"]
	})

	if err := k8sClient.Delete(ctx, contributor); err != nil {
		t.Fatalf("deleting the contributor: %v", err)
	}
	waitFor(t, "the contributed rule to be withdrawn", func() bool {
		return !ingressHosts(ctx, ns, "adopted")["added.example.com"]
	})

	if !ingressHosts(ctx, ns, "adopted")["pre-existing.example.com"] {
		t.Error("the revert removed a rule the operator never contributed")
	}
}
