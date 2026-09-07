# API Reference

## Packages
- [terasky.com/v1alpha1](#teraskycomv1alpha1)


## terasky.com/v1alpha1

Package v1alpha1 contains API Schema definitions for the terasky.com v1alpha1 API group.

Four kinds in two scope-matched pairs, mirroring Role/ClusterRole: ResourcePatch +
SharedResource are namespaced and confined to one namespace; ClusterResourcePatch +
ClusterSharedResource are cluster-scoped and reach across namespaces.


### Resource Types
- [ClusterResourcePatch](#clusterresourcepatch)
- [ClusterSharedResource](#clustersharedresource)
- [ResourcePatch](#resourcepatch)
- [SharedResource](#sharedresource)



#### ApplyMode

_Underlying type:_ _string_

ApplyMode selects how the contribution reaches the target.

_Validation:_
- Enum: [ServerSideApply ClientSideApply]

_Appears in:_
- [ApplySpec](#applyspec)

| Field | Description |
| --- | --- |
| `ServerSideApply` | ApplyModeServerSideApply lets the API server merge and track field ownership. Preferred.<br /> |
| `ClientSideApply` | ApplyModeClientSideApply merges in the operator, using spec.patch.mergeKeys for list<br />granularity the target's schema does not declare. Required for atomic lists.<br /> |


#### ApplySpec



ApplySpec declares how the contribution reaches the target.



_Appears in:_
- [ResourcePatchSpec](#resourcepatchspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[ApplyMode](#applymode)_ | Mode selects server-side or client-side apply. | ServerSideApply | Enum: [ServerSideApply ClientSideApply] <br /> |
| `fieldManager` _string_ | FieldManager overrides the derived field manager name. Defaults to<br />"patch-operator/<namespace>/<name>", hashed if that would exceed the API server's limit. |  |  |
| `conflictPolicy` _[ConflictPolicy](#conflictpolicy)_ | ConflictPolicy decides what happens when another manager owns a field this contributor<br />claims. | Fail | Enum: [Fail Priority Force] <br /> |


#### BaseReconcilePolicy

_Underlying type:_ _string_

BaseReconcilePolicy decides whether spec.base is re-asserted after creation.

_Validation:_
- Enum: [CreateOnly Enforce]

_Appears in:_
- [LifecycleSpec](#lifecyclespec)

| Field | Description |
| --- | --- |
| `CreateOnly` | BaseReconcileCreateOnly writes the base once, at creation, and never re-asserts it, so it<br />cannot claw back a field a contributor sets later.<br /> |
| `Enforce` | BaseReconcileEnforce continuously re-applies the base. Reserved; not implemented in v1alpha1.<br /> |


#### ClusterResourcePatch



ClusterResourcePatch contributes a slice of configuration to a shared object in any namespace,
or to a cluster-scoped object.


Identical spec to ResourcePatch — this is one API with two reaches, not two APIs. It is the
privileged kind: it is the only one that can cross a namespace boundary, so it should be a
platform-team grant rather than something handed to tenant Compositions.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `terasky.com/v1alpha1` | | |
| `kind` _string_ | `ClusterResourcePatch` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ResourcePatchSpec](#resourcepatchspec)_ |  |  |  |
| `status` _[ResourcePatchStatus](#resourcepatchstatus)_ |  |  |  |


#### ClusterSharedResource



ClusterSharedResource tracks every cluster-scoped target, and every namespaced target that has
at least one ClusterResourcePatch contributor.


It has no promotedTo: promotion is one-way and a ClusterSharedResource is never demoted.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `terasky.com/v1alpha1` | | |
| `kind` _string_ | `ClusterSharedResource` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SharedResourceSpec](#sharedresourcespec)_ |  |  |  |
| `status` _[SharedResourceStatus](#sharedresourcestatus)_ |  |  |  |


#### ConflictPolicy

_Underlying type:_ _string_

ConflictPolicy decides what happens when two contributors claim the same field path.

_Validation:_
- Enum: [Fail Priority Force]

_Appears in:_
- [ApplySpec](#applyspec)

| Field | Description |
| --- | --- |
| `Fail` | ConflictPolicyFail writes nothing and reports the conflict. Default.<br /> |
| `Priority` | ConflictPolicyPriority forces only against other patch-operator field managers, never<br />against a foreign controller.<br /> |
| `Force` | ConflictPolicyForce forces unconditionally. Last resort.<br /> |


#### ConflictStatus



ConflictStatus records one field-path conflict and who is party to it.



_Appears in:_
- [SharedResourceStatus](#sharedresourcestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `fieldPath` _string_ | FieldPath is the contested path. |  |  |
| `claimants` _string array_ | Claimants are the contributors claiming it, qualified by kind since a namespaced and a<br />cluster-scoped contributor can be party to the same conflict after a promotion. |  |  |
| `holder` _string_ | Holder is the contributor or foreign field manager that currently owns it. |  |  |




#### ContributorRef



ContributorRef identifies a contributor. Kind is carried because after a promotion one tracker
holds a mix of ResourcePatch and ClusterResourcePatch contributors.



_Appears in:_
- [ContributorStatus](#contributorstatus)
- [SharedResourceStatus](#sharedresourcestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kind` _string_ | Kind is ResourcePatch or ClusterResourcePatch. |  |  |
| `name` _string_ |  |  |  |
| `namespace` _string_ |  |  |  |
| `uid` _string_ | UID pins the identity, so a recreated contributor of the same name is not mistaken for the<br />original. The delete-safety rule in DESIGN.md 6.2 matches on this. |  |  |


#### ContributorState

_Underlying type:_ _string_

ContributorState is one contributor's state on a tracker.

_Validation:_
- Enum: [Applied Superseded Conflicted Releasing]

_Appears in:_
- [ContributorStatus](#contributorstatus)
- [TargetStatus](#targetstatus)

| Field | Description |
| --- | --- |
| `Applied` | ContributorStateApplied means this contributor's fields are on the target.<br /> |
| `Superseded` | ContributorStateSuperseded means a higher-priority contributor won a conflict, so this<br />contributor's fields are genuinely absent. Reported as Ready=False, not hidden.<br /> |
| `Conflicted` | ContributorStateConflicted means this contributor is party to an unresolved conflict.<br /> |
| `Releasing` | ContributorStateReleasing means this contributor's fields are being withdrawn.<br /> |


#### ContributorStatus



ContributorStatus is the tracker's record of one contributor.



_Appears in:_
- [SharedResourceStatus](#sharedresourcestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `patchRef` _[ContributorRef](#contributorref)_ | PatchRef identifies the contributor. |  |  |
| `observedGeneration` _integer_ | ObservedGeneration is the contributor spec generation this record reflects. |  |  |
| `priority` _integer_ | Priority is copied from the contributor for deterministic ordering without a second read. |  |  |
| `fieldManager` _string_ | FieldManager is the SSA field manager this contributor owns its fields under. |  |  |
| `state` _[ContributorState](#contributorstate)_ | State is this contributor's state on the target. |  | Enum: [Applied Superseded Conflicted Releasing] <br /> |
| `lastAppliedHash` _string_ | LastAppliedHash lets the tracker skip an apply when nothing has changed. |  |  |
| `ownedPaths` _string array_ | OwnedPaths are the field paths this contributor set. ClientSideApply bookkeeping. |  |  |
| `priorValues` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#rawextension-runtime-pkg)_ | PriorValues holds what those paths contained before this contributor first touched them,<br />for paths that already existed. ClientSideApply revert restores these rather than deleting<br />the field, which is the correct behaviour when a contributor overwrote a pre-existing<br />setting. Losing this on promotion would silently break revert. |  |  |
| `creationTimestamp` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#time-v1-meta)_ | CreationTimestamp is copied for the deterministic sort, which orders by<br />(priority desc, creationTimestamp asc, uid asc). |  |  |


#### LifecycleSpec



LifecycleSpec declares create and release behaviour.



_Appears in:_
- [ResourcePatchSpec](#resourcepatchspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `onMissing` _[OnMissingPolicy](#onmissingpolicy)_ | OnMissing decides what happens when the target does not exist. | Wait | Enum: [Fail Wait Create] <br /> |
| `onRelease` _[OnReleasePolicy](#onreleasepolicy)_ | OnRelease decides what happens to this contributor's fields when it is deleted. | Revert | Enum: [Revert Orphan Delete] <br /> |
| `adoptExisting` _boolean_ | AdoptExisting allows OnMissing=Create to adopt an object that already exists rather than<br />failing on the create. | true |  |
| `baseReconcile` _[BaseReconcilePolicy](#basereconcilepolicy)_ | BaseReconcile decides whether spec.base is re-asserted after creation. | CreateOnly | Enum: [CreateOnly Enforce] <br /> |


#### MergeKey



MergeKey declares the identifying key for a list field, supplying the granularity a target's
schema does not declare. Used under ClientSideApply.



_Appears in:_
- [PatchSpec](#patchspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `path` _string_ | Path to the list field, in dotted notation, e.g. "spec.rules". |  | MinLength: 1 <br /> |
| `key` _string_ | Key is the field within each list element that identifies it, e.g. "host". |  | MinLength: 1 <br /> |


#### OnMissingPolicy

_Underlying type:_ _string_

OnMissingPolicy decides what happens when the target does not exist.

_Validation:_
- Enum: [Fail Wait Create]

_Appears in:_
- [LifecycleSpec](#lifecyclespec)

| Field | Description |
| --- | --- |
| `Fail` | OnMissingFail reports an error when the target is absent.<br /> |
| `Wait` | OnMissingWait parks until some other contributor creates the target.<br /> |
| `Create` | OnMissingCreate creates the target from spec.base. Invalid in Selector mode.<br /> |


#### OnReleasePolicy

_Underlying type:_ _string_

OnReleasePolicy decides what happens to this contributor's fields when it is deleted.

_Validation:_
- Enum: [Revert Orphan Delete]

_Appears in:_
- [LifecycleSpec](#lifecyclespec)

| Field | Description |
| --- | --- |
| `Revert` | OnReleaseRevert withdraws this contributor's fields, restoring any prior values.<br /> |
| `Orphan` | OnReleaseOrphan leaves this contributor's fields in place.<br /> |
| `Delete` | OnReleaseDelete deletes the whole target once the last contributor releases, but only<br />if this contributor created it. See DESIGN.md 6.2.<br /> |


#### PatchSpec



PatchSpec is what this contributor adds to the target.



_Appears in:_
- [ResourcePatchSpec](#resourcepatchspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `type` _[PatchType](#patchtype)_ | Type selects how Value or Ops is interpreted. | StrategicMerge | Enum: [StrategicMerge Merge JSON6902] <br /> |
| `value` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#rawextension-runtime-pkg)_ | Value is the partial object to merge, for StrategicMerge and Merge types. |  |  |
| `ops` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#rawextension-runtime-pkg)_ | Ops is the list of RFC 6902 operations, for the JSON6902 type. |  |  |
| `mergeKeys` _[MergeKey](#mergekey) array_ | MergeKeys declares identifying keys for list fields under ClientSideApply. |  |  |


#### PatchType

_Underlying type:_ _string_

PatchType selects how spec.patch is interpreted.

_Validation:_
- Enum: [StrategicMerge Merge JSON6902]

_Appears in:_
- [PatchSpec](#patchspec)

| Field | Description |
| --- | --- |
| `StrategicMerge` | PatchTypeStrategicMerge is a strategic merge patch, using mergeKeys under ClientSideApply.<br /> |
| `Merge` | PatchTypeMerge is an RFC 7386 JSON merge patch.<br /> |
| `JSON6902` | PatchTypeJSON6902 is a list of RFC 6902 JSON patch operations.<br /> |


#### PromotionRef



PromotionRef points at the ClusterSharedResource that took over from a namespaced tracker.



_Appears in:_
- [SharedResourceStatus](#sharedresourcestatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ |  |  |  |
| `adopted` _boolean_ | Adopted is set once the ClusterSharedResource has confirmed it holds the copied state. The<br />namespaced tracker's finalizer is released only after this. |  |  |


#### ResourcePatch



ResourcePatch contributes a slice of configuration to a shared object in its own namespace.


A ResourcePatch can only ever touch objects in its own namespace. That is structural, not a
policy check: the target namespace is forced to the ResourcePatch's namespace at admission and
re-checked at reconcile, and no field expresses anything else. The property therefore holds
regardless of how privileged the principal creating it is — which matters because a patch
emitted by Crossplane or kro is admitted as the orchestrator's near-cluster-admin ServiceAccount.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `terasky.com/v1alpha1` | | |
| `kind` _string_ | `ResourcePatch` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[ResourcePatchSpec](#resourcepatchspec)_ |  |  |  |
| `status` _[ResourcePatchStatus](#resourcepatchstatus)_ |  |  |  |


#### ResourcePatchSpec



ResourcePatchSpec is the spec shared by ResourcePatch and ClusterResourcePatch. The two kinds
differ only in what Target may reach, not in shape.



_Appears in:_
- [ClusterResourcePatch](#clusterresourcepatch)
- [ResourcePatch](#resourcepatch)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `target` _[TargetRef](#targetref)_ | Target identifies the object(s) this contributor contributes to. |  |  |
| `lifecycle` _[LifecycleSpec](#lifecyclespec)_ | Lifecycle declares create and release behaviour. |  |  |
| `apply` _[ApplySpec](#applyspec)_ | Apply declares how the contribution reaches the target. |  |  |
| `priority` _integer_ | Priority orders contributors deterministically and breaks conflicts under<br />ConflictPolicy=Priority. Higher wins. | 100 |  |
| `serviceAccountRef` _[ServiceAccountRef](#serviceaccountref)_ | ServiceAccountRef makes the operator impersonate a ServiceAccount for every target write,<br />so the API server enforces that identity's RBAC continuously rather than once at admission. |  |  |
| `base` _[RawExtension](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#rawextension-runtime-pkg)_ | Base is the seed manifest, consulted only when Lifecycle.OnMissing is Create. Under the<br />default BaseReconcile=CreateOnly it is written once and never re-asserted. |  |  |
| `patch` _[PatchSpec](#patchspec)_ | Patch is what this contributor always contributes. |  |  |


#### ResourcePatchStatus



ResourcePatchStatus is the status shared by ResourcePatch and ClusterResourcePatch.



_Appears in:_
- [ClusterResourcePatch](#clusterresourcepatch)
- [ResourcePatch](#resourcepatch)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#condition-v1-meta) array_ | Conditions follow the Crossplane condition shape so an XR readiness check or a kro status<br />roll-up consumes them without an adapter. |  |  |
| `observedTargets` _[TargetStatus](#targetstatus) array_ | ObservedTargets lists the targets this contributor currently resolves to. |  |  |
| `sharedResourceRefs` _[TrackerRef](#trackerref) array_ | SharedResourceRefs points at the trackers that own those targets. |  |  |
| `appliedGeneration` _integer_ | AppliedGeneration is the spec generation last successfully applied to every target. |  |  |
| `authorizedAs` _string_ | AuthorizedAs records the principal the admission webhook validated this contributor for, so<br />the controller can re-run its SubjectAccessReview on a TTL. Admission is point-in-time;<br />this closes the revoked-RBAC gap. |  |  |
| `lastAuthorizedTime` _[Time](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#time-v1-meta)_ | LastAuthorizedTime is when the SubjectAccessReview last succeeded. |  |  |


#### ServiceAccountRef



ServiceAccountRef names a ServiceAccount the operator impersonates for target writes.



_Appears in:_
- [ResourcePatchSpec](#resourcepatchspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `name` _string_ | Name of the ServiceAccount. |  | MinLength: 1 <br /> |
| `namespace` _string_ | Namespace of the ServiceAccount. Only settable on ClusterResourcePatch, and gated by an<br />"impersonate" SubjectAccessReview against the requesting principal. On ResourcePatch the<br />ServiceAccount is always resolved in the ResourcePatch's own namespace. |  |  |


#### SharedResource



SharedResource tracks one namespaced target whose contributors all live in this namespace.


Operator-owned: users do not create these. It is the only writer to its target, which is what
makes writes serialised, apply order deterministic, and the reference count evaluable at a
single point.





| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ | `terasky.com/v1alpha1` | | |
| `kind` _string_ | `SharedResource` | | |
| `metadata` _[ObjectMeta](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#objectmeta-v1-meta)_ | Refer to Kubernetes API documentation for fields of `metadata`. |  |  |
| `spec` _[SharedResourceSpec](#sharedresourcespec)_ |  |  |  |
| `status` _[SharedResourceStatus](#sharedresourcestatus)_ |  |  |  |


#### SharedResourceSpec



SharedResourceSpec identifies the target a tracker owns.



_Appears in:_
- [ClusterSharedResource](#clustersharedresource)
- [SharedResource](#sharedresource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `targetRef` _[TrackerTargetRef](#trackertargetref)_ | TargetRef identifies the target object. |  |  |


#### SharedResourceStatus



SharedResourceStatus is the status shared by SharedResource and ClusterSharedResource.



_Appears in:_
- [ClusterSharedResource](#clustersharedresource)
- [SharedResource](#sharedresource)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `phase` _[TrackerPhase](#trackerphase)_ | Phase is the tracker's overall state. |  | Enum: [Waiting Applied Conflicted Releasing Promoting] <br /> |
| `conditions` _[Condition](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#condition-v1-meta) array_ | Conditions follow the same shape as contributors'. |  |  |
| `promotedTo` _[PromotionRef](#promotionref)_ | PromotedTo fences this tracker and names its successor. Only ever set on a namespaced<br />SharedResource; promotion is one-way and a ClusterSharedResource is never demoted. |  |  |
| `createdByOperator` _boolean_ | CreatedByOperator records that this operator created the target, which is half of the<br />precondition for ever deleting it. |  |  |
| `creatorPatchRef` _[ContributorRef](#contributorref)_ | CreatorPatchRef is the contributor that actually created the target. Only it may request<br />deletion. Losing this on promotion would silently break that guarantee. |  |  |
| `observedBaseHash` _string_ | ObservedBaseHash is the hash of the base the target was created from, used to detect a<br />second contributor supplying a different base. |  |  |
| `observedTargetUID` _string_ | ObservedTargetUID pins the target's identity. A change means the target was replaced out of<br />band, which invalidates all ClientSideApply bookkeeping. |  |  |
| `observedResourceVersion` _string_ | ObservedResourceVersion is the target's resourceVersion after the last successful write,<br />used to recognise and drop the echo of that write. |  |  |
| `contributors` _[ContributorStatus](#contributorstatus) array_ | Contributors is the reference count. It is derived state, rebuilt from a live list on every<br />reconcile, so a missed event self-heals rather than corrupting the count. |  |  |
| `contributorCount` _integer_ | ContributorCount is len(Contributors), surfaced as its own field only so it can be a printer<br />column: "who is writing to this object?" should be answerable in one command. |  |  |
| `conflicts` _[ConflictStatus](#conflictstatus) array_ | Conflicts records unresolved field-path conflicts. |  |  |


#### TargetMode

_Underlying type:_ _string_

TargetMode selects how a contributor identifies the object(s) it contributes to.

_Validation:_
- Enum: [Single Selector]

_Appears in:_
- [TargetRef](#targetref)

| Field | Description |
| --- | --- |
| `Single` | TargetModeSingle resolves to exactly one object, whether or not it currently exists.<br />This is the only mode that supports create-on-missing and reference-counted deletion.<br /> |
| `Selector` | TargetModeSelector resolves to zero or more existing objects and is patch-only.<br /> |


#### TargetRef



TargetRef identifies the object(s) a contributor contributes to.



_Appears in:_
- [ResourcePatchSpec](#resourcepatchspec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `mode` _[TargetMode](#targetmode)_ | Mode selects single-object or selector-based targeting. | Single | Enum: [Single Selector] <br /> |
| `apiVersion` _string_ | APIVersion of the target, e.g. "networking.k8s.io/v1". |  | MinLength: 1 <br /> |
| `kind` _string_ | Kind of the target, e.g. "Ingress". |  | MinLength: 1 <br /> |
| `name` _string_ | Name of the target. Required in Single mode, forbidden in Selector mode. |  |  |
| `namespace` _string_ | Namespace of the target.<br /><br />On a ResourcePatch this is optional and defaults to the ResourcePatch's own namespace; any<br />other value is rejected. On a ClusterResourcePatch it is required for namespaced kinds and<br />forbidden for cluster-scoped ones. |  |  |
| `selector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#labelselector-v1-meta)_ | Selector matches targets by label in Selector mode. |  |  |
| `namespaceSelector` _[LabelSelector](https://kubernetes.io/docs/reference/generated/kubernetes-api/v1.34/#labelselector-v1-meta)_ | NamespaceSelector restricts Selector mode to matching namespaces. Only valid on<br />ClusterResourcePatch; forbidden on ResourcePatch, which is confined to its own namespace. |  |  |
| `maxTargets` _integer_ | MaxTargets caps how many objects a Selector-mode contributor may resolve to. Exceeding it<br />fails the contributor rather than silently truncating the match set. | 100 | Minimum: 1 <br /> |


#### TargetStatus



TargetStatus reports the state of one resolved target.



_Appears in:_
- [ResourcePatchStatus](#resourcepatchstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ |  |  |  |
| `kind` _string_ |  |  |  |
| `name` _string_ |  |  |  |
| `namespace` _string_ |  |  |  |
| `uid` _string_ |  |  |  |
| `state` _[ContributorState](#contributorstate)_ | State mirrors this contributor's ContributorState on the tracker for this target. |  | Enum: [Applied Superseded Conflicted Releasing] <br /> |




#### TrackerPhase

_Underlying type:_ _string_

TrackerPhase is the tracker's overall state.

_Validation:_
- Enum: [Waiting Applied Conflicted Releasing Promoting]

_Appears in:_
- [SharedResourceStatus](#sharedresourcestatus)

| Field | Description |
| --- | --- |
| `Waiting` | TrackerPhaseWaiting means the target does not exist and no contributor will create it.<br /> |
| `Applied` | TrackerPhaseApplied means every contributor's contribution is on the target.<br /> |
| `Conflicted` | TrackerPhaseConflicted means at least one field conflict is unresolved.<br /> |
| `Releasing` | TrackerPhaseReleasing means at least one contributor is being withdrawn.<br /> |
| `Promoting` | TrackerPhasePromoting fences a namespaced tracker: it writes nothing to the target while a<br />ClusterSharedResource takes over. See DESIGN.md 6.3.<br /> |


#### TrackerRef



TrackerRef points at the tracker that owns a target this contributor contributes to.



_Appears in:_
- [ResourcePatchStatus](#resourcepatchstatus)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `kind` _string_ | Kind is SharedResource or ClusterSharedResource. |  |  |
| `name` _string_ |  |  |  |
| `namespace` _string_ |  |  |  |


#### TrackerTargetRef



TrackerTargetRef is the concrete, fully resolved target of a tracker.



_Appears in:_
- [SharedResourceSpec](#sharedresourcespec)

| Field | Description | Default | Validation |
| --- | --- | --- | --- |
| `apiVersion` _string_ |  |  |  |
| `kind` _string_ |  |  |  |
| `name` _string_ |  |  |  |
| `namespace` _string_ | Namespace is empty for a cluster-scoped target. On a namespaced SharedResource it is<br />implicit in the tracker's own namespace but recorded here too, so the reference is complete<br />when copied to a ClusterSharedResource during promotion. |  |  |


