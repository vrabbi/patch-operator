# Install

## Requirements

- Kubernetes 1.30+
- [cert-manager](https://cert-manager.io/) — issues the webhook's serving certificate

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=5m
```

## Full install

Installs all four CRDs, the operator, and the admission webhooks -- a
`ValidatingWebhookConfiguration` holding the authorization boundary and a small
`MutatingWebhookConfiguration` that records the admitting principal (see
[Security](../security.md#re-checking-after-admission) for why that has to be a second webhook).

```bash
kubectl apply -f https://github.com/vrabbi/patch-operator/releases/latest/download/install.yaml
kubectl -n patch-operator-system rollout status deploy/patch-operator-controller-manager
```

The released manifest pins the image **by digest**, so it keeps deploying the bytes that release
tested and signed even if a tag is later moved.

## Namespace-only install

**Prefer this when no shared object is ever contributed to from outside its own namespace.**

It omits the cluster-scoped CRDs entirely, so cross-namespace contribution is not merely denied but
**absent** — there is no API to express it — and the operator needs no cluster-wide write RBAC.

```bash
kubectl apply -f https://github.com/vrabbi/patch-operator/releases/latest/download/install-namespaced-only.yaml
```

You get `ResourcePatch` and `SharedResource` only. The strongest control in the design — that a
`ResourcePatch` cannot reach outside its namespace — then holds by construction rather than by
policy. See [Security](../security.md).

## Install from a checkout

To deploy a build of your own, or to change the kustomize overlay before applying it:

```bash
git clone https://github.com/vrabbi/patch-operator
cd patch-operator
make deploy IMG=ghcr.io/vrabbi/patch-operator:latest              # or deploy-namespaced-only
```

`make build-installer IMG=...` renders both overlays into `dist/` without applying them, which is
what the release workflow does.

## Verifying a release

Every release image is signed keylessly with [cosign](https://github.com/sigstore/cosign) using the
release workflow's GitHub OIDC identity, and carries build-provenance and SBOM attestations. There
is no long-lived signing key to trust or rotate — verification checks *which workflow in which
repository* produced the artifact.

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/vrabbi/patch-operator/\.github/workflows/release\.yml@refs/tags/.*$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/vrabbi/patch-operator@sha256:<digest>

gh attestation verify oci://ghcr.io/vrabbi/patch-operator@sha256:<digest> --repo vrabbi/patch-operator
```

The install manifests are covered by a cosign-signed `checksums.txt` on the same release. Each
release body carries the digest and the exact commands.

A running operator reports its build:

```bash
kubectl -n patch-operator-system logs deploy/patch-operator-controller-manager | head -1
```

## Granting access

The grant asymmetry is the point of the scope split, so it is worth getting right.

**`ResourcePatch` is safe to grant broadly**, including to Crossplane's and kro's ServiceAccounts.
The blast radius is one namespace, whatever a Composition author writes.

```bash
# Let Crossplane compose ResourcePatches in a tenant namespace
kubectl create rolebinding crossplane-patches \
  --clusterrole=resourcepatch-editor \
  --serviceaccount=crossplane-system:crossplane \
  -n team-a

# Let the tenant manage their own
kubectl create rolebinding team-a-patches \
  --clusterrole=resourcepatch-editor \
  --group=team-a-developers \
  -n team-a

# Read-only
kubectl create rolebinding team-a-patch-viewers \
  --clusterrole=resourcepatch-viewer \
  --group=team-a-readers \
  -n team-a
```

**`ClusterResourcePatch` is the privileged counterpart.** It is the only kind that can cross a
namespace boundary or touch a cluster-scoped object.

```bash
# Platform team only. Withholding this from the orchestrators is what makes the
# containment above a real boundary rather than a convention.
kubectl create clusterrolebinding platform-cluster-patches \
  --clusterrole=clusterresourcepatch-editor \
  --group=platform-engineering
```

## Verify

```bash
kubectl get crd | grep terasky.com
# clusterresourcepatches.terasky.com   Cluster
# clustersharedresources.terasky.com   Cluster
# resourcepatches.terasky.com          Namespaced
# sharedresources.terasky.com          Namespaced

kubectl -n patch-operator-system get pods
```

A quick smoke test:

```bash
kubectl create namespace demo
cat <<'EOF' | kubectl apply -f -
apiVersion: terasky.com/v1alpha1
kind: ResourcePatch
metadata:
  name: demo-a
  namespace: demo
spec:
  target:
    mode: Single
    apiVersion: v1
    kind: ConfigMap
    name: shared
  lifecycle:
    onMissing: Create
    onRelease: Revert
  base:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: shared
  patch:
    value:
      data:
        from-a: "1"
EOF

kubectl -n demo get configmap shared -o jsonpath='{.data}'    # {"from-a":"1"}
kubectl -n demo get resourcepatch,sharedresource
kubectl -n demo delete resourcepatch demo-a
kubectl -n demo get configmap shared -o jsonpath='{.data}'    # {} -- reverted
```

## Configuration

| Flag | Default | Purpose |
|---|---|---|
| `--leader-elect` | `false` | **Enable in HA.** Two writers on one target is exactly what this operator exists to prevent. |
| `--reauthorize-after` | `10m` | How often to re-run the SubjectAccessReview recorded at admission. Zero disables it. |
| `--max-watched-target-kinds` | `50` | Cap on distinct target kinds watched. Kinds beyond it converge on the requeue interval rather than on events. |
| `--metrics-bind-address` | `0` | `:8443` for HTTPS metrics. |
| `--disable-webhooks` | `false` | **Local development only** — it removes the SubjectAccessReview boundary. |

## Availability

The webhook uses `failurePolicy: Fail`, which is the correct trade for an authorization check and
makes webhook availability a **hard dependency**: while it is down, contributors cannot be created
and any XR composing one blocks.

The shipped Deployment runs **two replicas** for that reason. Leader election means only one
reconciles, so the second is there for the webhook and for failover, not for throughput.

## Uninstall

```bash
# Withdraw contributions first, so nothing is left orphaned on shared objects.
kubectl delete resourcepatches --all -A
kubectl delete clusterresourcepatches --all
make undeploy
```

!!! warning "Delete contributors before the operator"
    Contributor finalizers are released by the operator. Removing the operator first leaves them
    wedged in `Terminating`, and their fields on shared objects with nothing left to withdraw them.
