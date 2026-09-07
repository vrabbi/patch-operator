# The shared-resource problem

A shared resource is one that **no single instantiation owns**. Each owns a slice of its fields.

Three properties follow, and together they are what no existing tool provides:

1. The object must exist as long as **at least one** contributor exists.
2. Each contributor's fields must disappear when **that** contributor does.
3. Removing one contributor must not disturb **anyone else's** fields.

## Why `ownerReferences` are not enough

This is the first thing anyone reaches for, and it deserves a precise answer rather than a
dismissal. Kubernetes garbage collection genuinely reference-counts: a dependent with several
owners is deleted only when *all* owners are gone.

**Cross-namespace contribution rules it out entirely.** A namespaced dependent's owners must live in
the same namespace, so a shared object in `platform` cannot be owned by contributors in `team-a` and
`team-b`. A cluster-scoped dependent cannot be owned by a namespaced object at all.

**Same-namespace contribution is the case where GC would actually work.** Contributors, target and
tracker co-located in one namespace is exactly the shape GC handles — and it is precisely the shape
of this design's namespaced pair. The objection above does not apply there.

**But GC is delete-or-don't, in both cases.** There is no "remove my fields and leave the object"
semantic, which is what a patch-only contributor actually needs.

**And GC deletion is unconditional** once the last owner goes, silently overriding whatever policy
the contributor expressed. `onRelease: Orphan` and `onRelease: Revert` are the two most common
policies, and GC ignores both.

So reference counting has to be explicit, in an object the operator controls — for the
cross-namespace case because GC cannot express it, and for the same-namespace case because GC would
express it **wrongly**.

!!! info "The ownership chain that does work"
    ```
    XR / kro Instance --ownerRef--> ResourcePatch        --tracked by--> SharedResource        --manages--> target
    XR / kro Instance --ownerRef--> ClusterResourcePatch --tracked by--> ClusterSharedResource --manages--> target
    ```
    Deletion propagates *in* from the orchestrator by ordinary GC, and is *arbitrated* by the
    operator. Each layer does what it is good at. This operator puts no `ownerReferences` on
    targets.

## Why server-side apply is not enough either

SSA's `managedFields` is the right model for multi-writer field ownership, and this design leans on
it hard. What it does not provide:

- **Lifecycle.** Create the object if nobody has yet; delete it when the last contributor leaves.
- **Any record of who the contributors are.** There is no reference count to read.
- **Arbitration** when two contributors legitimately want the same field. SSA reports the conflict;
  something has to decide.
- **Any answer for atomic lists.** A list without `listType=map` is owned wholesale by one manager,
  so two contributors cannot both append to it. See [Apply modes](apply-modes.md).

## The reconcile unit is the target, not the request

The single most important structural decision: **contributors are inputs, and the tracker is the
unit of reconciliation.** The tracker controller is the only code path that writes to a target.

The naive alternative — one controller per contributor, each writing its own slice — is what
`provider-kubernetes` does today, and it is why that approach needs blind conflict forcing. With N
contributors writing independently you get:

- N-way races on one object,
- non-deterministic apply order,
- no place to notice that two contributors want the same field,
- and no coherent moment at which to decide the object is now unreferenced.

Aggregating first fixes all four at once. Writes to one target are serialised through one work-queue
key; apply order is a deterministic sort (`priority` desc, then `creationTimestamp`, then `uid`)
stable across restarts and replicas; conflicts are detected before any write, because every
contribution is in hand; and the reference count is a list length evaluated at one point in the code.

!!! note "Why the `uid` tiebreak matters"
    Two contributors created in the same clock tick must still sort identically on every replica and
    after every restart. Without the tiebreak the target flaps as leadership moves.

## Non-goals

- **Not a policy engine.** Contributions are declared per instantiation, not by cluster-wide rules
  over a match set. Kyverno already does the latter well.
- **Not a replacement for `Object` or kro.** It is designed to be *emitted by* them. An XR still
  uses `Object` for the resources it solely owns.
- **Not a templating language.** Value interpolation is the orchestrator's job — Crossplane patches
  and kro CEL are already good at it.
- **Not for objects with a single owner.** If one thing owns the object, use `Object`.
