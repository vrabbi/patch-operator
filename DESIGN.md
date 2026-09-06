# patch-operator — design

**Status:** pre-alpha, design only. No implementation yet.
**API group / version:** `terasky.com/v1alpha1`

Four CRDs in two scope-matched pairs, mirroring `Role`/`ClusterRole`: `ResourcePatch` +
`SharedResource` (namespaced, confined to one namespace) and `ClusterResourcePatch` +
`ClusterSharedResource` (cluster-scoped, cross-namespace). See §3.5.

---

## 1. Problem & non-goals

### The shared-resource problem

Platform teams increasingly compose infrastructure with orchestrators that instantiate a graph of
resources per claim: a Crossplane XR, a kro `Instance`, a Helm release. Each instantiation owns its
own resources cleanly. What none of them handle well is the resource that is **shared across
instantiations**:

- A shared `Ingress` where every application XR needs to add one rule and one TLS host.
- A shared `NetworkPolicy` where every tenant needs to add one ingress peer.
- A shared `ConfigMap` — a routing table, an allow-list, a service registry — where each instance
  contributes one key.
- A shared cloud-side object modelled as a CR (an `ProviderConfig`, a peering, a DNS zone) where
  each instance adds one entry.
- A shared `ClusterRole` aggregating rules contributed per tenant.

The defining property is that **no single instantiation owns the object**. Each one owns a *slice of
its fields*. The object must exist as long as at least one contributor exists, and each contributor's
fields must disappear when that contributor does — without disturbing anyone else's.

### Why the obvious mechanisms do not work

**`ownerReferences` are the natural reach for reference counting, and they fall short.** Kubernetes
garbage collection genuinely reference-counts: a dependent with several owners is deleted only when
*all* owners are gone.

How far short depends on the case, and it is worth being precise rather than dismissing the
mechanism wholesale:

- **Cross-namespace contribution rules it out entirely.** A namespaced dependent's owners must live
  in the same namespace, so a shared object in `platform` cannot be owned by contributors in `team-a`
  and `team-b`. A cluster-scoped dependent cannot be owned by a namespaced object at all.
- **Same-namespace contribution is the case where GC would actually work.** Contributors, target and
  tracker co-located in one namespace is exactly the shape GC handles — and it is precisely the shape
  of this design's namespaced pair (§3.5). The objection above does not apply there.
- **But GC is delete-or-don't, in both cases.** There is no "remove my fields and leave the object"
  semantic, which is what a patch-only contributor actually needs.
- **And GC deletion is unconditional** once the last owner goes, silently overriding whatever policy
  the contributor expressed.

So reference counting has to be explicit, in an object the operator controls — for the
cross-namespace case because GC cannot express it, and for the same-namespace case because GC would
express it *wrongly*. §6.3 returns to this once the lifecycle policies are defined.

**Server-side apply solves the merging half — and only that half.** SSA's `managedFields` is exactly
the right model for multi-writer field ownership: each field manager owns the fields it applies,
conflicts are detected rather than silently overwritten, and a manager that re-applies without a
field it previously owned **releases** that field — deleting it outright if no other manager owns it.
That last property is the mechanism for a clean revert, and this design leans on it hard.

What SSA does *not* provide: lifecycle (create the object if nobody has yet; delete it when the last
contributor leaves), any record of *who* the contributors are, arbitration when two contributors
legitimately want the same field, or any answer for the many CRDs whose list fields are atomic and
therefore cannot be co-owned at all.

### What this operator is

A **reference-counted, revertible, multi-writer field-contribution primitive**, with explicit
conflict arbitration and a lifecycle policy per contributor.

### Non-goals

- **Not a policy engine.** Contributions are declared per instantiation, not by cluster-wide rules
  over a match set. Kyverno already does the latter well.
- **Not a replacement for `provider-kubernetes` `Object` or kro.** It is designed to be *emitted by*
  them. An XR still uses `Object` for the resources it solely owns.
- **Not a templating language.** Contributions are strategic-merge, RFC 7386 merge, or JSON 6902
  patches. Any value interpolation is the orchestrator's job — Crossplane patches and kro CEL are
  already good at it.
- **Not a drift-correction tool for objects with a single owner.** If one thing owns the object,
  use `Object`.

---

## 2. Prior art

| System | What it gives | The gap |
|---|---|---|
| **Crossplane `provider-kubernetes` `Object`** with SSA | Closest existing answer. Several `Object`s may target one resource with partial manifests; `managementPolicies: ["Observe","Update"]` stops non-lead objects creating or deleting it. Field manager per MR. | Its own docs list the limits: it **forces every conflict** ("`provider-kubernetes` forces the conflicts"), it **cannot remove its partial fields on delete**, and it requires field sets to be **disjoint** with no mechanism to coordinate that. No create-on-missing → delete-on-last-release lifecycle; one MR must be designated "lead" by hand. |
| **kro `externalRef`** | Reads an existing shared resource into an RGD and exposes its fields. | Strictly read-only — "kro reads the resource from the cluster but never creates, updates, or deletes it." It solves *referencing* a shared resource, never *contributing* to one. |
| **Kyverno mutate-existing** | Background mutation of existing resources via `patchesStrategicMerge` / `patchesJson6902`. | Policy-shaped: a cluster-scoped rule over a match set, not a per-instantiation contribution. No reference counting, no revert when the thing that motivated the mutation goes away, no per-contributor identity. |
| **Kubernetes GC, multiple `ownerReferences`** | True reference counting, built in and free. | Same-namespace only; cluster-scoped dependents excluded; delete-or-nothing with no revert; unconditional. See §1. |
| **Crossplane `Usage`** | Expresses deletion *ordering* between resources. | About ordering, not field contribution or shared writes. |
| **Argo CD / kapp / Flux SSA** | App-level server-side apply with ownership tracking. | Operates on whole applications from a git source, not per-XR field contributions with their own lifecycles. |

**Read the `provider-kubernetes` server-side-apply document as the strongest argument for this
project.** It documents the shared-resource pattern, and then documents that the pattern leaks in
exactly the three places this operator is built to close: forced conflicts, no revert on delete, and
uncoordinated disjointness.

Sources:
- <https://github.com/crossplane-contrib/provider-kubernetes/blob/main/docs/server-side-apply.md>
- <https://github.com/crossplane-contrib/provider-kubernetes/issues/26>
- <https://kro.run/docs/concepts/rgd/resource-definitions/external-references/>
- <https://kyverno.io/docs/policy-types/cluster-policy/mutate/>
- <https://kubernetes.io/docs/reference/using-api/server-side-apply/>

---

## 3. API

Four CRDs, in two scope-matched pairs. Two design decisions carry most of the weight: the split
between a **contributor** CRD and a **tracker** CRD (§4.1), and the split between a **namespaced**
and a **cluster-scoped** pair (§3.5).

### 3.1 `ResourcePatch` (namespaced) — the contributor

