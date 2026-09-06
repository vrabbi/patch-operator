# patch-operator — design

**Status:** pre-alpha, design only. No implementation yet.
**API group / version:** `terasky.com/v1alpha1`

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
*all* owners are gone. But:

- A namespaced dependent's owners must live in the **same namespace**. A shared object in `platform`
  cannot be owned by contributors in `team-a` and `team-b`.
- A **cluster-scoped** dependent cannot be owned by a namespaced object at all.
- GC is delete-or-don't. There is no "remove my fields and leave the object" semantic, which is what
  a patch-only contributor actually needs.
- GC deletion is unconditional once the last owner goes, so it silently overrides any policy the
  contributor expressed.

So reference counting has to be explicit, in an object the operator controls.

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

Two CRDs, and the split between them is the central design decision.

### 3.1 `ResourcePatch` (namespaced) — the contributor

One per contributing instantiation. **This is the only CRD users write.** A Crossplane Composition or
a kro RGD emits one of these; the XR/Instance owns it via `ownerReferences` in the normal way.

```yaml
apiVersion: terasky.com/v1alpha1
kind: ResourcePatch
metadata:
  name: team-a-ingress-rule
  namespace: team-a
spec:
  target:
    mode: Single                        # Single | Selector
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    # --- Single mode ---
    name: shared-ingress
    namespace: platform                 # omit for cluster-scoped kinds
    # --- Selector mode ---
    # selector:            { matchLabels: { tier: frontend } }
    # namespaceSelector:   { matchLabels: { env: prod } }
    # maxTargets:          50

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

  serviceAccountRef:                    # optional but recommended; see §7
    name: team-a-patcher
    namespace: team-a                   # defaults to .metadata.namespace

  base:                                 # only consulted when onMissing: Create
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    metadata:
      name: shared-ingress
      namespace: platform
    spec:
      ingressClassName: nginx

  patch:
    type: StrategicMerge                # StrategicMerge | Merge | JSON6902
    value:
      spec:
        rules:
          - host: team-a.example.com
            http:
              paths:
                - path: /
                  pathType: Prefix
                  backend:
                    service: { name: team-a, port: { number: 80 } }
    # For type: JSON6902 use `ops:` instead of `value:`
    # ops:
    #   - { op: add, path: /spec/rules/-, value: {...} }
    mergeKeys:                          # ClientSideApply only; see §5.2
      - path: spec.rules
        key: host

status:
  conditions:                           # Ready, Synced, TargetFound, Applied, Conflict, Authorized
    - type: Ready
      status: "True"
  observedTargets:
    - { apiVersion: networking.k8s.io/v1, kind: Ingress, name: shared-ingress,
        namespace: platform, uid: 8f3c..., state: Applied }
  sharedResourceRefs:
    - ingress.networking.k8s.io-platform-shared-ingress-a1b2c3
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

### 3.2 `SharedResource` (cluster-scoped) — the tracker

**Operator-owned. Users do not create these** (whether they *may* pre-declare one is a roadmap
question, §10). One per distinct target object, named deterministically from the target's GVK,
namespace and name — sanitised, with a hash suffix to stay inside DNS-1123 and 253 characters.

Cluster-scoped for two reasons: the target may itself be cluster-scoped, and the contributors may
live in several different namespaces. There is no namespace that is correct for it.

```yaml
apiVersion: terasky.com/v1alpha1
kind: SharedResource
metadata:
  name: ingress.networking.k8s.io-platform-shared-ingress-a1b2c3
  finalizers: [terasky.com/shared-resource]
spec:
  targetRef:
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    name: shared-ingress
    namespace: platform
