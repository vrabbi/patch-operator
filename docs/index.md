# patch-operator

**Reference-counted, revertible, multi-writer field contribution for shared Kubernetes
resources.**

!!! warning "Status: pre-alpha (v1alpha1)"
    The API will change. Not yet recommended for production.

## The problem

Platform teams compose infrastructure with orchestrators that instantiate a graph of resources per
claim — a Crossplane XR, a kro `Instance`, a Helm release. Each instantiation owns its own
resources cleanly. What none of them handle well is the resource that is **shared across
instantiations**:

- A shared `Ingress` where every application adds one rule.
- A shared `NetworkPolicy` where every tenant adds one peer.
- A shared `ConfigMap` — a routing table, an allow-list — where each instance contributes one key.
- A shared `ClusterRole` aggregating rules contributed per tenant.

The defining property is that **no single instantiation owns the object**. Each owns a *slice of
its fields*. The object must exist as long as at least one contributor exists, and each
contributor's fields must disappear when that contributor does — without disturbing anyone else's.

## What exists already, and where it stops

| System | What it gives | The gap |
|---|---|---|
| Crossplane `provider-kubernetes` `Object` | The closest existing answer. Several `Object`s can patch one target with partial manifests. | Its own docs list the limits: it **forces every field conflict**, it **cannot remove its partial fields on delete**, and it requires field sets to be **disjoint** with no way to coordinate that. No create-on-missing → delete-on-last-release lifecycle. |
| kro `externalRef` | Reads a shared resource into an RGD. | Strictly read-only. It solves *referencing* a shared resource, never *contributing* to one. |
| Kyverno mutate-existing | Background mutation of existing resources. | Policy-shaped: a cluster-wide rule over a match set, not a per-instantiation contribution. No reference counting, no revert. |
| Kubernetes GC, multi-`ownerReferences` | True reference counting, built in. | Same-namespace only; no revert; deletes unconditionally, overriding any policy the contributor expressed. |

## What this adds

Four CRDs in `terasky.com/v1alpha1`, in two scope-matched pairs that mirror `Role`/`ClusterRole`.
Users write only the **contributors**; the **trackers** are operator-owned.

| CRD | Scope | May target | Tracked by |
|---|---|---|---|
| **`ResourcePatch`** | namespaced | namespaced kinds, **its own namespace only** | `SharedResource` in that namespace |
| **`ClusterResourcePatch`** | cluster | any namespace, and cluster-scoped kinds | `ClusterSharedResource` |

Two decisions carry most of the weight:

**The tracker is the only writer to its target.** Contributors are inputs; the tracker is the unit
of reconciliation. That makes writes serialised per object, apply order a deterministic sort stable
across restarts, conflicts detectable *before* any write because every contribution is in hand at
once, and the reference count a list length evaluated at a single point.

**A `ResourcePatch` cannot reach outside its namespace.** Not "is not permitted to" — *cannot*.
That is structural, so it holds regardless of how privileged the principal creating it is. See
[Security](security.md), which is the page to read first.

## A worked example

Two applications in `team-a` share one Ingress. One creates it; the other only adds a rule.

```yaml
apiVersion: terasky.com/v1alpha1
kind: ResourcePatch
metadata:
  name: checkout-ingress-rule
  namespace: team-a
spec:
  target:
    mode: Single
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    name: team-a-shared-ingress    # resolved in team-a; nothing else is expressible
  lifecycle:
    onMissing: Create              # this contributor seeds the object
    onRelease: Revert              # ...and withdraws only its own fields on the way out
  apply:
    mode: ClientSideApply          # spec.rules is an atomic list; see Apply modes
    conflictPolicy: Fail
  base:
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: team-a-shared-ingress
    spec:
      ingressClassName: nginx
  patch:
    type: StrategicMerge
    mergeKeys:
      - path: spec.rules
        key: host                  # "my rule", not "whatever is first"
    value:
      spec:
        rules:
          - host: checkout.team-a.example.com
            http:
              paths:
                - path: /
                  pathType: Prefix
                  backend:
                    service: { name: checkout, port: { number: 80 } }
```

Delete this `ResourcePatch` and its rule disappears. The Ingress survives for as long as any other
contributor needs it — **including when this creating contributor is the one that left first**.

## Where to go next

- **[Security](security.md)** — read this first. This is an "apply arbitrary fields to arbitrary
  objects" primitive, and the authorization model is the hard part.
- **[The shared-resource problem](concepts/shared-resources.md)** — why `ownerReferences` are not
  enough.
- **[Apply modes](concepts/apply-modes.md)** — why client-side apply is not a legacy fallback.
- **[Install](guides/install.md)** — including the namespace-only install.