One per contributing instantiation. A Crossplane Composition or a kro RGD emits one of these; the
XR/Instance owns it via `ownerReferences` in the normal way.

**A `ResourcePatch` can only ever touch objects in its own namespace.** That is a structural
property, not a policy check — see §7.0, where it does most of the multi-tenancy work.

```yaml
apiVersion: terasky.com/v1alpha1
kind: ResourcePatch
metadata:
  name: checkout-ingress-rule
  namespace: team-a
spec:
  target:
    mode: Single                        # Single | Selector
    apiVersion: networking.k8s.io/v1
    kind: Ingress                       # must be a namespaced kind
    # --- Single mode ---
    name: team-a-shared-ingress
    namespace: team-a                   # optional; defaults to .metadata.namespace,
                                        # and a mismatch is rejected at admission
    # --- Selector mode ---
    # selector:   { matchLabels: { tier: frontend } }
    # maxTargets: 50
    # (namespaceSelector is forbidden here -- see 3.2)

  lifecycle:
    onMissing: Create                   # Fail | Wait | Create
    onRelease: Revert                   # Revert | Orphan | Delete
    adoptExisting: true
    baseReconcile: CreateOnly           # CreateOnly | Enforce

  apply:
    mode: ServerSideApply               # ServerSideApply | ClientSideApply
    fieldManager: ""                    # default: patch-operator/<ns>/<name>
    conflictPolicy: Fail                # Fail | Priority | Force

  priority: 100

  serviceAccountRef:                    # optional but recommended; see 7.3
    name: team-a-patcher                # always in .metadata.namespace;
                                        # a namespace field here is rejected

  base:                                 # only consulted when onMissing: Create
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: team-a-shared-ingress
      namespace: team-a
    spec:
      ingressClassName: nginx

  patch:
    type: StrategicMerge                # StrategicMerge | Merge | JSON6902
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
    # For type: JSON6902 use `ops:` instead of `value:`
    # ops:
    #   - { op: add, path: /spec/rules/-, value: {...} }
    mergeKeys:                          # ClientSideApply only; see 5.2
      - path: spec.rules
        key: host

status:
  conditions:                           # Ready, Synced, TargetFound, Applied, Conflict, Authorized
    - type: Ready
      status: "True"
  observedTargets:
    - { apiVersion: networking.k8s.io/v1, kind: Ingress, name: team-a-shared-ingress,
        namespace: team-a, uid: 8f3c..., state: Applied }
  sharedResourceRefs:
    - { kind: SharedResource, namespace: team-a,
        name: ingress.networking.k8s.io-team-a-shared-ingress-a1b2c3 }
  appliedGeneration: 4
```

#### Field notes

**`base` and `patch` are deliberately separate.** `base` answers "if I have to create this object,
what should it look like?"; `patch` answers "what do I always contribute?". A patch-only contributor
omits `base` entirely and sets `onMissing: Wait` or `Fail`.

Keeping them separate is what stops the base and the contributions fighting. With the default
`baseReconcile: CreateOnly`, the base is written exactly once — at creation — and never re-asserted,
so it can never claw back a field that a contributor later set. `Enforce` (continuously re-apply the
base under a dedicated field manager) is left as a roadmap item precisely because it reintroduces
that fight and needs its own priority story.

If several contributors supply a `base`, the first one to win the create race creates the object; the
others compare their base against `status.observedBaseHash` and, if it differs, raise a
`BaseMismatch` condition and stop. They do **not** re-apply. A disagreement about the seed is a
configuration error and should be loud, not a flapping object.

**`priority` (default 100, higher wins)** orders contributors deterministically and breaks conflicts
under `conflictPolicy: Priority`.

**`Ready` and `Synced` conditions follow the Crossplane condition shape** so an XR's readiness check
and a kro Instance's status roll-up work without adapters. `Ready=True` requires that the
contribution has been applied to every resolved target *and* that the contributor is not party to an
unresolved conflict. A contributor whose fields were superseded under `conflictPolicy: Priority`
reports `Ready=False, reason=Superseded` — losing quietly would be worse than failing loudly.

**`mode: Selector` is patch-only.** `onMissing: Create` is rejected at admission in Selector mode:
there is no single object to create, and "create N objects matching a selector" is incoherent.
`onRelease: Delete` is likewise rejected — a selector-matched object was by definition not created by
this contributor.

### 3.2 `ClusterResourcePatch` (cluster-scoped) — the cross-namespace contributor

**Identical spec to `ResourcePatch`.** Same `target`, `lifecycle`, `apply`, `priority`,
`serviceAccountRef`, `base` and `patch` — this is one API with two reaches, not two APIs. Everything
in §3.1's field notes applies unchanged.

The differences are entirely in what the target may be:

```yaml
apiVersion: terasky.com/v1alpha1
kind: ClusterResourcePatch
metadata:
  name: team-a-platform-ingress-rule     # cluster-scoped: no namespace
spec:
  target:
    mode: Single
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    name: shared-ingress
    namespace: platform                  # required for namespaced kinds; a different
                                         # namespace than the contributors' -- the point
    # For a cluster-scoped kind (ClusterRole, StorageClass, ...) omit `namespace`.
    # Selector mode may additionally use:
    # namespaceSelector: { matchLabels: { env: prod } }
  lifecycle: { onMissing: Create, onRelease: Revert }
  apply:     { mode: ClientSideApply, conflictPolicy: Fail }
  priority: 100
  serviceAccountRef:
    name: team-a-patcher
    namespace: team-a                    # may be cross-namespace, gated by the
                                         # `impersonate` SAR in 7.3
  # base:, patch: exactly as in 3.1
```

#### Admission validation, by variant

| | `ResourcePatch` | `ClusterResourcePatch` |
|---|---|---|
| **target kind** | must be a **namespaced** kind (checked via RESTMapper) | namespaced *or* cluster-scoped |
| **`target.namespace`** | optional; defaults to `.metadata.namespace`; **any other value is rejected** | required for namespaced kinds, forbidden for cluster-scoped ones |
| **`namespaceSelector`** | forbidden | allowed |
| **`serviceAccountRef.namespace`** | forbidden — always the CR's own namespace | allowed; gated by the `impersonate` SAR |
| **`maxTargets`** | a guard rail; the namespace already bounds the fan-out | the real blast-radius control |
| **Tracked by** | `SharedResource` in that namespace | `ClusterSharedResource` |
| **Typical RBAC grant** | tenant namespaces, freely | platform team only (§7.0) |

The namespaced constraints are enforced at admission *and* re-checked at reconcile, so a
`ResourcePatch` cannot escape its namespace even if its spec were mutated by a path that bypassed the
webhook.

### 3.3 `SharedResource` (namespaced) — the tracker

**Operator-owned. Users do not create these** (whether they *may* pre-declare one is a roadmap
question, §10). One per distinct target object, living **in the target's namespace** — which, for
this variant, is also every contributor's namespace. Named deterministically from the target's GVK
and name, sanitised, with a hash suffix to stay inside DNS-1123 and 253 characters.

