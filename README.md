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

Two CRDs in `terasky.com/v1alpha1`:

- **`ResourcePatch`** (namespaced) — one per contributing instantiation, emitted by a Composition or
  RGD. Declares the target, what to contribute, what to do if the target is missing, and what to do
  on release.
- **`SharedResource`** (cluster-scoped) — operator-owned tracker, one per target object. Holds the
  contributor list (the reference count) and is the *only* writer to the target.

Both server-side apply (default) and client-side apply are supported — the latter is not a legacy
mode but the one that makes targets with atomic list fields, `Ingress.spec.rules` among them, work at
all.

## Read next

- **[DESIGN.md](./DESIGN.md)** — the full design. Start at §7 (Authorization): this is an
  "apply arbitrary fields to arbitrary objects" primitive, and that is the hard part, not the merging.
- **[examples/](./examples/)** — contributor manifests and how Crossplane and kro emit them.
