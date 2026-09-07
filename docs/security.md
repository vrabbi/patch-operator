# Security

Read this page before deploying. This operator is an **"apply arbitrary fields to arbitrary
objects" primitive**: whoever can create a contributor inherits whatever the write path can do.
The merging is the easy part.

Three layers address that, in decreasing order of how much they can be relied on.

## 1. Containment by scope — the primary control

The scope split is the strongest property in this design, because it is **structural rather than an
authorization check**.

A `ResourcePatch` in namespace `team-a` cannot touch an object outside `team-a`. Not "is not
permitted to" — *cannot*. The target namespace is forced to the contributor's own namespace at
admission **and re-checked at reconcile**, and no field expresses anything else.

That matters because it holds **regardless of how privileged the principal creating it is**.

!!! danger "The caveat that makes this necessary"
    When Crossplane or kro creates a contributor, the principal seen at admission is the
    **orchestrator's ServiceAccount** — not the human who created the XR. Those ServiceAccounts are
    typically close to cluster-admin, because they have to be able to create anything a Composition
    might reference.

    So every check that reasons about *the requester* is weak for exactly the population of patches
    that matters most. Containment does not reason about the requester at all.

### What this means operationally

- **Grant `ResourcePatch` freely** in tenant namespaces — to tenants, and to Crossplane and kro.
  The blast radius of that grant is one namespace, whatever a Composition author writes.

    ```bash
    kubectl create rolebinding team-a-patches \
      --clusterrole=resourcepatch-editor \
      --serviceaccount=crossplane-system:crossplane \
      -n team-a
    ```

- **Treat `ClusterResourcePatch` as a platform-team grant.** It is the only kind that can cross a
  namespace boundary or touch a cluster-scoped object, so it is the only one that needs the
  scrutiny the rest of this page describes. **Withholding it from the orchestrators is what makes
  the containment above a real boundary rather than a convention.**

- **Consider a namespace-only install.** Omit the cluster-scoped CRDs entirely and cross-namespace
  contribution is not merely denied but *absent* — there is no API to express it, and the operator
  needs no cluster-wide write RBAC. See [Install](guides/install.md).

    ```bash
    make deploy-namespaced-only IMG=...
    ```

## 2. SubjectAccessReview on every mutation

A validating webhook on both contributor kinds, for **CREATE, UPDATE and DELETE**, with
`failurePolicy: Fail`. Each verb runs `SubjectAccessReview`s against the *target*, using
`request.userInfo`. The question is always: *could this principal have made this change to the
target directly?*

| Verb on the contributor | SARs against the target |
|---|---|
| **CREATE** | `patch` and `update`; plus `create` if `onMissing: Create`; plus `delete` if `onRelease: Delete` |
| **UPDATE** | the same set, against **both** the old and the new target when `spec.target` changes |
| **DELETE** | `patch`/`update` for `Revert`; `delete` for `Delete`; **none** for `Orphan`, which touches nothing |

**Covering UPDATE and DELETE is what makes this sound.** Create-only checking is bypassed trivially:
create a harmless patch, then update it to point at a different target, or flip `onRelease` to
`Delete` and delete the CR. Both are privilege escalations a create-time check never sees.

Details worth knowing:

- A **DELETE `AdmissionReview` carries `oldObject` and no `object`**, so the lifecycle policy that
  decides which SAR to run is read from `oldObject`. Reading it from the wrong side would fail open.
- In **Selector mode** the matched names are unknown at admission, so the SAR is issued against the
  resource and namespace with an **empty name** — which asks "may this principal patch *any* object
  of this kind here?". That is the stricter question, and the correct one, since a selector may
  match objects that do not exist yet.
- A contributor may not target this operator's **own kinds**. That is reconcile-loop safety, not a
  sensitive-kind policy.

!!! info "There is no protected-kind allowlist or denylist"
    Authorization is SAR plus impersonation. The operator holds no opinion about which kinds are
    sensitive — RBAC already encodes that, and duplicating it in operator configuration produces a
    second, divergent policy surface that admins must remember to maintain.

### Re-checking after admission

Admission is point-in-time. A contributor created while its author held broad rights would keep
working forever after those rights were revoked, because nothing ever touches the object again.

So the webhook records the admitting principal in the `terasky.com/authorized-as` annotation, and
the controller re-runs the SAR on a TTL (`--reauthorize-after`, default 10 minutes) and immediately
whenever the effective target or lifecycle differs from what was admitted. The result is the
`Authorized` condition.

When it goes false the operator **stops writing but does not revert**. Losing authorization is not
the same as being released, and silently tearing down a tenant's ingress rule because an RBAC
binding was reorganised would be worse than the exposure.

## 3. `spec.serviceAccountRef` — impersonation

The second real boundary, and the answer to the caveat above.

When set, the operator performs **every** target write while impersonating that ServiceAccount. The
API server then enforces that identity's RBAC on the write itself — continuously, at write time,
using the tenant's rights rather than the operator's.

```yaml
spec:
  serviceAccountRef:
    name: team-a-patcher     # on ResourcePatch: always resolved in the contributor's own namespace
```

Guard rails, so impersonation is not itself the escalation:

- **The webhook runs an extra SAR for verb `impersonate`** on the referenced ServiceAccount. You
  may only point a contributor at a ServiceAccount you could already impersonate. Without this
  check, `serviceAccountRef` would be a way to *borrow* privilege rather than drop it.
- **A `ResourcePatch` may only name a ServiceAccount in its own namespace** — the field has no
  `namespace` sub-field there at all, so containment covers the identity the write runs as.
  `ClusterResourcePatch` may name any ServiceAccount, gated by that same `impersonate` SAR.
- **Impersonating clients bypass the operator's shared cache.** The cache is populated with the
  operator's own credentials, so a cached read would silently bypass the tenant's RBAC.

!!! tip "When `serviceAccountRef` is unset"
    Writes use the operator's own identity, bounded only by the admission SAR. That is the weaker
    configuration. **Set `serviceAccountRef` on any cluster with more than one tenant** — and set it
    whenever a Composition emits a `ClusterResourcePatch`, where it is the only real boundary.

## What the operator's own RBAC looks like

Necessarily broad on target objects: the operator cannot know at install time which kinds a
contributor will name.

```yaml
- apiGroups: ["*"]
  resources: ["*"]
  verbs: [get, list, watch, create, update, patch, delete]
```

Two things bound that, and they are the whole point of the design: a `ResourcePatch` cannot reach
outside its own namespace whatever the requester's rights, and `serviceAccountRef` moves the write
path onto a tenant identity the API server checks on every write. A namespace-only install drops the
cluster-scoped half entirely.

The `impersonate` grant is what `serviceAccountRef` needs, gated at admission by the `impersonate`
SAR described above.

## Availability is a security trade

`failurePolicy: Fail` is the correct choice for an authorization webhook, and it makes webhook
availability a **hard dependency**: while the webhook is down, no contributor can be created, and
any XR that composes one blocks.

The shipped Deployment runs **two replicas** for that reason. Leader election means only one
reconciles, so the second replica is there for the webhook and for failover, not for throughput.