```yaml
apiVersion: terasky.com/v1alpha1
kind: SharedResource
metadata:
  name: ingress.networking.k8s.io-team-a-shared-ingress-a1b2c3
  namespace: team-a
  finalizers: [terasky.com/shared-resource]
spec:
  targetRef:
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    name: team-a-shared-ingress
    # namespace is implicit: .metadata.namespace
status:
  phase: Applied                        # Waiting | Applied | Conflicted | Releasing | Promoting
  promotedTo:                           # set only while/after promoting (see 6.3)
    name: ""                            # the ClusterSharedResource that took over
  createdByOperator: true
  creatorPatchRef: { kind: ResourcePatch, namespace: team-a, name: checkout-ingress-rule, uid: 1111-... }
  observedBaseHash: sha256:...
  observedTargetUID: 8f3c-...
  observedResourceVersion: "148213"
  contributors:
    - patchRef: { kind: ResourcePatch, namespace: team-a, name: checkout-ingress-rule, uid: 1111-... }
      observedGeneration: 4
      priority: 100
      fieldManager: patch-operator/team-a/checkout-ingress-rule
      state: Applied                    # Applied | Superseded | Conflicted | Releasing
      lastAppliedHash: sha256:...
      ownedPaths:                       # ClientSideApply bookkeeping only
        - spec.rules[host=checkout.team-a.example.com]
      priorValues: {}                   # ClientSideApply: original values, for restore-on-revert
  conflicts:
    - fieldPath: spec.rules
      claimants: [team-a/checkout-ingress-rule, team-a/search-ingress-rule]
      holder: team-a/checkout-ingress-rule
```

`status.contributors` is the reference count. It is derived state — rebuilt on every reconcile from a
live list of contributors — so a lost update or a missed watch event self-heals rather than
corrupting the count.

`patchRef` and `creatorPatchRef` carry a `kind`, because after a promotion (§6.3) a single tracker
holds a mix of `ResourcePatch` and `ClusterResourcePatch` contributors.

### 3.4 `ClusterSharedResource` (cluster-scoped) — the tracker for everything else

Identical status shape to `SharedResource`, with `spec.targetRef.namespace` explicit (or absent for a
cluster-scoped target). It tracks:

- every cluster-scoped target, and
- every namespaced target that has at least one `ClusterResourcePatch` contributor.

It has no `promotedTo` field: promotion is one-way, and a `ClusterSharedResource` is never demoted
(§6.3).

### 3.5 Scope rules and tracker selection

| CRD | Scope | May target | Contributors live | Tracked by |
|---|---|---|---|---|
| `ResourcePatch` | namespaced | namespaced kinds, **its own namespace only** | all in that one namespace | `SharedResource` in that namespace |
| `ClusterResourcePatch` | cluster | any namespace, and cluster-scoped kinds | anywhere | `ClusterSharedResource` |

**Exactly one tracker owns a target.** The single-writer invariant (§4.1) is what the whole
architecture rests on, and the scope split must not weaken it. The tracker for a given target is
therefore determined by a rule, not by whichever controller got there first:

1. **Cluster-scoped target** → always a `ClusterSharedResource`.
2. **Namespaced target, all contributors are `ResourcePatch`** → a `SharedResource` in the target's
   namespace. By construction that is also every contributor's namespace.
3. **Namespaced target with at least one `ClusterResourcePatch`** → a `ClusterSharedResource`,
   reached by **promotion** (§6.3) if a `SharedResource` was already tracking it.

Rule 3 exists because a namespaced tracker cannot honestly represent a contributor that is not in its
namespace: its reference count would be incomplete, and an incomplete reference count deletes objects
that are still in use.

## 4. Controller architecture

### 4.1 Reconcile the target, not the request

The single most important structural decision: **contributors are inputs, and the
tracker is the unit of reconciliation.** The tracker controller is the *only* code path that writes
to a target object, and exactly one tracker owns any given target (§3.5).

The naive alternative — one controller reconciling contributors, each writing its own slice — is
what `provider-kubernetes` does today, and it is why that approach needs blind conflict forcing. With
N contributors writing independently you get N-way races on one object, non-deterministic apply
order, no place to notice that two contributors want the same field, and no coherent moment at which
to decide the object is now unreferenced.

Aggregating first fixes all four at once:

- writes to one target are serialised through one work queue key;
- apply order is a deterministic sort, stable across operator restarts;
- conflicts are detected before any write, because all contributions are in hand;
- the reference count is a list length, evaluated at one point in the code.

### 4.2 The controllers

Four CRDs, but **not four hand-written controllers.** The contributor types share one spec struct and
differ only in reach, and the tracker types share one status struct and differ only in scope. The
implementation is **one generic contributor reconciler and one generic tracker reconciler**,
parameterised over scope and instantiated twice each. The scope split should cost a validation table
and a tracker-selection rule, not a second copy of the logic.

**Contributor controller** (`ResourcePatch`, `ClusterResourcePatch`) — resolves and registers. It
never writes to a target.

1. Validate the spec (mode/lifecycle compatibility, patch well-formedness).
2. Resolve `spec.target` to a concrete list of target keys. `Single` yields exactly one — whether or
   not the object exists. `Selector` yields zero or more, capped at `maxTargets`. For a
   `ResourcePatch` every resolved key is forced into the CR's own namespace, re-checked here and not
   only at admission.
3. Select the tracker kind for each key by the rule in §3.5, and ensure it exists (create if absent;
   `AlreadyExists` is a normal, expected outcome and not an error). A `ClusterResourcePatch` that
   finds a namespaced `SharedResource` already tracking its target starts a **promotion** (§6.3).
4. Add the `terasky.com/contributor` finalizer.
5. Enqueue each tracker.
6. Mirror the per-contributor state from the tracker's `status.contributors` back onto the
   contributor's `status` as conditions. A contributor whose tracker was promoted follows
   `status.promotedTo` and re-registers on the `ClusterSharedResource`.

On deletion it does *not* revert anything itself; it enqueues its tracker and waits for it to confirm
the release before dropping the finalizer (§6).

**Tracker controller** (`SharedResource`, `ClusterSharedResource`) — the writer.

1. List bound contributors via a **field index on the computed target key**, so this is a cheap
   indexed lookup rather than a cluster-wide scan. The index spans **both** contributor kinds, since
   after a promotion one tracker holds a mix of them. Selector-mode contributors are indexed against
   every key they currently resolve to. A namespaced `SharedResource` indexes only within its own
   namespace, which keeps its lookups cheap and its cache small.
2. Sort by `(priority desc, creationTimestamp asc, uid asc)`. The `uid` tiebreak matters: two
   contributors created in the same clock tick must still sort identically on every replica and after
   every restart, or the object flaps.
3. Read the live target. Absent → honour the highest-priority contributor's `onMissing`.
4. Detect conflicts across the sorted contributions (§5.3) and resolve per `conflictPolicy`.
5. Apply (§5).
6. Reconcile the reference count and lifecycle (§6).
7. Write status on both the tracker and, indirectly, each contributor.

