# Troubleshooting

Start by reading the contributor's conditions and its tracker's status. Between them they say what
the operator decided and why.

```bash
kubectl describe resourcepatch -n team-a my-patch
kubectl get sharedresources -n team-a
kubectl get clustersharedresources
```

## Conditions

| Condition | Meaning |
|---|---|
| `Ready` | Applied to every resolved target **and** not party to an unresolved conflict. |
| `Synced` | The last reconcile completed without error. |
| `TargetFound` | Every resolved target exists. |
| `Applied` | The contribution reached every resolved target. |
| `Conflict` | This contributor is party to a field conflict. |
| `Authorized` | The recorded principal's SubjectAccessReview re-check still passes. |

## `Ready=False, reason=WaitingForTarget`

The target does not exist and this contributor does not create it.

Expected while another contributor is still coming up. If it persists, either nothing is set to
create the target, or the creating contributor is itself failing:

```bash
kubectl get resourcepatches -n team-a \
  -o custom-columns='NAME:.metadata.name,ONMISSING:.spec.lifecycle.onMissing,READY:.status.conditions[?(@.type=="Ready")].status'
```

If none says `Create`, add `onMissing: Create` with a `spec.base` to whichever contributor should
own creation.

## `Ready=False, reason=Conflict`

Two contributors want the same field path with different values.

```bash
kubectl get sharedresource -n team-a <tracker> -o jsonpath='{.status.conflicts}' | jq
```

!!! tip "First check whether it is really an atomic list"
    The most common cause is **not** two contributors wanting the same field. It is
    `ServerSideApply` against a list with no merge key — `Ingress.spec.rules` being the usual
    culprit — where the first contributor takes the *whole list* and the second conflicts on
    `spec.rules` even though they wanted different hosts.

    The fix is `apply.mode: ClientSideApply` with `patch.mergeKeys`. See
    [Apply modes](../concepts/apply-modes.md).

If it is a genuine disagreement, decide it: change one contributor's value, or set
`conflictPolicy: Priority` and give the intended winner a higher `priority`.

## `Ready=False, reason=Superseded`

A higher-priority contributor holds a contested field, so **this contribution is genuinely absent**.

This is reported rather than hidden on purpose — an XR must not report ready while its contribution
is missing. Either raise this contributor's `priority`, or stop claiming the contested field.

## `Ready=False, reason=InvalidSpec`

The spec cannot be honoured. The message names the field.

Common ones:

| Message contains | Fix |
|---|---|
| `own namespace` | A `ResourcePatch` named another namespace. Use a `ClusterResourcePatch`, or move the contributor. |
| `namespaced kinds` | A `ResourcePatch` targeted a cluster-scoped kind. Use a `ClusterResourcePatch`. |
| `requires onMissing: Create` | `onRelease: Delete` needs `onMissing: Create` — only the creating contributor may delete. |
| `namespace is required` | A `ClusterResourcePatch` targeting a namespaced kind must name the namespace. |
| `mergeKeys only apply under` | Move to `apply.mode: ClientSideApply`, or drop the merge keys. |
| `maxTargets` | The selector matched too many objects. Narrow it or raise the cap. |
| `not implemented in v1alpha1` | `baseReconcile: Enforce` is not available yet. |

## `Ready=False, reason=BaseMismatch`

Two contributors supplied different `spec.base` values for the same target. The first to win the
create race created the object; this one refuses to re-apply a conflicting seed.

Make the bases identical, or give only one contributor a base and set the others to
`onMissing: Wait`.

## `Authorized=False`

The principal recorded at admission no longer passes its SubjectAccessReview — usually because an
RBAC binding was changed.

```bash
kubectl get resourcepatch -n team-a my-patch \
  -o jsonpath='{.metadata.annotations.terasky\.com/authorized-as}'
```

!!! note "The operator stops writing but does not revert"
    Losing authorization is not the same as being released. Silently tearing down a tenant's ingress
    rule because a binding was reorganised would be worse than the exposure.

Restore the binding, or recreate the contributor under a principal that is authorized.

## Admission rejects the create

The webhook denies with the reason inline:

```
admission webhook "vresourcepatch.terasky.com" denied the request:
  alice may not patch ingresses.networking.k8s.io team-a/shared-ingress,
  so it may not create a contributor that does: not permitted by RBAC
```

You need the same rights on the **target** that the contributor would exercise. Check them
directly:

```bash
kubectl auth can-i patch ingress/shared-ingress -n team-a --as alice
```

For `serviceAccountRef` you also need `impersonate` on that ServiceAccount:

```bash
kubectl auth can-i impersonate serviceaccounts/team-a-patcher -n team-a --as alice
```

## Everything hangs and no contributor can be created

Check the webhook. It uses `failurePolicy: Fail`, so while it is unreachable **no contributor can be
created and any XR composing one blocks**.

```bash
kubectl -n patch-operator-system get pods
kubectl get validatingwebhookconfiguration patch-operator-validating-webhook-configuration -o yaml
# cert-manager must have populated caBundle
kubectl -n patch-operator-system get secret webhook-server-cert
```

Usually cert-manager is missing or the certificate has not been issued.

## A contributor is stuck in `Terminating`

Its finalizer is held until the tracker confirms its fields are withdrawn. That is deliberate: a
contributor that vanished first would leave fields nobody owns and nobody can find.

```bash
kubectl get resourcepatch -n team-a my-patch -o jsonpath='{.status.conditions}' | jq
kubectl -n patch-operator-system logs -l control-plane=controller-manager --tail=200
```

Common causes: the operator is down, the target's namespace is terminating, or authorization was
lost so the revert write is rejected.

!!! danger "Removing the finalizer by hand"
    Only as break-glass, and understand the cost: the contributor's fields stay on the shared object
    with nothing left to withdraw them.

    ```bash
    kubectl patch resourcepatch -n team-a my-patch \
      --type=merge -p '{"metadata":{"finalizers":[]}}'
    ```

## A promotion seems stuck

```bash
kubectl get sharedresource -n team-a <tracker> -o jsonpath='{.status.promotedTo}'
```

- `promotedTo` set, `adopted: false` — the successor has not confirmed the state copy yet. The
  fenced tracker writes nothing meanwhile, so the target is safe; it retries.
- `promotedTo` set and the successor is missing — the operator unfences and retries, rather than
  leaving the target with no writer.

The namespaced tracker is deleted only after adoption is flagged. See
[Promotion](../concepts/promotion.md).

## Drift is corrected slowly

Target watches are created lazily per kind and capped by `--max-watched-target-kinds` (default 50).
Kinds beyond the cap converge on the requeue interval rather than on events — correctness is
unchanged, responsiveness is not.

```bash
kubectl -n patch-operator-system logs -l control-plane=controller-manager | grep "watched-GVK cap"
```

## Useful commands

```bash
# Who contributes to this object?
kubectl get sharedresource -n team-a <tracker> -o jsonpath='{.status.contributors[*].patchRef}' | jq

# What does a contributor own?
kubectl get sharedresource -n team-a <tracker> \
  -o jsonpath='{.status.contributors[?(@.patchRef.name=="my-patch")].ownedPaths}'

# Field ownership as the API server sees it
kubectl get ingress -n team-a shared-ingress -o jsonpath='{.metadata.managedFields[*].manager}'

# Operator logs
kubectl -n patch-operator-system logs -l control-plane=controller-manager -f
```
