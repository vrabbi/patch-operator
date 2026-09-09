# With Crossplane

The XR owns the contributor via the ordinary composed-resource `ownerReference`. That is the whole
integration: deletion propagates *in* from Crossplane by normal garbage collection, and the operator
arbitrates what that means for the shared target.

```
XR --ownerRef--> ResourcePatch --tracked by--> SharedResource --manages--> target
```

**Readiness needs no adapter.** Contributors publish a Crossplane-shaped `Ready` condition, so
Crossplane's default readiness check gates the XR on it directly. `Ready=True` means the
contribution is applied to every resolved target **and** the contributor is not party to an
unresolved conflict — so an XR never reports ready while its ingress rule is stuck behind a field
conflict.

## Which kind should a Composition emit?

!!! tip "Default to `ResourcePatch`"
    The principal admitting a composed patch is **Crossplane's** ServiceAccount, not the user who
    created the XR, and it is typically close to cluster-admin. So SubjectAccessReview does little
    to bound a Composition author.

    A namespaced `ResourcePatch` does not care: it cannot reach outside its namespace whatever the
    principal can do. Emit `ClusterResourcePatch` only when the shared target genuinely lives in
    another namespace — and withhold that grant from Crossplane's ServiceAccount unless you mean it.
    See [Security](../security.md).

## Contained: a per-tenant shared Ingress

Every `XApp` in a namespace contributes one rule to that namespace's shared Ingress.

```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: app-with-namespaced-shared-ingress
spec:
  compositeTypeRef:
    apiVersion: platform.terasky.com/v1alpha1
    kind: XApp
  mode: Pipeline
  pipeline:
    - step: patch-and-transform
      functionRef:
        name: function-patch-and-transform
      input:
        apiVersion: pt.fn.crossplane.io/v1beta1
        kind: Resources
        resources:
          - name: shared-ingress-contribution
            base:
              apiVersion: terasky.com/v1alpha1
              kind: ResourcePatch
              spec:
                target:
                  mode: Single
                  apiVersion: networking.k8s.io/v1
                  kind: Ingress
                  name: shared-ingress
                  # No target.namespace: it defaults to this ResourcePatch's own
                  # namespace, and nothing else would be accepted.
                lifecycle:
                  onMissing: Create
                  onRelease: Revert
                  adoptExisting: true
                apply:
                  mode: ClientSideApply    # spec.rules is atomic
                  conflictPolicy: Fail
                priority: 100
                base:
                  apiVersion: networking.k8s.io/v1
                  kind: Ingress
                  metadata:
                    name: shared-ingress
                  spec:
                    ingressClassName: nginx
                patch:
                  type: StrategicMerge
                  mergeKeys:
                    - path: spec.rules
                      key: host
                  value:
                    spec:
                      rules:
                        - host: placeholder
                          http:
                            paths:
                              - path: /
                                pathType: Prefix
                                backend:
                                  service:
                                    name: placeholder
                                    port:
                                      number: 80
            patches:
              # Place the contributor in the XR's namespace, so target, tracker and
              # contributors all live together.
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.namespace
                toFieldPath: metadata.namespace
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.namespace
                toFieldPath: spec.base.metadata.namespace
              # Each XApp contributes exactly one rule, keyed by its own hostname.
              - type: FromCompositeFieldPath
                fromFieldPath: spec.hostname
                toFieldPath: spec.patch.value.spec.rules[0].host
              - type: FromCompositeFieldPath
                fromFieldPath: spec.serviceName
                toFieldPath: spec.patch.value.spec.rules[0].http.paths[0].backend.service.name
```

## Privileged: a shared platform Ingress

When the target genuinely lives elsewhere. This needs `create` on `clusterresourcepatches` for
Crossplane's ServiceAccount — the grant that lets a Composition author reach across namespaces.

```yaml
          - name: platform-ingress-contribution
            base:
              apiVersion: terasky.com/v1alpha1
              kind: ClusterResourcePatch
              spec:
                target:
                  mode: Single
                  apiVersion: networking.k8s.io/v1
                  kind: Ingress
                  name: shared-ingress
                  namespace: platform
                lifecycle:
                  onMissing: Wait          # the platform team creates it
                  onRelease: Revert
                apply:
                  mode: ClientSideApply
                  conflictPolicy: Fail
                patch:
                  type: StrategicMerge
                  mergeKeys:
                    - path: spec.rules
                      key: host
                  value:
                    spec:
                      rules:
                        - host: placeholder
            patches:
              - type: FromCompositeFieldPath
                fromFieldPath: spec.hostname
                toFieldPath: spec.patch.value.spec.rules[0].host
              # Impersonate the tenant, so the write carries the tenant's RBAC rather
              # than Crossplane's. This is the real boundary here -- set it whenever a
              # Composition emits ClusterResourcePatch.
              - type: FromCompositeFieldPath
                fromFieldPath: metadata.namespace
                toFieldPath: spec.serviceAccountRef.namespace
              - type: FromCompositeFieldPath
                fromFieldPath: spec.tenant
                toFieldPath: spec.serviceAccountRef.name
                transforms:
                  - type: string
                    string:
                      type: Format
                      fmt: "%s-patcher"
```

## Relationship to `provider-kubernetes`

They are complements, not alternatives. Keep using `Object` for resources an XR **solely owns**;
use a contributor for the slice of a **shared** object it contributes.

Both use SSA field managers, so they coexist when their field sets are disjoint.
`conflictPolicy: Priority` deliberately will **not** force against a foreign manager, so `Object`'s
unconditional forcing wins any genuine overlap and the contributor reports `Conflict` rather than
flapping. Pick one owner per field.