A tracker in `phase: Promoting` is **fenced**: it does not write to the target at all (§6.3).

### 4.3 Watching targets

Target GVKs are not known at compile time, so the operator maintains **dynamic informers created
lazily** on first use for a GVK and torn down when the last tracker for that GVK is deleted. Target
events map back to a tracker by key. A namespace-only install (§7.0) watches only namespaced kinds,
and can restrict every informer to the namespaces it actually has contributors in.

Watching arbitrary GVKs cluster-wide is the main scalability hazard — an informer over every `Secret`
or `ConfigMap` in a large cluster is expensive. Mitigations, in order of preference:

- **Label-restricted informers.** The operator stamps `terasky.com/shared: "true"` on targets it
  manages and runs the informer with that label selector, so the cache holds only managed objects.
  This must be opt-out (`spec.target.markTarget: false`) because writing a label is itself a
  mutation, and some targets are managed by controllers that will fight over `metadata.labels`.
- **A cap on distinct watched GVKs**, with a clear condition when it is hit rather than silent
  degradation.
- **A resync-only fallback** (`--no-watch-gvk=v1/Secret`) that polls on the resync interval for GVKs
  where a watch is not affordable. Drift correction becomes slower; correctness does not change.

### 4.4 Avoiding hot loops

The operator's own writes come straight back as watch events. Two guards:

- Each contributor's rendered contribution is hashed into `lastAppliedHash`. If the hash is unchanged
  **and** `status.observedResourceVersion` matches the live object, the apply is skipped entirely.
- After a successful write, the resulting `resourceVersion` is recorded, so the echo of that write is
  recognised and dropped.

Leader election is required. Work is keyed per target so a single object is never reconciled
concurrently, and failed applies back off exponentially — conflicts especially, which must never
become a tight retry loop against the API server.

---

## 5. Apply modes

### 5.1 ServerSideApply — the default

Each contributor's contribution is applied as its **own SSA patch under its own field manager**,
`patch-operator/<namespace>/<name>` (hashed if it would exceed the field-manager length limit). The
API server performs the merge and tracks ownership in `managedFields`. The operator does not compute
a merged object at all.

Three properties fall out for free, and they are why SSA is the default:

**Conflict detection is the API server's job.** A second contributor claiming a field the first owns
gets a 409 naming the current owner. The operator surfaces that as a `Conflict` condition rather than
resolving it.

**`conflictPolicy` is honest about force.** Default `Fail`: report the conflict, write nothing, leave
both contributors `Ready=False`. `Priority`: force **only** when the conflicting manager is another
`patch-operator` field manager — internal arbitration by the priority the user declared — and never
when the holder is a foreign controller, because stealing a field from a controller that will
immediately write it back is a flap, not a resolution. `Force`: unconditional, opt-in, documented as
a last resort. This is the concrete improvement over `Object`'s unconditional forcing.

**Revert is nearly free.** To release a contributor, re-apply an object containing only
`apiVersion`, `kind` and `metadata.name` under that contributor's field manager. The API server
releases every field that manager owned, and deletes outright the ones no other manager owns. No
bookkeeping, no reverse-patch computation, no risk of the operator's idea of "the previous value"
having gone stale. The field manager entry is then removed.

### 5.2 ClientSideApply — the escape hatch, and why it must exist

SSA's field ownership is only as granular as the target's schema. A list without `listType: map` and
`listMapKey` markers is **atomic**: it is owned wholesale by a single manager, and a second
contributor cannot add an element to it — it can only take the entire list.

This is not an edge case. It hits the motivating example directly: `Ingress.spec.rules` carries no
merge key, so two contributors cannot both add a rule under SSA. Vast numbers of third-party CRDs
never set these markers at all. **CSA is therefore not a legacy compatibility mode — it is the mode
that makes the primary use case work**, and the doc should be read that way.

The CSA path:

1. Read the live target with its `resourceVersion`.
2. If creating, apply the `base`.
3. Merge each contribution in priority order, using `spec.patch.mergeKeys` to identify list elements
   by a user-declared key where the schema does not declare one. `mergeKeys` is what supplies the
   granularity the schema is missing.
4. Three-way merge against the contributor's `lastAppliedHash` state so that fields the contributor
   previously set but has now dropped are removed.
5. `Update` with a `resourceVersion` precondition; on 409, re-read and retry with backoff.

The cost is real bookkeeping. For each contributor the tracker records **`ownedPaths`** (what
this contributor set) **and `priorValues`** (what those paths held *before* this contributor first
touched them, for paths that already existed). Revert then restores the original value rather than
deleting the field — which is the correct behaviour when a contributor overwrote a pre-existing
setting rather than adding a new one, and is something SSA cannot express.

Two rules about that bookkeeping:

- It lives in the tracker's `status`, **never in annotations on the target**. Annotations bloat,
  leak into diffs users read, and are lost if another controller rewrites metadata.
- It is keyed to `observedTargetUID`. If the target is deleted and recreated out of band, the UID
  changes, the bookkeeping is invalidated wholesale, and everything is re-applied from scratch. Stale
  `priorValues` restored onto a different object would be actively harmful.

**Guidance to state plainly in the docs: use SSA unless the target's schema makes it wrong.** The
admission webhook inspects the target's OpenAPI schema, and if a contribution writes into an atomic
list while `mode: ServerSideApply` is set, it warns (and sets a `ListMergeUnsupported` condition)
pointing at `ClientSideApply` plus `mergeKeys`.

### 5.3 Conflict semantics

A conflict is two contributors setting **the same field path to different values**. Same path, same
value is not a conflict — it is redundancy, and it is allowed, because two XRs asking for the same
ingress class is normal.

Detection is per mode: SSA gets it from the 409; CSA compares the rendered path sets before writing.
Either way, resolution happens *before* any write, and the outcome is recorded in
the tracker's `status.conflicts` with the field path, all claimants, and the current holder — so a
user can see who is fighting without reading `managedFields`. Claimants are qualified by kind, since
a namespaced and a cluster-scoped contributor can be party to the same conflict after a promotion.

Whatever the policy, the object must never flap. A conflict that cannot be resolved parks with a
condition and a backoff; it does not produce alternating writes.

---

## 6. Lifecycle & reference counting

Finalizers on all four CRDs: `terasky.com/contributor` on `ResourcePatch` and
`ClusterResourcePatch`, `terasky.com/shared-resource` on `SharedResource` and
`ClusterSharedResource`.

Everything in this section is scope-independent. Which tracker owns a given target is decided by
§3.5; once decided, release, reference counting and deletion behave identically in both variants. The
one scope-specific mechanic is promotion (§6.3).

### 6.1 Releasing one contributor

A contributor — `ResourcePatch` or `ClusterResourcePatch` — is deleted, usually because its XR or
Instance was deleted, propagating through `ownerReferences`. It enters `Terminating` with its
finalizer held. Its tracker:

