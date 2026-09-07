# Selector mode

Selector mode patches **every object matching a selector**, rather than one named object.

```yaml
spec:
  target:
    mode: Selector
    apiVersion: networking.k8s.io/v1
    kind: NetworkPolicy
    selector:
      matchLabels:
        terasky.com/allow-scrape: "true"
    maxTargets: 50
```

## Selector mode is patch-only

Two lifecycle policies are **rejected at admission**:

- **`onMissing: Create`** — "create N objects matching a selector" is incoherent. There is no
  single object to create.
- **`onRelease: Delete`** — a selector-matched object was by definition not created by this
  contributor, and [only the creator may delete](../concepts/lifecycle.md#releasing-the-last-contributor).

So a Selector-mode contributor uses `onMissing: Wait` with `onRelease: Revert` or `Orphan`.

## Scope

| | `ResourcePatch` | `ClusterResourcePatch` |
|---|---|---|
| Fan-out | confined to **its own namespace** | any namespace |
| `namespaceSelector` | **forbidden** | allowed |
| `maxTargets` | a guard rail; the namespace already bounds it | **the real blast-radius control** |

A namespaced selector contributor is perfectly usable — it just cannot escape its namespace, which
is the same containment property that applies in Single mode.

## Blast radius

!!! warning "`maxTargets` fails rather than truncates"
    Exceeding the cap **fails the contributor** rather than silently applying to a subset. A
    half-applied fan-out is worse than a reported one, because nothing tells you which half.

    ```
    Ready=False  reason=InvalidSpec
    selector matched more than maxTargets=50 objects; narrow the selector or raise the cap
    ```

Applies are also rate-limited across a fan-out, so one contributor cannot saturate the API server.

## Authorization is stricter here

The matched names are not knowable at admission, so the SubjectAccessReview is issued against the
resource and namespace with an **empty name** — asking *"may this principal patch **any** object of
this kind here?"*.

That is deliberately the stricter question. A selector may match objects that do not exist yet, so a
name-scoped check would authorize less than the contributor can actually do.

Practically: a Selector-mode contributor needs namespace-wide (or cluster-wide) patch rights on the
kind, not rights on particular objects.

## How Selector mode is tracked

Selector-mode contributors are **not** in the target-key field index, because their match set is not
knowable from the spec — indexing a stale resolution would go quietly out of date as labels change.
They are resolved by listing at reconcile time, and found again through their recorded
`status.sharedResourceRefs`.

One consequence worth knowing: a contributor gets **one tracker per matched object**, so a
Selector-mode contributor's `status.sharedResourceRefs` lists several.

```bash
kubectl get resourcepatch -n observability scrape-peer \
  -o jsonpath='{.status.observedTargets}' | jq
```

## A worked example

Every NetworkPolicy in a production namespace gets an ingress peer allowing scrapes:

```yaml
apiVersion: terasky.com/v1alpha1
kind: ClusterResourcePatch
metadata:
  name: observability-scrape-peer
spec:
  target:
    mode: Selector
    apiVersion: networking.k8s.io/v1
    kind: NetworkPolicy
    selector:
      matchLabels:
        terasky.com/allow-scrape: "true"
    namespaceSelector:
      matchLabels:
        env: prod
    maxTargets: 50

  lifecycle:
    onMissing: Wait
    onRelease: Revert

  apply:
    mode: ServerSideApply
    conflictPolicy: Fail

  priority: 200

  serviceAccountRef:
    name: observability-patcher
    namespace: observability

  patch:
    type: StrategicMerge
    value:
      spec:
        ingress:
          - from:
              - namespaceSelector:
                  matchLabels:
                    kubernetes.io/metadata.name: observability
            ports:
              - protocol: TCP
                port: 9090
```

When this contributor is deleted, the peer is withdrawn from every matched policy — and only that
peer.

!!! tip "Prefer Single mode where you can"
    Selector mode gives up create-on-missing, reference-counted deletion, and name-scoped
    authorization. When you know the object, name it.
