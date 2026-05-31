# Tailgate Operator Helm Chart

A Kubernetes operator that gives pods native [Tailscale](https://tailscale.com/) egress.
Each `EgressGroup` provisions one shared `tailscaled` gateway, and a per-node agent
veth-stitches the group's member pods into the gateway's network namespace so their
egress traffic leaves through the tailnet.

## Prerequisites

- Kubernetes 1.25+
- Helm 3.8+
- A Tailscale tailnet with an OAuth client (`devices:write` / auth-key scope) so the
  operator can mint ephemeral authkeys for the gateways it creates
- Nodes that allow privileged pods (the agent runs privileged with `hostPID` +
  `hostNetwork`)

## Installation

### From OCI Registry (GHCR)

```bash
# Install the latest version
helm install tailgate-operator oci://ghcr.io/rajsinghtech/charts/tailgate-operator \
  --namespace tailgate-system \
  --create-namespace \
  --set tailnet.tailnet=example.com \
  --set tailnet.oauthClientID=<client-id> \
  --set tailnet.oauthClientSecret=<client-secret>

# Install a specific version
helm install tailgate-operator oci://ghcr.io/rajsinghtech/charts/tailgate-operator \
  --version <chart-version> \
  --namespace tailgate-system \
  --create-namespace
```

### From Local Chart

```bash
git clone https://github.com/rajsinghtech/tailgate.git
cd tailgate

helm install tailgate-operator charts/tailgate-operator \
  --namespace tailgate-system \
  --create-namespace
```

## Configuration

See [values.yaml](values.yaml) for the full list of configurable parameters.

### Images

| Parameter | Description | Default |
|-----------|-------------|---------|
| `operator.image.repository` | Operator image repository | `ghcr.io/rajsinghtech/tailgate-operator` |
| `operator.image.tag` | Operator image tag (defaults to chart appVersion) | `""` |
| `operator.image.pullPolicy` | Operator image pull policy | `IfNotPresent` |
| `agent.image.repository` | Agent image repository | `ghcr.io/rajsinghtech/tailgate-agent` |
| `agent.image.tag` | Agent image tag (defaults to chart appVersion) | `""` |
| `gateway.image.repository` | Per-group gateway image stamped into gateway workloads | `ghcr.io/rajsinghtech/tailgate-gateway` |
| `gateway.image.tag` | Gateway image tag (defaults to chart appVersion) | `""` |
| `imagePullSecrets` | Image pull secrets for private registries | `[]` |

### Operator

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of operator replicas | `1` |
| `operator.resources.limits.cpu` | CPU limit | `500m` |
| `operator.resources.limits.memory` | Memory limit | `256Mi` |
| `operator.resources.requests.cpu` | CPU request | `10m` |
| `operator.resources.requests.memory` | Memory request | `128Mi` |

### Agent

| Parameter | Description | Default |
|-----------|-------------|---------|
| `agent.enabled` | Deploy the per-node agent DaemonSet | `true` |
| `agent.clusterCIDRs` | In-cluster CIDRs kept on the primary CNI for exit-node members (comma-separated) | `10.96.0.0/12` |
| `agent.tolerations` | Agent pod tolerations | `[{operator: Exists}]` |

### Tailnet credentials

| Parameter | Description | Default |
|-----------|-------------|---------|
| `tailnet.create` | Create the credentials Secret from chart values | `true` |
| `tailnet.secretName` | Name of the credentials Secret | `tailgate-tailnet-creds` |
| `tailnet.tailnet` | Tailnet name (key `TS_TAILNET`) | `""` |
| `tailnet.oauthClientID` | OAuth client ID (key `TS_OAUTH_CLIENT_ID`) | `""` |
| `tailnet.oauthClientSecret` | OAuth client secret (key `TS_OAUTH_CLIENT_SECRET`) | `""` |

To reference an existing Secret instead of creating one, set `tailnet.create=false`
and point `tailnet.secretName` at a Secret containing `TS_TAILNET`,
`TS_OAUTH_CLIENT_ID` and `TS_OAUTH_CLIENT_SECRET`.

### CRDs & RBAC

| Parameter | Description | Default |
|-----------|-------------|---------|
| `crds.install` | Install the EgressGroup CRD with Helm | `true` |
| `crds.keep` | Keep CRDs on chart uninstall | `true` |
| `serviceAccount.create` | Create the ServiceAccount | `true` |
| `rbac.create` | Create the ClusterRole + ClusterRoleBinding | `true` |
| `extraObjects` | Extra templated Kubernetes objects to render with the chart | `[]` |

## Usage

After installation, create an `EgressGroup` that selects the pods you want to give
tailnet egress:

```yaml
apiVersion: tailscale.rajsingh.info/v1alpha1
kind: EgressGroup
metadata:
  name: my-egress
spec:
  # the minimal group: reach tailnet peers (CGNAT 100.64.0.0/10 + the IPv6 ULA).
  selector:
    podSelector:
      matchLabels:
        tailgate.rajsingh.info/egress: my-egress
```

Route a set of tailnet CIDRs through the gateway, optionally pinning an exit node:

```yaml
apiVersion: tailscale.rajsingh.info/v1alpha1
kind: EgressGroup
metadata:
  name: prod-egress
spec:
  routes:                  # non-empty routes ⇒ subnet reach (there is no "mode" field)
    - 10.20.0.0/16
  exitNode:
    nodeID: us-exit-1
    allowLANAccess: true
  dns:
    enabled: true          # give members native tailnet DNS
  selector:
    namespaceSelector:
      matchLabels:
        team: payments
```

Inspect groups:

```bash
kubectl get egressgroups
kubectl describe eg my-egress
```

## Upgrading

```bash
helm upgrade tailgate-operator oci://ghcr.io/rajsinghtech/charts/tailgate-operator \
  --namespace tailgate-system
```

## Uninstalling

```bash
helm uninstall tailgate-operator --namespace tailgate-system
```

**Note:** By default, CRDs are kept after uninstall. To remove the CRD:

```bash
kubectl delete crd egressgroups.tailscale.rajsingh.info
```

## License

Apache 2.0 - See [LICENSE](../../LICENSE) for details.