1. Marks the contributor `Releasing`.
2. Applies its `onRelease` policy to that contributor's fields only:
   - **`Revert`** (default) — SSA: re-apply empty under its field manager. CSA: delete `ownedPaths`,
     restoring `priorValues` where present.
   - **`Orphan`** — leave the fields in place, untouched.
   - **`Delete`** — see §6.2; this is about the whole object, not the fields.
3. Removes the contributor from `status.contributors`.
4. Releases the contributor's finalizer.

Order matters: the finalizer is released **only after** the revert is confirmed. A contributor that
vanishes before its fields are withdrawn leaves fields nobody owns and nobody can find.

### 6.2 Releasing the last contributor

When `status.contributors` empties:

- **`Orphan`** → leave the target; delete the tracker.
- **`Revert`** → withdraw the remaining fields; leave the target; delete the tracker.
- **`Delete`** → delete the target, **but only if both**:
  - `status.createdByOperator == true` — the operator created this object, so it is the operator's to
    remove; and
  - the request comes from `status.creatorPatchRef` — the contributor that actually created it,
  matched by kind, name, namespace **and UID**.

That second condition is a hard safety property, not a nicety. Without it, any patch-only contributor
could set `onRelease: Delete`, attach itself to a pre-existing production `Deployment`, and delete it
on the way out. A contributor that did not create an object may never delete it, whatever it asks
for. Admission rejects `onRelease: Delete` combined with `onMissing: Fail|Wait` for the same reason,
and the runtime check stands independently in case the spec was mutated afterwards.

### 6.3 Promotion: when a namespaced target gains a cluster-scoped contributor

A namespaced target tracked by a `SharedResource` acquires its first `ClusterResourcePatch`
contributor. Rule 3 of §3.5 says a `ClusterSharedResource` must now own it, because a namespaced
tracker cannot count a contributor outside its namespace — and an undercount deletes objects that are
still in use.

The migration is **one-way and must never leave two trackers able to write.** It is keyed on the
target and is idempotent, so a crash mid-flight resumes rather than corrupts:

1. The contributor controller resolves the target and finds the existing `SharedResource`.
2. **Fence first.** Set `status.phase: Promoting` and `status.promotedTo` on the namespaced tracker.
   *This commit is what stops it writing, and it must land before step 3.* Ordering these the other
   way round is the one way to get two writers on one object.
3. Create the `ClusterSharedResource`, copying `status.contributors` — **including `ownedPaths` and
   `priorValues`** — along with `observedTargetUID`, `observedResourceVersion`, `createdByOperator`,
   `creatorPatchRef` and `observedBaseHash`.
4. The `ClusterSharedResource` confirms adoption. **Only then** is the namespaced tracker's finalizer
   released and the object deleted.
5. Existing `ResourcePatch` contributors follow `promotedTo`, re-register on the cluster tracker, and
   update `status.sharedResourceRefs`. Their contributions are never re-applied or reverted; nothing
   about the target changes during a promotion.

Two properties of that copy are load-bearing rather than incidental:

- **Losing `priorValues` would silently break revert** for every existing contributor under
  ClientSideApply — fields would be deleted on release instead of restored to what they held before.
- **Losing `createdByOperator` / `creatorPatchRef` would silently break the delete-safety rule**
  (§6.2): the tracker would no longer know that it created the object, or which contributor created
  it, and the "only the creator may delete" guarantee would evaporate at exactly the moment the
  contributor set became cross-namespace.

Crash recovery: a fenced tracker with no `ClusterSharedResource` unfences and retries; a fenced
tracker whose `ClusterSharedResource` already exists resumes at step 4.

**Never demote.** When the last `ClusterResourcePatch` leaves, the `ClusterSharedResource` stays and
keeps tracking the namespaced target. Demotion would be a second migration with all the same hazards
for no user-visible benefit (§10).

### 6.4 Why not `ownerReferences` on the target

Worth answering directly, because it is the first thing anyone reaches for — and the honest answer
now differs between the two variants.

**For cluster-scoped targets and cross-namespace contributors, GC simply cannot express it.** Owners
must be in the target's namespace; a cluster-scoped target cannot have a namespaced owner.

**For the namespaced pair, GC genuinely would work** — contributors, tracker and target are all
co-located, which is exactly the shape multi-owner GC handles. That objection has no force here, and
§1 says so. The reason to reject it anyway is different, and stronger:

- **GC deletes unconditionally** when the last owner disappears. That silently overrides
  `onRelease: Orphan` and `onRelease: Revert` — the two most common policies. Running both mechanisms
  means two independent deletion authorities over one object, and the one that ignores the user's
  stated policy always wins the race.
- **Uniformity.** A lifecycle that behaved one way for namespaced targets and another for
  cluster-scoped ones would be a persistent source of surprise, and would make promotion (§6.3) a
  change in deletion semantics rather than a bookkeeping migration.

**So this design does not put `ownerReferences` on targets, in either variant.** The tracker plus
finalizers is the sole lifecycle authority.

The ownership chain that *does* work, and that this design depends on:

```
XR / kro Instance --ownerRef--> ResourcePatch        --tracked by--> SharedResource        --manages--> target
XR / kro Instance --ownerRef--> ClusterResourcePatch --tracked by--> ClusterSharedResource --manages--> target
```

Deletion propagates *in* from the orchestrator by ordinary GC, and is *arbitrated* by the operator.
Each layer does what it is good at.

### 6.5 Recovering from lost state

- **Missed delete events.** `status.contributors` is rebuilt from a live list on every reconcile, so a
  contributor whose CR no longer exists is detected and released on the next pass. A
  periodic full resync sweeps trackers that received no events at all.
- **Target replaced out of band.** A changed `observedTargetUID` invalidates all CSA bookkeeping,
  raises `TargetReplaced`, and triggers a full re-apply.
- **Target deleted out of band.** If any contributor sets `onMissing: Create`, recreate from that
  contributor's `base` and re-apply all contributions; otherwise park in `Waiting`.
- **Empty tracker.** A tracker with no contributors and no lifecycle work pending is deleted.
- **Interrupted promotion.** Resumed from the fence, per §6.3.

---

## 7. Authorization

**Start here when reviewing this design.** The hard problem is not merging — SSA does most of that.
The hard problem is that this operator is an *"apply arbitrary fields to arbitrary objects"*
primitive. Whoever can create a contributor inherits whatever the write path can do.

Three layers address that, in decreasing order of how much they can be relied upon: **containment by
scope**, then SubjectAccessReview at admission, then impersonation at write time.

### 7.0 Containment by scope — the primary control

The scope split (§3.5) is the strongest security property in this design, because it is
**structural rather than an authorization check**. A `ResourcePatch` in namespace `team-a` cannot
touch an object outside `team-a`. Not "is not permitted to" — *cannot*: the target namespace is
forced to the CR's own namespace at admission and re-checked at reconcile, and there is no field that
expresses anything else.

