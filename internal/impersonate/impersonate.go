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

// Package impersonate builds clients that write as a tenant's ServiceAccount rather than as the
// operator.
//
// This is the second authorization layer, and the one that actually bounds a patch emitted by
// Crossplane or kro. A SubjectAccessReview at admission checks the *orchestrator's* ServiceAccount,
// which is typically close to cluster-admin; impersonation moves enforcement to write time under
// an identity the platform team chose, and the API server applies that identity's RBAC to every
// write rather than once at creation.
package impersonate

import (
	"fmt"
	"sync"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	patchv1alpha1 "github.com/vrabbi/patch-operator/api/v1alpha1"
)

// ServiceAccountUsername renders the API server username for a ServiceAccount.
func ServiceAccountUsername(namespace, name string) string {
	return fmt.Sprintf("system:serviceaccount:%s:%s", namespace, name)
}

// Factory hands out clients that impersonate a ServiceAccount, caching them so a per-reconcile
// lookup does not rebuild a REST client and its connection pool every pass.
type Factory struct {
	// BaseConfig is the operator's own rest config, cloned per impersonated identity.
	BaseConfig *rest.Config
	// Options are passed to every constructed client; carries the scheme and mapper.
	Options client.Options
	// Default is returned when a contributor names no ServiceAccount, so callers need no branch.
	Default client.Client

	mu     sync.RWMutex
	cached map[string]client.Client
}

// NewFactory builds a Factory.
func NewFactory(cfg *rest.Config, opts client.Options, base client.Client) *Factory {
	return &Factory{
		BaseConfig: rest.CopyConfig(cfg),
		Options:    opts,
		Default:    base,
		cached:     map[string]client.Client{},
	}
}

// For returns the client to write a contributor's target with.
//
// With no serviceAccountRef the operator's own identity is used, bounded only by the admission
// SubjectAccessReview. That is the weaker configuration, and the docs recommend against it on any
// cluster with more than one tenant.
//
// A namespaced contributor's ServiceAccount is always resolved in its own namespace, never the one
// named in the spec. Trusting the spec here would let a ResourcePatch impersonate out of its
// namespace and defeat the containment property that makes the namespaced kind safe to grant.
func (f *Factory) For(c patchv1alpha1.Contributor) (client.Client, string, error) {
	ref := c.GetPatchSpec().ServiceAccountRef
	if ref == nil || ref.Name == "" {
		return f.Default, "", nil
	}

	namespace := ref.Namespace
	if !c.IsClusterScoped() {
		namespace = c.GetNamespace()
	}
	if namespace == "" {
		return nil, "", fmt.Errorf("serviceAccountRef.namespace is required on a cluster-scoped contributor")
	}

	username := ServiceAccountUsername(namespace, ref.Name)

	f.mu.RLock()
	cached, ok := f.cached[username]
	f.mu.RUnlock()
	if ok {
		return cached, username, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if cached, ok := f.cached[username]; ok {
		return cached, username, nil
	}

	cfg := rest.CopyConfig(f.BaseConfig)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: username}

	// An impersonating client must not read through the manager's shared cache: the cache is
	// populated with the operator's own credentials, so a cached read would silently bypass the
	// tenant's RBAC and report data the tenant cannot see.
	opts := f.Options
	opts.Cache = nil

	built, err := client.New(cfg, opts)
	if err != nil {
		return nil, "", fmt.Errorf("building an impersonating client for %s: %w", username, err)
	}
	f.cached[username] = built
	return built, username, nil
}
