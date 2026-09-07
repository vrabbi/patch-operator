# patch-operator

**Status: pre-alpha — design only. No implementation yet.**

A Kubernetes operator for the **shared resource** problem: many independent orchestrator
instantiations — Crossplane XRs, kro Instances, Helm releases — each needing to contribute a slice of
configuration to *one* object that none of them owns.

A shared `Ingress` where every app adds a rule. A shared `NetworkPolicy` where every tenant adds a
peer. A shared `ConfigMap` where each instance contributes one key. The object must exist while at
least one contributor exists, and each contributor's fields must disappear when that contributor
does — without disturbing anyone else's.

Existing tools each solve part of this. Crossplane's `provider-kubernetes` `Object` can patch a
shared resource but forces every field conflict, cannot remove its fields on delete, and has no
create-on-missing/delete-on-last-release lifecycle. kro's `externalRef` is read-only. Kyverno's
mutate-existing is policy-shaped with no reference counting. Kubernetes GC with multiple
`ownerReferences` is real reference counting but is same-namespace-only and offers no revert.

The missing primitive is **reference-counted, revertible, multi-writer field contribution with
explicit conflict arbitration**.

## Shape

Four CRDs in `terasky.com/v1alpha1`, in two scope-matched pairs that mirror `Role`/`ClusterRole`.
Users write only the *contributors*; the *trackers* are operator-owned.

| CRD | Scope | May target | Tracked by |
|---|---|---|---|
| **`ResourcePatch`** | namespaced | namespaced kinds, **its own namespace only** | `SharedResource` in that namespace |
| **`ClusterResourcePatch`** | cluster | any namespace, and cluster-scoped kinds | `ClusterSharedResource` |

A contributor declares the target, what to contribute, what to do if the target is missing, and what
to do on release. A tracker holds the contributor list — the reference count — and is the *only*
writer to its target. Exactly one tracker owns any given object.

Both server-side apply (default) and client-side apply are supported — the latter is not a legacy
mode but the one that makes targets with atomic list fields, `Ingress.spec.rules` among them, work at
all.

## On multi-tenancy

This is an "apply arbitrary fields to arbitrary objects" primitive, so authorization is the hard part
— not the merging. The scope split is the strongest control, because it is **structural rather than
an authorization check**: a `ResourcePatch` *cannot* reach outside its namespace regardless of how
privileged the principal creating it is.

That distinction matters in practice. A patch emitted by a Crossplane Composition is admitted as
Crossplane's ServiceAccount, which is typically close to cluster-admin — so every check that reasons
about the requester is weak for exactly the patches that matter most. Containment does not reason
about the requester at all.

So: **grant `ResourcePatch` freely** in tenant namespaces, and **treat `ClusterResourcePatch` as a
platform-team grant**. A namespace-only install — omitting the cluster CRDs entirely — needs no
cluster-wide write RBAC. SubjectAccessReview on every mutation and optional ServiceAccount
impersonation back this up; see §7.

## Quick start

```bash
# cert-manager issues the webhook's serving certificate
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=5m

make deploy IMG=ghcr.io/vrabbi/patch-operator:latest

# Or, when no shared object is ever contributed to from outside its own namespace:
# omits the cluster-scoped CRDs entirely and needs no cluster-wide write RBAC.
make deploy-namespaced-only IMG=ghcr.io/vrabbi/patch-operator:latest
```

## Development

```bash
make test          # unit + envtest integration tests
make lint          # golangci-lint, built with the toolchain pinned from go.mod
make docs-build    # mkdocs build --strict
make test-e2e      # requires a Kind cluster with the operator deployed
```

`make test` resolves envtest binaries with `setup-envtest`; no cluster is needed. The e2e suite
needs a real cluster and is run by CI.

## Read next

- **[Documentation site](https://vrabbi.github.io/patch-operator/)** — concepts, guides and the
  generated API reference.
- **[Security](https://vrabbi.github.io/patch-operator/security/)** — read this before deploying.
  This is an "apply arbitrary fields to arbitrary objects" primitive, and the authorization model is
  the hard part.
- **[DESIGN.md](./DESIGN.md)** — the full design and the reasoning behind it.
- **[examples/namespaced/](./examples/namespaced/)** — the contained, low-privilege pattern.
- **[examples/cluster/](./examples/cluster/)** — cross-namespace and cluster-scoped targets.
- **[examples/](./examples/)** — how Crossplane and kro emit each kind.