That matters because it holds **regardless of how privileged the creating principal is**. §7.4
explains why that is not a hypothetical: patches created by Crossplane or kro are admitted as the
orchestrator's near-cluster-admin ServiceAccount, so every check that reasons about the requester is
weak for exactly the population of patches that matters most. Containment does not reason about the
requester at all.

The practical consequence for cluster operators:

- **Grant `ResourcePatch` freely** in tenant namespaces — to tenants, and to Crossplane and kro. The
  blast radius of that grant is one namespace, whatever the Composition author writes.
- **Treat `ClusterResourcePatch` as a privileged grant**, held by the platform team. It is the only
  one of the two that can cross a namespace boundary or touch a cluster-scoped object, so it is the
  only one that needs the scrutiny §7.1–§7.4 describe.
- **A namespace-only install is possible.** Don't install the `ClusterResourcePatch` and
  `ClusterSharedResource` CRDs at all. The operator then needs no cluster-wide write RBAC, and
  cross-namespace contribution is not merely denied but absent.

Everything below is about making the `ClusterResourcePatch` path safe. It applies to `ResourcePatch`
too, but there it is defence in depth on top of a boundary that already holds.

### 7.1 SubjectAccessReview on every mutation

A validating webhook on both contributor CRDs for **CREATE, UPDATE and DELETE** — not create alone —
with `failurePolicy: Fail`. Each verb runs `SubjectAccessReview`s against the **target**, using
`request.userInfo` as the subject. The question asked is always: *could this principal have made this
change to the target directly?*

For a `ResourcePatch` the SAR is necessarily scoped to the CR's own namespace, because that is the
only namespace it can reach. For a `ClusterResourcePatch` the SAR is the first real boundary, and is
scoped to whatever namespace — or cluster scope — the target names.

| Verb on the contributor | SARs against the target |
|---|---|
| **CREATE** | `patch` and `update`; plus `create` if `onMissing: Create`; plus `delete` if `onRelease: Delete` |
| **UPDATE** | the same set, evaluated against **both the old and the new target** when `spec.target` changes, and re-evaluated when `lifecycle` widens (e.g. `Revert` → `Delete`) |
| **DELETE** | `patch`/`update` when `onRelease: Revert`; `delete` when `onRelease: Delete`; none when `onRelease: Orphan`, which touches nothing |

Covering UPDATE and DELETE is what makes this sound. Create-only checking is bypassed trivially:
create a harmless patch, then update it to point at a different target, or flip `onRelease` to
`Delete` and delete the CR. Both are privilege escalations that a create-time check never sees.

Implementation notes:

- **A DELETE `AdmissionReview` carries `oldObject` and no `object`.** The lifecycle policy that
  decides which SAR to run must be read from `oldObject`. Getting this wrong fails open.
- **In `Selector` mode the matched names are not known at admission**, so the SAR is issued against
  the GVR and namespace with an **empty resource name** — which asks "may this principal patch *any*
  object of this kind in this namespace?". That is the stricter question, and it is the correct one:
  a selector may match objects that do not exist yet.
- Cluster-scoped targets are checked with an empty namespace.
- Also denied at admission: a contributor targeting any of this operator's own four kinds. This is
  **reconcile-loop safety**, not a sensitive-kind policy.
- A `ResourcePatch` whose `target.namespace` names anything other than its own namespace is rejected
  outright (§3.2) — before any SAR runs, since no SAR could make it legal.

**There is no protected-GVK allowlist or denylist in this design.** Authorization is SAR plus
impersonation. The operator holds no opinion about which kinds are sensitive; RBAC already encodes
that, and duplicating it in operator configuration produces a second, divergent policy surface that
cluster admins must remember to maintain.

### 7.2 Re-checking after admission

Admission is point-in-time. A contributor created while its author held broad rights keeps working
forever after those rights are revoked, because nothing ever touches the CR again.

So: the webhook records the admitting principal in an immutable annotation, and the controller
re-runs the SAR before writes on a TTL (~10 minutes), and immediately whenever the effective target
or lifecycle differs from what was admitted. The result is the `Authorized` condition; when it goes
false the operator stops writing and says why — it does **not** revert, because losing authorization
is not the same as being released, and silently tearing down a tenant's ingress rule because an RBAC
binding was reorganised would be worse than the exposure.

### 7.3 `spec.serviceAccountRef` — impersonation

The second layer, in v1alpha1 rather than deferred, and the answer to §7.4.

When set, the operator performs **every** target write while impersonating that ServiceAccount. The
API server then enforces that SA's RBAC on the write itself — continuously, at write time, not once
at admission, and using the tenant's rights rather than the operator's. The operator holds
`impersonate` on `serviceaccounts`; the write carries only what the named SA can do.

Guard rails, so that impersonation is not itself the escalation:

- **The webhook runs an extra SAR for verb `impersonate`** on resource `serviceaccounts`, name = the
  referenced SA, namespace = its namespace, subject = `request.userInfo`. You may only point a
  `ResourcePatch` at a ServiceAccount you could already impersonate. Without this check
  `serviceAccountRef` would be a way to *borrow* privilege rather than drop it.
- **`ResourcePatch` may only name a ServiceAccount in its own namespace** — the field has no
  `namespace` sub-field there at all (§3.2), so the containment property of §7.0 extends to the
  identity the write runs as. `ClusterResourcePatch` may name any ServiceAccount, gated by that same
  `impersonate` SAR, and cross-namespace references can be disabled cluster-wide by a flag.
- **When `serviceAccountRef` is unset**, writes use the operator's own identity, bounded only by the
  admission SAR. That is the weaker configuration. Docs should recommend `serviceAccountRef` for any
  cluster with more than one tenant.

### 7.4 The caveat to state loudly

When Crossplane or kro creates a contributor, `request.userInfo` is the **orchestrator's**
ServiceAccount — Crossplane's or kro's controller SA — not the human who created the XR. Those SAs
are typically close to cluster-admin, because they have to be able to create anything a Composition
might reference.

So SAR meaningfully bounds a user creating contributors directly, and does comparatively little
against a Composition author, whose patches are admitted as a near-cluster-admin. **This caveat now
applies almost entirely to `ClusterResourcePatch`**, and that is the point of the scope split: for a
`ResourcePatch`, a near-cluster-admin admitting principal buys nothing, because the namespace
boundary is structural and does not consult the principal at all (§7.0).

For `ClusterResourcePatch` the caveat stands in full, and there are two answers:

1. **Do not grant it to the orchestrators.** If Crossplane cannot create `ClusterResourcePatch`es, no
   Composition can author a cross-namespace patch, and the question does not arise. This is the
   recommended default, and it is why §7.0 says to grant the namespaced kind freely and the cluster
   kind narrowly.
2. **Where a Composition genuinely must contribute to a shared platform object**, set
   `serviceAccountRef` so the write path drops to a tenant identity the API server enforces on every
   write. A platform team can require it. This is exactly why `serviceAccountRef` belongs in
   v1alpha1 and not on the roadmap.

