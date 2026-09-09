# API reference

!!! note "Generated from the Go types"
    The reference below is produced by `make api-docs` from the doc comments in `api/v1alpha1/`,
    and CI fails if it has drifted. To change it, edit the Go doc comments rather than the
    generated file.

## Reading this reference

Two things are worth knowing before you scan the fields.

**The two contributor kinds share one spec.** `ResourcePatch` and `ClusterResourcePatch` both use
`ResourcePatchSpec`, and the two trackers both use `SharedResourceSpec` and
`SharedResourceStatus`. This is one API with two reaches, not two APIs — see the
[scope model](../concepts/scope-model.md) for what actually differs.

**The trackers are operator-owned.** You will not create a `SharedResource` or
`ClusterSharedResource`; you read them to find out who contributes to a shared object and what each
contributor owns.

--8<-- "reference/api-generated.md"
