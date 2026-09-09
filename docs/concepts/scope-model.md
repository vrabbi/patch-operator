# Scope model

Four CRDs in two scope-matched pairs, mirroring `Role`/`ClusterRole`.

| CRD | Scope | May target | Contributors live | Tracked by |
|---|---|---|---|---|
| `ResourcePatch` | namespaced | namespaced kinds, **its own namespace only** | all in that one namespace | `SharedResource` in that namespace |
| `ClusterResourcePatch` | cluster | any namespace, and cluster-scoped kinds | anywhere | `ClusterSharedResource` |

**The two contributor kinds share an identical spec.** This is one API with two reaches, not two
APIs — same `target`, `lifecycle`, `apply`, `priority`, `serviceAccountRef`, `base` and `patch`. The
implementation is one generic reconciler instantiated twice, so the split costs a validation table
and a tracker-selection rule rather than a second copy of the logic.

## What differs

| | `ResourcePatch` | `ClusterResourcePatch` |
|---|---|---|
| **target kind** | must be a **namespaced** kind | namespaced *or* cluster-scoped |
| **`target.namespace`** | optional; defaults to the CR's namespace; **any other value is rejected** | required for namespaced kinds, forbidden for cluster-scoped ones |
| **`namespaceSelector`** | forbidden | allowed |
| **`serviceAccountRef.namespace`** | forbidden — always the CR's own namespace | allowed; gated by an `impersonate` SAR |
| **`maxTargets`** | a guard rail; the namespace already bounds the fan-out | the real blast-radius control |
| **Typical RBAC grant** | tenant namespaces, freely | platform team only |

The namespaced constraints are enforced **at admission and re-checked at reconcile**, so a
`ResourcePatch` cannot escape its namespace even if its spec reached the cluster by a path that
bypassed the webhook.

## Tracker selection

**Exactly one tracker owns a target.** The single-writer invariant is what the whole architecture
rests on, so the tracker for a given target is determined by a rule, not by whichever controller got
there first:

1. **Cluster-scoped target** → always a `ClusterSharedResource`.
2. **Namespaced target, all contributors are `ResourcePatch`** → a `SharedResource` in the target's
   namespace. By construction that is also every contributor's namespace.
3. **Namespaced target with at least one `ClusterResourcePatch`** → a `ClusterSharedResource`,
   reached by [promotion](promotion.md).

!!! info "Why rule 3 exists"
    A namespaced tracker cannot honestly represent a contributor that is not in its namespace: its
    reference count would be incomplete, and **an incomplete reference count deletes objects that
    are still in use.**

Because the rule is a function of the contributor set rather than of arrival order, two contributors
racing to create a tracker reach the same answer whoever wins. Tracker names are deterministic for
the same reason — both racers compute the same name, so the loser's `AlreadyExists` is a normal
outcome rather than an error.

## The trackers

Operator-owned; users do not create them.

`SharedResource` lives **in the target's namespace**, which for this variant is also every
contributor's namespace. `ClusterSharedResource` is cluster-scoped and tracks every cluster-scoped
target plus every namespaced target that has at least one cluster-scoped contributor.

Both carry the same status: the contributor list (which **is** the reference count), the recorded
creator, per-contributor revert bookkeeping, and any unresolved conflicts.

```bash
# Who is writing to this object?
kubectl get sharedresources -n team-a
kubectl get clustersharedresources
```

!!! note "The contributor list is derived state"
    It is rebuilt from a live list on every reconcile rather than trusted from status, so a missed
    watch event or a lost update self-heals instead of corrupting the count.