Documentation must say this plainly rather than implying that admission checks alone make the
operator safe in a multi-tenant cluster. For `ClusterResourcePatch`, they do not.

---

## 8. Failure modes

| Failure | Behaviour |
|---|---|
| **Atomic list under SSA** (§5.2) | Two contributors cannot co-own a list without merge keys. Detected from the OpenAPI schema at admission; warns and sets `ListMergeUnsupported`, pointing at `ClientSideApply` + `mergeKeys`. |
| **Foreign controller uses `Update`, not `Apply`** | `Update` collapses `managedFields` ownership. Field values survive; ownership records degrade. The operator detects that its manager entry vanished while its fields persist, and re-applies to reassert ownership. Frequent recurrence is reported — that target has a controller this operator cannot cooperate with. |
| **Target deleted out of band** | Recreate from `base` if any contributor sets `onMissing: Create`; otherwise `Waiting`. |
| **Target replaced (UID changed)** | Invalidate CSA bookkeeping, `TargetReplaced`, full re-apply (§6.4). |
| **Webhook unavailable** | `failurePolicy: Fail` means contributor writes are rejected, which blocks XR reconciliation. This is the correct trade for an authorization webhook, but it makes webhook availability a hard dependency: run ≥2 replicas, and document it as a cluster-wide blast radius. |
| **Selector fan-out blast radius** | `maxTargets` caps a single contributor; exceeding it fails the contributor rather than silently truncating. Applies are rate-limited across a fan-out so one contributor cannot saturate the API server. A namespaced contributor is additionally bounded by its namespace; for `ClusterResourcePatch` with a `namespaceSelector`, `maxTargets` is the only bound. |
| **Cluster and namespaced contributors racing to create a tracker** | Both may attempt creation for the same target. Tracker names are deterministic (§3.3), so one wins and the other's `AlreadyExists` is a normal outcome; the scope rule (§3.5) then decides whether the namespaced one must promote. The rule is a function of the contributor set, never of arrival order, so the outcome is the same whoever wins the race. |
| **This operator and `Object` on the same resource** | Both use SSA field managers, so they coexist if their fields are disjoint. `conflictPolicy: Priority` deliberately will not force against a foreign manager, so `Object`'s unconditional forcing wins any genuine overlap and the contributor reports `Conflict` rather than flapping. Document: pick one owner per field. |
| **Operator down** | Nothing reverts, nothing is deleted (finalizers hold contributor deletions pending). Targets keep their last applied state. Deleting an XR blocks until the operator returns — an availability cost that must be documented. |
| **Promotion interrupted mid-flight** | The fence (§6.3) is committed before the `ClusterSharedResource` is created, so an interrupted promotion leaves the namespaced tracker fenced and *not writing* — never two writers. Recovery resumes from the fence: unfence and retry if no cluster tracker exists, otherwise complete the hand-over. A tracker stuck `Promoting` past a threshold is reported. |
| **Contributors disagree on `onRelease` for one target** | Legal and common — a creator says `Delete`, a patch-only contributor says `Revert`. Each policy applies only to its own contributor's fields (§6.1); the object-level action at last release is decided by the creator's policy alone, and only if it *is* the creator (§6.2). After a promotion the mix spans both contributor kinds, which changes nothing: the rule keys on `creatorPatchRef`, not on scope. |
| **Target namespace deleted under a namespaced contributor** | Namespace deletion removes the target, the `SharedResource` and the `ResourcePatch`es together, since all three live there. Finalizers must not wedge the namespace in `Terminating`: a tracker whose namespace is terminating skips revert (there is nothing left to revert onto) and releases finalizers promptly. |
| **Contributor stuck terminating** | If a revert cannot complete (target unreachable, authorization lost), the contributor's finalizer is held and the condition says why. Provide a documented, explicit break-glass for dropping the finalizer, since a wedged finalizer otherwise blocks XR deletion indefinitely. |

---

## 9. Observability

**Conditions** on both CRDs, in the Crossplane shape (`type`, `status`, `reason`, `message`,
`lastTransitionTime`) so XR readiness checks and kro status roll-ups consume them directly:
`Ready`, `Synced`, `TargetFound`, `Applied`, `Conflict`, `Authorized`.

**Events** on the contributor (what happened to my contribution), on the tracker (what happened to
this target), and on the target itself (who contributed and when — so someone reading
`kubectl describe` on a shared Ingress can see this operator is involved without knowing it exists).

**Metrics**, all labelled by tracker scope so a namespace-only install can drop the cluster series:
contributors per target (a histogram — the tail identifies objects that have become
coordination bottlenecks); conflicts by field path; forced conflicts (should be near zero; a rising
count means users are papering over a real disagreement); apply latency by mode; revert failures;
watched-GVK count against the cap; SAR denials and impersonation failures.

**`kubectl` printer columns** on both trackers: target kind/namespace/name, contributor count, phase,
conflict count, age. Answering "who is writing to this object?" should be one command — and because a
namespaced target may be tracked by either kind after a promotion, the docs should give the
two-command form (`kubectl get sharedresources -n <ns>` and `kubectl get clustersharedresources`) or
a printer column on the contributor pointing at its tracker.

---

## 10. Open questions & roadmap

- **Contribution rendering.** Whether to support CEL or Go templates inside `patch.value`. Currently
  out of scope on the grounds that Crossplane patches and kro CEL already do this upstream, but a
  contributor that needs a value derived from the *live target* has no way to express it today.
- **`baseReconcile: Enforce`.** Continuously re-asserting the base needs its own priority semantics
  against contributions, and reintroduces the base-versus-patch fight `CreateOnly` avoids (§3.1).
- **User-creatable trackers.** Letting a platform team pre-declare a shared target — its
  base, its apply mode, its policy — before any contributor exists would move policy from N
  contributors to one place. Attractive, but it makes the tracker a user-facing API with its own
  authorization story.
- **Requiring `serviceAccountRef` cluster-wide.** A flag making it mandatory — or mandatory only for
  `ClusterResourcePatch` — would give platform teams a single switch for multi-tenant safety (§7.4).
- **Demotion.** When the last `ClusterResourcePatch` leaves a promoted namespaced target, the tracker
  stays cluster-scoped (§6.3). Demoting would restore the tidier namespaced tracker, at the cost of a
  second migration with the same fencing hazards and no user-visible benefit. Deliberately deferred.
- **A namespace allowlist on `ClusterResourcePatch`.** Constraining a cluster-scoped contributor to a
  named set of namespaces would give an intermediate privilege level between the two CRDs. Attractive,
  but it re-introduces a policy surface that §7 otherwise avoids, and RBAC on the namespaced kind
  already covers most of the need.
- **Contributor-visible target state.** Exposing selected live target fields on `ResourcePatch.status`
  so an XR can consume them, overlapping with kro's `externalRef`.
