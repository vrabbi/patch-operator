# Design document

The full design lives in
[`DESIGN.md`](https://github.com/vrabbi/patch-operator/blob/main/DESIGN.md) in the repository root.

It is the reasoning behind the implementation rather than user documentation: prior art and where
each existing tool stops, the API shapes and why they are split the way they are, the failure modes
the design accepts, and four worked scenarios chosen because they are where a design like this
usually breaks.

Worth reading if you want to know **why**, rather than **how**:

- **§2 Prior art** — what `provider-kubernetes` `Object`, kro `externalRef`, Kyverno mutate-existing
  and multi-owner GC each give, and the specific gap in each.
- **§4.1 Reconcile the target, not the request** — the structural decision the whole operator rests
  on, and what goes wrong without it.
- **§5.2 ClientSideApply** — why it is not a legacy fallback.
- **§6.4 Why not `ownerReferences`** — answered honestly per variant, including conceding the case
  where GC genuinely would work.
- **§7 Authorization** — the hardest part, and the reason the scope split exists.
- **§8 Failure modes** — including the ones the design accepts rather than solves.
- **Appendix A** — four end-to-end walkthroughs, each mirrored by a test in the repository.

## Deviations

Where the implementation differs from the document as first written, the document is the record of
intent and the code is the record of fact. Two things came out of building it:

- **`baseReconcile: Enforce`** is specified but rejected at admission in v1alpha1, rather than
  silently accepted and ignored.
- **Target watches** cannot be cleanly stopped — controller-runtime has no un-watch — so a kind
  watched for a target that later disappears keeps its informer until the process restarts. The GVK
  cap is what bounds that. The design notes the limitation rather than papering over it.
