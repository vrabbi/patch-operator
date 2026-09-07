# With kro

The Instance owns the contributor via `ownerReferences`, so deleting the Instance withdraws only
that Instance's fields from the shared object.

**This is the write-side complement to kro's `externalRef`.** `externalRef` lets an RGD *read* a
shared resource but never create, update or delete it; a contributor lets an Instance *contribute*
to one while the operator reference-counts the contributors. The two compose: `externalRef` to read
fields off the shared object, a contributor to write your slice of it.

## Contained: the recommended default

The contributor lands in the Instance's namespace and cannot reach outside it, whatever kro's
ServiceAccount is permitted to do.

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: app-with-namespaced-shared-ingress
spec:
  schema:
    apiVersion: v1alpha1
    kind: App
    spec:
      name: string
      tenant: string
      hostname: string
      port: integer | default=80
    status:
      ingressReady: ${ingressContribution.status.conditions[?(@.type=="Ready")].status}

  resources:
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.name}
        spec:
          selector:
            app: ${schema.spec.name}
          ports:
            - port: ${schema.spec.port}

    - id: ingressContribution
      # Gate downstream resources on the contribution actually landing. Ready is False
      # while a field conflict is unresolved, so nothing downstream proceeds on a
      # half-applied shared object.
      readyWhen:
        - ${ingressContribution.status.conditions.exists(c, c.type == "Ready" && c.status == "True")}
      template:
        apiVersion: terasky.com/v1alpha1
        kind: ResourcePatch
        metadata:
          name: ${schema.spec.name}-ingress-rule
          # namespace is inherited from the Instance. The target resolves in this same
          # namespace, and no other value would be accepted.
        spec:
          target:
            mode: Single
            apiVersion: networking.k8s.io/v1
            kind: Ingress
            name: shared-ingress
          lifecycle:
            onMissing: Create
            onRelease: Revert
            adoptExisting: true
          apply:
            mode: ClientSideApply
            conflictPolicy: Fail
          priority: 100
          serviceAccountRef:
            # Namespaced kind: resolved in this ResourcePatch's own namespace, and the
            # field has no `namespace` sub-field here.
            name: ${schema.spec.tenant}-patcher
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
                  - host: ${schema.spec.hostname}
                    http:
                      paths:
                        - path: /
                          pathType: Prefix
                          backend:
                            service:
                              name: ${schema.spec.name}
                              port:
                                number: ${schema.spec.port}
```

## Reading and writing the same shared object

`externalRef` and a contributor compose cleanly. Read the shared object's current state, contribute
your slice, and reference both:

```yaml
  resources:
    # Read-only: kro never writes this.
    - id: sharedIngress
      externalRef:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        metadata:
          name: shared-ingress

    # Write: the operator reference-counts this contribution.
    - id: contribution
      template:
        apiVersion: terasky.com/v1alpha1
        kind: ResourcePatch
        # ... as above

    - id: status
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.spec.name}-ingress-status
        data:
          ingressClass: ${sharedIngress.spec.ingressClassName}
```

## Privileged: a cross-namespace target

A cross-namespace target requires the cluster kind, which requires granting kro's ServiceAccount
`create` on `clusterresourcepatches`. Withhold that grant unless a shared platform object genuinely
needs per-Instance contributions — and set `serviceAccountRef` when you do.

```yaml
    - id: platformContribution
      readyWhen:
        - ${platformContribution.status.conditions.exists(c, c.type == "Ready" && c.status == "True")}
      template:
        apiVersion: terasky.com/v1alpha1
        kind: ClusterResourcePatch
        metadata:
          name: ${schema.spec.name}-platform-ingress-rule
        spec:
          target:
            mode: Single
            apiVersion: networking.k8s.io/v1
            kind: Ingress
            name: shared-ingress
            namespace: platform
          lifecycle:
            onMissing: Wait
            onRelease: Revert
          apply:
            mode: ClientSideApply
            conflictPolicy: Fail
          serviceAccountRef:
            name: ${schema.spec.tenant}-patcher
            namespace: ${schema.spec.tenantNamespace}
          patch:
            type: StrategicMerge
            mergeKeys:
              - path: spec.rules
                key: host
            value:
              spec:
                rules:
                  - host: ${schema.spec.hostname}
```