- **API group naming.** `terasky.com/v1alpha1` for now; revisit before any v1beta1.

---

## Appendix A — worked scenarios

Four walkthroughs, each chosen because it is where a design like this usually breaks.

### A.1 Two XRs contribute to one Ingress; one creates, one only patches

The shared Ingress lives in `platform`; the contributing XRs are in `team-a` and `team-b`. **That is
cross-namespace, so both contributors are `ClusterResourcePatch`es** (§3.5) — a `ResourcePatch` in
`team-a` could not name a target in `platform` at all. In a well-run cluster these are authored by
the platform team, not by tenant Compositions (§7.0).

`team-a`'s contributor (priority 100) sets `onMissing: Create` with a `base` and contributes a rule
for `team-a.example.com`. `team-b`'s (priority 100) sets `onMissing: Wait`, no `base`, and contributes
a rule for `team-b.example.com`. Both use `mode: ClientSideApply` with
`mergeKeys: [{path: spec.rules, key: host}]` — see A.3 for why.

1. Both controllers resolve to the same target key and race to create the `ClusterSharedResource`.
   One wins; the other's `AlreadyExists` is normal. Both register as contributors.
2. The tracker controller sorts them (equal priority → `creationTimestamp`, then `uid`).
   The target is absent, so it consults the highest-priority contributor: `team-a`, which says
   `Create`. It creates the Ingress from `team-a`'s base, records `createdByOperator: true` and
   `creatorPatchRef: team-a/...`.
3. It merges both rules by `host` and writes once. `ownedPaths` for each contributor records the one
   rule it owns. `priorValues` is empty — neither overwrote anything pre-existing.
4. Both contributors report `Ready=True`. `team-b` never had to know whether it or `team-a` would win
   the create race.

### A.2 The creating XR is deleted first, while the patch-only contributor remains

This is the case that breaks naive lead-follower designs, where deleting the "lead" takes the object
out from under everyone else.

1. `team-a`'s XR is deleted; GC deletes its `ClusterResourcePatch`, which enters `Terminating` with
   its finalizer held.
2. The tracker marks `team-a` `Releasing` and applies its `onRelease: Revert`:
   its rule is removed from `spec.rules`; `team-b`'s is untouched.
3. `team-a` is dropped from `status.contributors`; **only then** is its finalizer released.
4. `status.contributors` is not empty — `team-b` remains — so **no lifecycle action is taken on the
   object**. The Ingress survives, serving `team-b` only. `createdByOperator` and `creatorPatchRef`
   are retained: if `team-b` later leaves with `onRelease: Revert`, the object is left behind,
   emptied of contributions. Nothing deletes it, because the only contributor that was ever
   authorized to delete it is gone, and its departure did not request deletion.
5. If instead `team-a` had set `onRelease: Delete`, the delete would still not fire at step 4:
   deletion is evaluated only when the contributor list empties (§6.2). The object outlives its
   creator for exactly as long as someone is still using it — which is the entire point.

### A.3 Both contributors claim `spec.rules`, which is an atomic list

`Ingress.spec.rules` has no `listMapKey`, so under SSA it is atomic — one manager owns the whole
list.

- **Under `mode: ServerSideApply`:** the admission webhook reads the OpenAPI schema, sees a
  contribution writing into an atomic list, and warns with `ListMergeUnsupported`. At runtime the
  first contributor takes the entire list; the second gets a 409 naming it. With the default
  `conflictPolicy: Fail`, nothing is written and both report `Conflict` — visibly stuck rather than
  silently wrong. With `Priority`, the higher-priority contributor takes the whole list and the
  loser reports `Ready=False, reason=Superseded`; **its rule is genuinely absent**, which is correct
  reporting of a bad configuration, not a resolution.
- **Under `mode: ClientSideApply` with `mergeKeys: [{path: spec.rules, key: host}]`:** the operator
  merges the list itself, keying elements by `host`. Distinct hosts are not a conflict at all, both
  rules land, and each contributor's `ownedPaths` records `spec.rules[host=...]` so its own rule can
  be withdrawn later without touching the other's.
- **If both claimed the same `host` with different backends**, that is a genuine conflict in either
  mode, resolved by `conflictPolicy`.

This scenario is the justification for CSA existing. The most obvious use case for the operator is
unsatisfiable under SSA alone. It is also entirely scope-independent: nothing about atomic lists
changes between the namespaced and cluster-scoped pairs.

### A.4 A namespaced target is promoted mid-life

The case the scope split introduces, and the one where a careless implementation loses data.

`team-a` runs a shared Ingress *inside its own namespace*, `team-a/team-a-shared-ingress`. Two
`ResourcePatch`es in `team-a` contribute rules — the contained, low-privilege pattern of §7.0. A
`SharedResource` in `team-a` tracks it. `checkout-ingress-rule` created the object, so
`createdByOperator: true` and `creatorPatchRef` names it. Both contributors use ClientSideApply, and
one of them overwrote a pre-existing annotation, so its `priorValues` is non-empty.

The platform team now needs to add a rule to that same Ingress from outside `team-a` — say a shared
status endpoint. They author a `ClusterResourcePatch`.

1. The contributor controller resolves the target, finds the existing `SharedResource` in `team-a`,
   and sees that rule 3 of §3.5 now applies: a namespaced tracker cannot count a contributor that is
   not in its namespace.
2. **Fence.** `status.phase: Promoting` and `status.promotedTo` are committed on the `team-a`
   tracker. From this moment it writes nothing.
3. A `ClusterSharedResource` is created, copying both contributors — with their `ownedPaths` and
   `priorValues` — plus `observedTargetUID`, `observedResourceVersion`, `createdByOperator`,
   `creatorPatchRef` and `observedBaseHash`.
4. The `ClusterSharedResource` confirms adoption; the `team-a` tracker's finalizer is released and it
   is deleted.
5. Both `ResourcePatch`es follow `promotedTo` and re-register on the cluster tracker. The new
   `ClusterResourcePatch` registers alongside them.

What did **not** happen is the point:

- **The Ingress was never written during the migration.** No revert, no re-apply, no flap. A user
  watching the object sees nothing.
- **`checkout-ingress-rule` is still the creator.** `creatorPatchRef` survived the copy, so the
  delete-safety rule (§6.2) still knows that this object was operator-created and by whom. Had it
  been dropped, the newly-arrived `ClusterResourcePatch` would have found an object with no recorded
  creator — and "only the creator may delete" would have quietly stopped protecting anything.
- **`priorValues` survived**, so the contributor that overwrote an annotation will still *restore* it
  on release rather than deleting it.
- **The `ResourcePatch`es are unchanged and still namespaced.** They did not become privileged by
  being promoted; they still cannot name a target outside `team-a`. Only the bookkeeping moved.

When the platform team's `ClusterResourcePatch` is later deleted, the tracker stays a
`ClusterSharedResource` (§6.3). The two `ResourcePatch`es carry on against it exactly as before.
