# Conflicts

A conflict is two contributors setting **the same field path to different values**.

!!! success "Same path, same value is not a conflict"
    It is redundancy, and it is allowed — two XRs asking for the same ingress class is normal. A
    co-owned field also survives one of its owners releasing it.

## Policies

```yaml
apply:
  conflictPolicy: Fail    # Fail | Priority | Force
```

### `Fail` (default)

Report the conflict and write nothing. Both contributors go `Ready=False` with reason `Conflict`,
and the tracker records the contested path and all claimants.

This is the honest default: a disagreement between two contributors is a configuration problem, and
resolving it silently means nobody finds out.

### `Priority`

The highest `priority` claimant wins; the loser reports `Ready=False, reason=Superseded`.

!!! warning "The loser's fields are genuinely absent"
    `Superseded` is reported rather than hidden. Losing quietly would be worse than failing loudly —
    an XR must not report ready while its contribution is missing.

`Priority` forces **only when every conflicting manager is another patch-operator manager.**

!!! danger "Never against a foreign controller"
    Forcing a field away from a controller that will write it straight back is a **flap, not a
    resolution** — the object oscillates indefinitely. When the current owner is foreign (say
    `provider-kubernetes` or a native controller), `Priority` reports the conflict instead.

    This is the concrete improvement over `provider-kubernetes` `Object`, which forces
    unconditionally.

### `Force`

Unconditional. Opt-in, documented as a last resort.

## Ordering

Contributors are sorted `(priority desc, creationTimestamp asc, uid asc)`.

The `uid` tiebreak is not decoration: two contributors created in the same clock tick must sort
identically on every replica and after every restart, or the target flaps as leadership moves.

## Reading a conflict

```bash
kubectl get sharedresource -n team-a <name> -o jsonpath='{.status.conflicts}' | jq
```

```json
[
  {
    "fieldPath": "spec.rules",
    "claimants": ["ResourcePatch/team-a/checkout", "ResourcePatch/team-a/search"],
    "holder": "ResourcePatch/team-a/checkout"
  }
]
```

Claimants are qualified by kind, since a namespaced and a cluster-scoped contributor can be party to
the same conflict after a [promotion](promotion.md).

The point of recording this is that a user can see **who is fighting** without reading
`managedFields`.

## The atomic-list trap

The most common cause of an unexpected conflict is not two contributors wanting the same field — it
is an [atomic list](apply-modes.md#clientsideapply-and-why-it-is-not-a-fallback).

Under `ServerSideApply`, `Ingress.spec.rules` has no merge key, so the **first** contributor takes
the entire list and the second gets a conflict on `spec.rules` even though the two wanted different
hosts.

- With `conflictPolicy: Fail`, nothing is written and both report `Conflict` — visibly stuck rather
  than silently wrong.
- With `Priority`, the winner takes the whole list and the loser's rule is genuinely absent. That is
  correct reporting of a bad configuration, not a resolution.

**The fix is `ClientSideApply` with a declared merge key**, under which distinct hosts are not a
conflict at all:

```yaml
apply:
  mode: ClientSideApply
patch:
  mergeKeys:
    - path: spec.rules
      key: host
```

If both contributors claimed the *same* host with different backends, that is a genuine conflict in
either mode, resolved by `conflictPolicy`.

## Never a flap

Whatever the policy, the object must never oscillate. A conflict that cannot be resolved parks with
a condition and a backoff; it does not produce alternating writes.

!!! tip "Watch the forced-conflict metric"
    A rising count of forced conflicts means users are papering over a real disagreement. It should
    be near zero.
