# Apply modes

Two modes, because the **target's schema** decides which is correct — not preference, and not
legacy.

## ServerSideApply — the default

Each contribution is applied as its own SSA patch under its own field manager
(`patch-operator/<namespace>/<name>`). The API server does the merging and tracks ownership in
`managedFields`; the operator never computes a merged object.

Three properties fall out, and they are why this is the default:

**Conflict detection is the API server's job.** A second contributor claiming a field the first owns
gets a 409 naming the current owner. The operator surfaces that rather than reimplementing it.

**Conflicts can be reported instead of forced.** See [Conflicts](conflicts.md). This is the concrete
improvement over `provider-kubernetes` `Object`, which forces unconditionally.

**Revert is nearly free.** To release a contributor, re-apply an object carrying only
`apiVersion`, `kind` and `metadata.name` under that contributor's field manager. The API server
releases every field that manager owned and deletes the ones no other manager owns.

!!! success "This is verified, not assumed"
    The revert mechanism is the claim the whole design rests on, so it is tested against a real API
    server before anything is built on it — asserting both that the field disappears *and* that the
    manager's `managedFields` entry is removed, while another contributor's fields stay untouched.
    See `internal/apply/ssa_semantics_test.go`.

    No reverse patch to compute, and no stored "previous value" that could have gone stale.

## ClientSideApply — and why it is not a fallback

SSA's field ownership is only as granular as the target's schema. **A list without `listType: map`
and `listMapKey` is atomic**: it is owned wholesale by a single manager, and a second contributor
cannot add an element to it — it can only take the entire list.

This is not an edge case. It hits the motivating example directly:

!!! danger "`Ingress.spec.rules` is atomic"
    It carries no merge key, so **two contributors cannot both add a rule under server-side apply.**
    Vast numbers of third-party CRDs never set these markers at all.

    ClientSideApply is therefore **the mode that makes the primary use case work**, not a
    compatibility option.

Under CSA the operator merges and writes with an optimistic-concurrency `Update`. `spec.patch.mergeKeys`
supplies the granularity the schema is missing:

```yaml
apply:
  mode: ClientSideApply
patch:
  type: StrategicMerge
  mergeKeys:
    - path: spec.rules
      key: host        # this contributor owns "the rule whose host is mine"
  value:
    spec:
      rules:
        - host: a.example.com
          # ...
```

Without a merge key the owned path would be `spec.rules[0]` — "whatever happens to be first" — and a
revert after the list order shifted would delete a **different contributor's** element. With one it
is `spec.rules[host=a.example.com]`, which is surgical.

### The cost: prior values

CSA keeps real bookkeeping. For each contributor the tracker records:

- **`ownedPaths`** — what this contributor set.
- **`priorValues`** — what those paths held *before* this contributor first touched them, for paths
  that already existed.

Revert then **restores the original value rather than deleting the field** — which is the correct
behaviour when a contributor overwrote a pre-existing setting rather than adding a new one, and is
something SSA cannot express.

!!! note "Captured once, never re-captured"
    Re-deriving `priorValues` on a later apply would record the contributor's *own* value and make
    revert a no-op. The first capture wins, permanently.

Two rules about that bookkeeping:

- It lives in the tracker's `status`, **never in annotations on the target**. Annotations bloat, leak
  into diffs users read, and are lost if another controller rewrites metadata.
- It is keyed to `observedTargetUID`. If the target is deleted and recreated out of band, the UID
  changes, the bookkeeping is invalidated wholesale, and everything is re-applied. Stale
  `priorValues` restored onto a different object would be actively harmful.

## Choosing

**Use SSA unless the target's schema makes it wrong.**

| Situation | Mode |
|---|---|
| Map fields (`ConfigMap.data`, `metadata.labels`, `metadata.annotations`) | ServerSideApply |
| Scalar fields (`spec.replicas`, `spec.ingressClassName`) | ServerSideApply |
| A list **with** `listType: map` (`Service.spec.ports`, `PodSpec.containers`) | ServerSideApply |
| A list **without** a merge key (`Ingress.spec.rules`, most CRD lists) | ClientSideApply + `mergeKeys` |
| A target whose controller uses `Update` and wipes `managedFields` | ClientSideApply |

`mergeKeys` are **rejected under SSA**: there the API server takes list semantics from the schema,
so accepting them would imply they take effect.

## Patch types

| Type | Field | Notes |
|---|---|---|
| `StrategicMerge` (default) | `patch.value` | The partial object to merge. Honours `mergeKeys` under CSA. |
| `Merge` | `patch.value` | RFC 7386 JSON merge patch. |
| `JSON6902` | `patch.ops` | RFC 6902 operations, for what a merge cannot express. |

An unchanged contribution issues **no write at all** — the rendered contribution is hashed, and a
matching hash plus an unchanged target `resourceVersion` skips the apply. That is what keeps the
operator's own watch events from becoming a hot loop.