status:
  phase: Applied                        # Waiting | Applied | Conflicted | Releasing
  createdByOperator: true
  creatorPatchRef: { namespace: team-a, name: team-a-ingress-rule, uid: 1111-... }
  observedBaseHash: sha256:...
  observedTargetUID: 8f3c-...
  observedResourceVersion: "148213"
  contributors:
    - patchRef: { namespace: team-a, name: team-a-ingress-rule, uid: 1111-... }
      observedGeneration: 4
      priority: 100
      fieldManager: patch-operator/team-a/team-a-ingress-rule
      state: Applied                    # Applied | Superseded | Conflicted | Releasing
      lastAppliedHash: sha256:...
      ownedPaths:                       # ClientSideApply bookkeeping only
        - spec.rules[host=team-a.example.com]
      priorValues: {}                   # ClientSideApply: original values, for restore-on-revert
  conflicts:
    - fieldPath: spec.rules
      claimants: [team-a/team-a-ingress-rule, team-b/team-b-ingress-rule]
      holder: team-a/team-a-ingress-rule
```

`status.contributors` is the reference count. It is derived state — rebuilt on every reconcile from a
live list of `ResourcePatch`es — so a lost update or a missed watch event self-heals rather than
corrupting the count.

---

## 4. Controller architecture

### 4.1 Reconcile the target, not the request

The single most important structural decision: **`ResourcePatch`es are inputs, and the
`SharedResource` is the unit of reconciliation.** The `SharedResource` controller is the *only* code
path that writes to a target object.

The naive alternative — one controller reconciling `ResourcePatch`es, each writing its own slice — is
what `provider-kubernetes` does today, and it is why that approach needs blind conflict forcing. With
N contributors writing independently you get N-way races on one object, non-deterministic apply
order, no place to notice that two contributors want the same field, and no coherent moment at which
to decide the object is now unreferenced.

Aggregating first fixes all four at once:

- writes to one target are serialised through one work queue key;
- apply order is a deterministic sort, stable across operator restarts;
- conflicts are detected before any write, because all contributions are in hand;
- the reference count is a list length, evaluated at one point in the code.

### 4.2 The two controllers

**`ResourcePatch` controller** — resolves and registers. It never writes to a target.

1. Validate the spec (mode/lifecycle compatibility, patch well-formedness).
2. Resolve `spec.target` to a concrete list of target keys. `Single` yields exactly one — whether or
   not the object exists. `Selector` yields zero or more, capped at `maxTargets`.
3. Ensure a `SharedResource` exists for each resolved key (create if absent; `AlreadyExists` is a
   normal, expected outcome and is not an error).
4. Add the `terasky.com/contributor` finalizer.
5. Enqueue each `SharedResource`.
6. Mirror the per-contributor state from `SharedResource.status.contributors` back onto
   `ResourcePatch.status` as conditions.

On deletion it does *not* revert anything itself; it enqueues the `SharedResource` and waits for it
to confirm the release before dropping the finalizer (§6).

**`SharedResource` controller** — the writer.

1. List bound `ResourcePatch`es via a **field index on the computed target key**, so this is a cheap
   indexed lookup rather than a cluster-wide scan. Selector-mode contributors are indexed against
   every key they currently resolve to.
2. Sort by `(priority desc, creationTimestamp asc, uid asc)`. The `uid` tiebreak matters: two
   contributors created in the same clock tick must still sort identically on every replica and after
   every restart, or the object flaps.
3. Read the live target. Absent → honour the highest-priority contributor's `onMissing`.
4. Detect conflicts across the sorted contributions (§5.3) and resolve per `conflictPolicy`.
5. Apply (§5).
6. Reconcile the reference count and lifecycle (§6).
7. Write status on both the `SharedResource` and, indirectly, each contributor.

### 4.3 Watching targets

Target GVKs are not known at compile time, so the operator maintains **dynamic informers created
lazily** on first use for a GVK and torn down when the last `SharedResource` for that GVK is deleted.
Target events map back to a `SharedResource` by key.

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

The cost is real bookkeeping. For each contributor the `SharedResource` records **`ownedPaths`** (what
this contributor set) **and `priorValues`** (what those paths held *before* this contributor first
touched them, for paths that already existed). Revert then restores the original value rather than
deleting the field — which is the correct behaviour when a contributor overwrote a pre-existing
setting rather than adding a new one, and is something SSA cannot express.

Two rules about that bookkeeping:

- It lives in `SharedResource.status`, **never in annotations on the target**. Annotations bloat,
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
`SharedResource.status.conflicts` with the field path, all claimants, and the current holder — so a
user can see who is fighting without reading `managedFields`.

Whatever the policy, the object must never flap. A conflict that cannot be resolved parks with a
condition and a backoff; it does not produce alternating writes.

---

## 6. Lifecycle & reference counting

Finalizers on both CRDs: `terasky.com/contributor` on `ResourcePatch`,
`terasky.com/shared-resource` on `SharedResource`.

### 6.1 Releasing one contributor

A `ResourcePatch` is deleted (usually because its XR or Instance was deleted, propagating through
`ownerReferences`). It enters `Terminating` with its finalizer held. The `SharedResource` controller:

1. Marks the contributor `Releasing`.
2. Applies its `onRelease` policy to that contributor's fields only:
   - **`Revert`** (default) — SSA: re-apply empty under its field manager. CSA: delete `ownedPaths`,
     restoring `priorValues` where present.
   - **`Orphan`** — leave the fields in place, untouched.
   - **`Delete`** — see §6.2; this is about the whole object, not the fields.
3. Removes the contributor from `status.contributors`.
4. Releases the `ResourcePatch` finalizer.

Order matters: the finalizer is released **only after** the revert is confirmed. A contributor that
vanishes before its fields are withdrawn leaves fields nobody owns and nobody can find.

### 6.2 Releasing the last contributor

When `status.contributors` empties:

- **`Orphan`** → leave the target; delete the `SharedResource`.
- **`Revert`** → withdraw the remaining fields; leave the target; delete the `SharedResource`.
- **`Delete`** → delete the target, **but only if both**:
  - `status.createdByOperator == true` — the operator created this object, so it is the operator's to
    remove; and
  - the request comes from `status.creatorPatchRef` — the contributor that actually created it.

That second condition is a hard safety property, not a nicety. Without it, any patch-only contributor
could set `onRelease: Delete`, attach itself to a pre-existing production `Deployment`, and delete it
on the way out. A contributor that did not create an object may never delete it, whatever it asks
for. Admission rejects `onRelease: Delete` combined with `onMissing: Fail|Wait` for the same reason,
and the runtime check stands independently in case the spec was mutated afterwards.

### 6.3 Why not `ownerReferences` on the target

Worth answering directly, because it is the first thing anyone reaches for.

Multi-owner GC *is* real reference counting and it is free. But it is unusable here: owners must be
in the target's namespace, cluster-scoped targets cannot have namespaced owners, and — decisively —
GC deletes unconditionally when the last owner disappears. That silently overrides
`onRelease: Orphan` and `onRelease: Revert`, which are the two most common policies. Running both
mechanisms means two independent deletion authorities over one object, one of which ignores the
user's stated policy. **This design does not put `ownerReferences` on targets.** The `SharedResource`
plus finalizers is the sole lifecycle authority, and it behaves identically for namespaced and
cluster-scoped targets.

The ownership chain that *does* work, and that this design depends on:

```
XR / kro Instance  --ownerRef-->  ResourcePatch        (the orchestrator's job)
ResourcePatch      --tracked by-> SharedResource       (the operator's job)
SharedResource     --manages---->  target object       (the operator's job)
```

Deletion propagates *in* from the orchestrator by ordinary GC, and is *arbitrated* by the operator.
Each layer does what it is good at.

### 6.4 Recovering from lost state

- **Missed delete events.** `status.contributors` is rebuilt from a live list on every reconcile, so a
  contributor whose `ResourcePatch` no longer exists is detected and released on the next pass. A
  periodic full resync sweeps `SharedResource`s that received no events at all.
- **Target replaced out of band.** A changed `observedTargetUID` invalidates all CSA bookkeeping,
  raises `TargetReplaced`, and triggers a full re-apply.
- **Target deleted out of band.** If any contributor sets `onMissing: Create`, recreate from that
  contributor's `base` and re-apply all contributions; otherwise park in `Waiting`.
- **Empty `SharedResource`.** A tracker with no contributors and no lifecycle work pending is deleted.

---

## 7. Authorization

**Start here when reviewing this design.** The hard problem is not merging — SSA does most of that.
The hard problem is that this operator is an *"apply arbitrary fields to arbitrary objects"*
primitive. Whoever can create a `ResourcePatch` inherits whatever the write path can do. Two layers
address that: SubjectAccessReview at admission, and impersonation at write time.

### 7.1 SubjectAccessReview on every mutation

A validating webhook on `ResourcePatch` for **CREATE, UPDATE and DELETE** — not create alone — with
`failurePolicy: Fail`. Each verb runs `SubjectAccessReview`s against the **target**, using
`request.userInfo` as the subject. The question asked is always: *could this principal have made this
change to the target directly?*

| Verb on the `ResourcePatch` | SARs against the target |
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
- Also denied at admission: a `ResourcePatch` targeting a `ResourcePatch` or a `SharedResource`. This
  is **reconcile-loop safety**, not a sensitive-kind policy.

**There is no protected-GVK allowlist or denylist in this design.** Authorization is SAR plus
impersonation. The operator holds no opinion about which kinds are sensitive; RBAC already encodes
that, and duplicating it in operator configuration produces a second, divergent policy surface that
cluster admins must remember to maintain.

### 7.2 Re-checking after admission

Admission is point-in-time. A `ResourcePatch` created while its author held broad rights keeps
working forever after those rights are revoked, because nothing ever touches the CR again.

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
- **`serviceAccountRef.namespace` defaults to the `ResourcePatch`'s own namespace.** A cross-namespace
  reference is gated by that same `impersonate` SAR, and can be disabled cluster-wide by a flag.
- **When `serviceAccountRef` is unset**, writes use the operator's own identity, bounded only by the
  admission SAR. That is the weaker configuration. Docs should recommend `serviceAccountRef` for any
  cluster with more than one tenant.

### 7.4 The caveat to state loudly

When Crossplane or kro creates the `ResourcePatch`, `request.userInfo` is the **orchestrator's**
ServiceAccount — Crossplane's or kro's controller SA — not the human who created the XR. Those SAs
are typically close to cluster-admin, because they have to be able to create anything a Composition
might reference.

So SAR meaningfully bounds a user creating `ResourcePatch`es directly, and does comparatively little
against a Composition author, who is writing patches that will be admitted as a near-cluster-admin.
The real boundary for composed patches is `serviceAccountRef`: the Composition or RGD author sets it
to voluntarily drop the write path to a tenant identity, and a platform team can require it. This is
exactly why it belongs in v1alpha1 and not on the roadmap.

Documentation must say this plainly rather than implying that admission checks alone make the
operator safe in a multi-tenant cluster. They do not.

---

## 8. Failure modes

| Failure | Behaviour |
|---|---|
| **Atomic list under SSA** (§5.2) | Two contributors cannot co-own a list without merge keys. Detected from the OpenAPI schema at admission; warns and sets `ListMergeUnsupported`, pointing at `ClientSideApply` + `mergeKeys`. |
| **Foreign controller uses `Update`, not `Apply`** | `Update` collapses `managedFields` ownership. Field values survive; ownership records degrade. The operator detects that its manager entry vanished while its fields persist, and re-applies to reassert ownership. Frequent recurrence is reported — that target has a controller this operator cannot cooperate with. |
| **Target deleted out of band** | Recreate from `base` if any contributor sets `onMissing: Create`; otherwise `Waiting`. |
| **Target replaced (UID changed)** | Invalidate CSA bookkeeping, `TargetReplaced`, full re-apply (§6.4). |
| **Webhook unavailable** | `failurePolicy: Fail` means `ResourcePatch` writes are rejected, which blocks XR reconciliation. This is the correct trade for an authorization webhook, but it makes webhook availability a hard dependency: run ≥2 replicas, and document it as a cluster-wide blast radius. |
| **Selector fan-out blast radius** | `maxTargets` caps a single contributor; exceeding it fails the contributor rather than silently truncating. Applies are rate-limited across a fan-out so one `ResourcePatch` cannot saturate the API server. |
| **This operator and `Object` on the same resource** | Both use SSA field managers, so they coexist if their fields are disjoint. `conflictPolicy: Priority` deliberately will not force against a foreign manager, so `Object`'s unconditional forcing wins any genuine overlap and the contributor reports `Conflict` rather than flapping. Document: pick one owner per field. |
| **Operator down** | Nothing reverts, nothing is deleted (finalizers hold `ResourcePatch` deletions pending). Targets keep their last applied state. Deleting an XR blocks until the operator returns — an availability cost that must be documented. |
| **Contributor stuck terminating** | If a revert cannot complete (target unreachable, authorization lost), the `ResourcePatch` finalizer is held and the condition says why. Provide a documented, explicit break-glass for dropping the finalizer, since a wedged finalizer otherwise blocks XR deletion indefinitely. |

---

## 9. Observability

**Conditions** on both CRDs, in the Crossplane shape (`type`, `status`, `reason`, `message`,
`lastTransitionTime`) so XR readiness checks and kro status roll-ups consume them directly:
`Ready`, `Synced`, `TargetFound`, `Applied`, `Conflict`, `Authorized`.

**Events** on the `ResourcePatch` (what happened to my contribution), on the `SharedResource` (what
happened to this target), and on the target itself (who contributed and when — so someone reading
`kubectl describe` on a shared Ingress can see this operator is involved without knowing it exists).

**Metrics:** contributors per target (a histogram — the tail identifies objects that have become
coordination bottlenecks); conflicts by field path; forced conflicts (should be near zero; a rising
count means users are papering over a real disagreement); apply latency by mode; revert failures;
watched-GVK count against the cap; SAR denials and impersonation failures.

**`kubectl` printer columns** on `SharedResource`: target kind/namespace/name, contributor count,
phase, conflict count, age. Answering "who is writing to this object?" should be one command.

---

## 10. Open questions & roadmap

- **Contribution rendering.** Whether to support CEL or Go templates inside `patch.value`. Currently
  out of scope on the grounds that Crossplane patches and kro CEL already do this upstream, but a
  contributor that needs a value derived from the *live target* has no way to express it today.
- **`baseReconcile: Enforce`.** Continuously re-asserting the base needs its own priority semantics
  against contributions, and reintroduces the base-versus-patch fight `CreateOnly` avoids (§3.1).
- **User-creatable `SharedResource`.** Letting a platform team pre-declare a shared target — its
  base, its apply mode, its policy — before any contributor exists would move policy from N
  contributors to one place. Attractive, but it makes the tracker a user-facing API with its own
  authorization story.
- **Requiring `serviceAccountRef` cluster-wide.** A flag making it mandatory would give platform
  teams a single switch for multi-tenant safety (§7.4).
- **Contributor-visible target state.** Exposing selected live target fields on `ResourcePatch.status`
  so an XR can consume them, overlapping with kro's `externalRef`.
- **API group naming.** `terasky.com/v1alpha1` for now; revisit before any v1beta1.

---

## Appendix A — worked scenarios

Three walkthroughs, each chosen because it is where a design like this usually breaks.

### A.1 Two XRs contribute to one Ingress; one creates, one only patches

`team-a` (priority 100) sets `onMissing: Create` with a `base` and contributes a rule for
`team-a.example.com`. `team-b` (priority 100) sets `onMissing: Wait`, no `base`, and contributes a
rule for `team-b.example.com`. Both use `mode: ClientSideApply` with
`mergeKeys: [{path: spec.rules, key: host}]` — see A.3 for why.

1. Both `ResourcePatch` controllers resolve to the same target key and race to create the
   `SharedResource`. One wins; the other's `AlreadyExists` is normal. Both register as contributors.
2. The `SharedResource` controller sorts them (equal priority → `creationTimestamp`, then `uid`).
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

1. `team-a`'s XR is deleted; GC deletes its `ResourcePatch`, which enters `Terminating` with its
   finalizer held.
2. The `SharedResource` controller marks `team-a` `Releasing` and applies its `onRelease: Revert`:
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
unsatisfiable under SSA alone.
