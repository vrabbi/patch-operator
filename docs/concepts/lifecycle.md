# Lifecycle and reference counting

Everything here is scope-independent. Which tracker owns a target is decided by the
[scope model](scope-model.md); once decided, release and deletion behave identically in both
variants.

## Policies

```yaml
lifecycle:
  onMissing: Create      # Fail | Wait | Create
  onRelease: Revert      # Revert | Orphan | Delete
  adoptExisting: true
  baseReconcile: CreateOnly
```

### `onMissing` — what if the target does not exist?

| Value | Behaviour |
|---|---|
| `Wait` (default) | Park until some other contributor creates it. Reports `Ready=False`. |
| `Fail` | Report an error. |
| `Create` | Create it from `spec.base`. Requires `spec.base`. Invalid in Selector mode. |

### `onRelease` — what happens to my fields when I go?

| Value | Behaviour |
|---|---|
| `Revert` (default) | Withdraw this contributor's fields, restoring any prior values. |
| `Orphan` | Leave the fields in place, untouched. |
| `Delete` | Delete the **whole target** once the last contributor releases — subject to the rule below. |

## `base` and `patch` are separate, deliberately

`base` answers "if I have to create this object, what should it look like?"; `patch` answers "what do
I always contribute?". A patch-only contributor omits `base` entirely.

Keeping them separate is what stops the base and the contributions fighting. Under the default
`baseReconcile: CreateOnly` the base is written **exactly once**, at creation, and never
re-asserted — so it can never claw back a field a contributor later set.

!!! note "Disagreeing bases are loud, not flappy"
    If several contributors supply a `base`, the first to win the create race creates the object.
    The others compare theirs against `status.observedBaseHash` and, if it differs, raise a
    `BaseMismatch` condition and stop. They do **not** re-apply. A disagreement about the seed is a
    configuration error and should be visible, not an oscillating object.

`baseReconcile: Enforce` is **rejected in v1alpha1** rather than silently doing nothing: it needs
its own priority semantics against contributions, which reintroduces the fight `CreateOnly` avoids.

## Releasing one contributor

A contributor is deleted — usually because its XR or Instance was deleted, propagating through
`ownerReferences`. It enters `Terminating` with its finalizer held, and its tracker:

1. Marks it `Releasing`.
2. Applies its `onRelease` policy **to that contributor's fields only**.
3. Removes it from `status.contributors`.
4. Releases the contributor's finalizer.

!!! warning "The ordering is the point"
    The finalizer is released **only after** the withdrawal is confirmed. A contributor that
    vanished before its fields were withdrawn would leave fields nobody owns and nobody can find.

## Releasing the last contributor

When `status.contributors` would empty:

- **`Orphan`** → leave the target; delete the tracker.
- **`Revert`** → withdraw the remaining fields; leave the target; delete the tracker.
- **`Delete`** → delete the target, **but only if both**:
    - `status.createdByOperator` is true — the operator created this object, so it is the
      operator's to remove; **and**
    - the request comes from `status.creatorPatchRef` — the contributor that **actually created
      it**, matched by kind, name, namespace **and UID**.

!!! danger "Why the creator check is not a nicety"
    Without it, any patch-only contributor could set `onRelease: Delete`, attach itself to a
    pre-existing production `Deployment`, and delete it on the way out.

    **A contributor that did not create an object may never delete it, whatever it asks for.**

    Admission rejects `onRelease: Delete` combined with `onMissing: Fail|Wait` for the same reason,
    and the runtime check stands independently in case a spec reached the cluster without admission.
    A contributor that *adopted* an existing object rather than creating it also cannot delete it,
    because `createdByOperator` is false.

### The case that breaks lead/follower designs

A creator leaving **while other contributors remain** must not take the object with it:

```
t0  creator (Create+Delete) and patcher (Wait+Revert) both contribute
t1  creator's XR is deleted
t2  creator's fields are withdrawn; patcher's are untouched
t3  contributors is NOT empty -> no object-level action at all
    the object survives, serving patcher only
```

The object outlives its creator for exactly as long as someone is still using it — which is the
entire point. If `patcher` later leaves with `Revert`, the object is left behind, emptied of
contributions. Nothing deletes it, because the only contributor ever authorized to do so is gone and
its departure did not request it.

## Recovering from lost state

| Situation | Behaviour |
|---|---|
| **Missed delete events** | `status.contributors` is rebuilt from a live list every reconcile, so a contributor whose CR no longer exists is released on the next pass. |
| **Target replaced out of band** | A changed `observedTargetUID` invalidates all client-side bookkeeping, raises `TargetReplaced`, and triggers a full re-apply. |
| **Target deleted out of band** | Recreated from `base` if any contributor sets `onMissing: Create`; otherwise `Waiting`. |
| **Empty tracker** | Deleted once it has no contributors and no lifecycle work pending. |
| **Interrupted promotion** | Resumed from the fence. See [Promotion](promotion.md). |
| **Namespace deleted** | Target, tracker and contributors go together, since all three live there. Finalizers are released promptly so the namespace does not wedge in `Terminating`. |
