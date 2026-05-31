# tailgate

<p align="center">
  <strong>Native Tailscale egress for groups of Kubernetes pods — one shared gateway per group.</strong>
</p>

<p align="center">
  <a href="https://github.com/rajsinghtech/tailgate/actions/workflows/test.yml"><img src="https://github.com/rajsinghtech/tailgate/actions/workflows/test.yml/badge.svg" alt="CI"></a>
  <a href="https://goreportcard.com/report/github.com/rajsinghtech/tailgate"><img src="https://goreportcard.com/badge/github.com/rajsinghtech/tailgate" alt="Go Report Card"></a>
  <a href="https://github.com/users/rajsinghtech/packages/container/package/tailgate-operator"><img src="https://img.shields.io/badge/ghcr.io-tailgate-blue?logo=docker&logoColor=white" alt="GHCR"></a>
  <a href="https://github.com/rajsinghtech/tailgate/pkgs/container/charts%2Ftailgate-operator"><img src="https://img.shields.io/badge/helm-OCI-0F1689?logo=helm&logoColor=white" alt="Helm"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-green" alt="License"></a>
</p>

`tailgate` is a Kubernetes operator that connects a *group* of pods to a [Tailscale](https://tailscale.com)
tailnet for **egress** — reaching tailnet peers, advertised subnet routes, app connectors, and exit
nodes — without running `tailscaled` in every pod. Each `EgressGroup` gets one shared `tailscaled`
gateway, and a node agent stitches member pods into that gateway over a veth pair. The tailnet sees
one device per group, not one per pod.

## Features

- **One device per group** — a group scales from 3 to 3,000 pods without adding devices to the tailnet; pod churn never changes the tailnet device list.
- **Native L3 egress** — the gateway is a kernel-mode `tailscaled`, so members egress whole CIDRs over any protocol (TCP, UDP, ICMP, …) at the network layer.
- **Reaches the whole tailnet** — CGNAT peers (`100.64.0.0/10` and the IPv6 ULA), subnet-router CIDRs, app-connector ranges, and full-tunnel exit nodes.
- **Dual-stack** — the pod↔gateway veth is dual-stack, so members reach peers over IPv4 and IPv6 regardless of the cluster's own IP family.
- **Live reconcile** — change routes, the exit node, or DNS on a running group and the gateway reloads its config in place; member tunnels stay up.
- **Native tailnet DNS** — opt a group into MagicDNS, the tailnet's split-DNS domains, app-connector names, and global forwarding via a mutating webhook, with `cluster.local` preserved.
- **No sidecar, no per-pod config** — membership is a label selector; the agent attaches each selected pod to its node-local gateway over a dedicated `ts0` veth and leaves the pod's primary `eth0` and cluster networking in place.

## Custom resources

| CRD | Scope | Short | Description |
|-----|-------|-------|-------------|
| `EgressGroup` | Cluster | `eg` | A set of pods that egress onto the tailnet through one shared gateway |

API group: `tailscale.rajsingh.info/v1alpha1`.

## How it works

```
              ┌──────────────────────────────────────────────┐
 EgressGroup ─►│  tailgate-operator (controller-runtime)      │
 (selector,    │  reconcile → authkey Secret (OAuth, tagged)  │
  routes,      │            + tailscaled config ConfigMap      │
  exitNode,    │            + per-group gateway DaemonSet      │
  acceptRoutes,│                                              │
  dns)         └───────────────┬──────────────────────────────┘
                               │ mints tag:egress-<group>, renders
                               │ tailscaled.json from the spec
                               ▼
   ┌──────── node ─────────────────────────────────────────────┐
   │  tailgate-agent (DaemonSet, hostPID/hostNetwork)           │
   │   • installs the route-only CNI (chained into the conflist)│
   │   • watches Pods + EgressGroups; for each MEMBER pod:      │
   │       veth-stitches it into the node's gateway netns +     │
   │       injects 100.64/10, the ULA, and spec.routes          │
   │                                                            │
   │  tailgate-gateway (per-group DaemonSet, own netns)         │
   │   tailscaled --tun=tailscale0 --config=tailscaled.json     │
   │   ip_forward + MASQUERADE onto tailscale0 (SNAT-to-tag)    │
   │   fwmark member traffic → policy table → tailscale0        │
   └────────────────────────────┬──────────────────────────────┘
                                │ WireGuard
                                ▼
            tailnet: CGNAT peers · subnet routers · app
            connectors · exit node (full tunnel)
```

Three components make up the system (images published to `ghcr.io/rajsinghtech/`):

- **`tailgate-operator`** (Deployment) — a controller-runtime reconciler. For each `EgressGroup` it mints a per-group OAuth authkey tagged `tag:egress-<group>` into a Secret, renders a declarative `tailscaled` config (`ipn.ConfigVAlpha`) from the spec into a ConfigMap, and creates the per-group gateway DaemonSet. Everything it creates is owner-referenced for garbage collection, and a finalizer deletes the gateway's tailnet device on teardown.
- **`tailgate-gateway`** (per-group DaemonSet, privileged) — the shared tailnet node for the group. It runs the official `tailscale/tailscale` image's `tailscaled` in kernel-TUN mode inside its **own** pod network namespace, so each group's `tailscale0` is isolated and the agent can stitch member veths into it. It enables IP forwarding, MASQUERADEs forwarded member traffic onto `tailscale0` (source identity = the group's tag), and `fwmark`s member traffic into a policy table that routes through `tailscale0` — so `tailscaled` sends each destination where its netmap says (a CGNAT peer, an accepted subnet/app-connector CIDR, or the exit node for `0.0.0.0/0`). It watches its config file and calls LocalAPI `ReloadConfig` on change, with no restart, and persists state so the node identity is stable across restarts.
- **`tailgate-agent`** (DaemonSet, privileged + `hostPID` + `hostNetwork`) — installs the chained route-only CNI plugin (`tailgate-cni`) into the node's CNI conflist, then watches Pods and `EgressGroup`s. For each pod a group selects, it veth-stitches the pod into that node's group-gateway namespace and installs the tailnet routes (`100.64.0.0/10`, the ULA, and `spec.routes`) toward the gateway. Membership is a label selector evaluated by an informer — there is no per-pod annotation or NetworkAttachmentDefinition to manage.

A shared per-group gateway keeps the tailnet device count proportional to the number of groups rather than the number of pods, and preserves the group's tag as the source identity for all member traffic.

## Install

`tailgate` needs a Tailscale **OAuth client** (with the `auth_keys` write scope, owning the
`tag:egress-*` tags) so the operator can mint per-group authkeys. Create one in the Tailscale admin
console under **Settings → OAuth clients**, and make sure your tailnet policy file owns the tags:

```jsonc
// tailnet policy file
"tagOwners": {
  "tag:egress-*": ["autogroup:admin"],
}
```

Create the namespace and the credentials Secret the operator reads:

```bash
kubectl create namespace tailgate-system

kubectl create secret generic tailgate-tailnet-creds \
  --namespace tailgate-system \
  --from-literal=TS_TAILNET='your-org.ts.net' \
  --from-literal=TS_OAUTH_CLIENT_ID='<oauth-client-id>' \
  --from-literal=TS_OAUTH_CLIENT_SECRET='<oauth-client-secret>'
```

Install the operator, agent, and CRD with Helm from the GHCR OCI registry:

```bash
helm install tailgate oci://ghcr.io/rajsinghtech/charts/tailgate-operator \
  --namespace tailgate-system
```

> The gateway pods are privileged (kernel TUN, packet filtering, sysctls) and the agent runs
> `hostNetwork`/`hostPID`. On clusters with Pod Security Admission, label the namespace
> `pod-security.kubernetes.io/enforce: privileged`.

### Install from raw manifests

Without Helm, apply the CRD and the bundled manifests directly (operator Deployment, agent DaemonSet,
ServiceAccount, and ClusterRole/Binding in `tailgate-system`):

```bash
kubectl apply -f config/crd/tailscale.rajsingh.info_egressgroups.yaml
kubectl apply -f deploy/manifests/tailgate.yaml
```

The operator reads `GW_IMAGE` (the gateway image it stamps into per-group DaemonSets) and the
`TS_TAILNET` / `TS_OAUTH_CLIENT_ID` / `TS_OAUTH_CLIENT_SECRET` keys from the `tailgate-tailnet-creds`
Secret. The agent reads `TAILGATE_CLUSTER_CIDRS` — the in-cluster pod and service ranges to keep on
the primary CNI for exit-node members, so kube-DNS and the API server are never blackholed through the
full tunnel (set it to your real pod **and** service CIDRs, e.g. `10.244.0.0/16,10.96.0.0/12`).

## Usage

Label the workload you want to egress, then declare an `EgressGroup` that selects it. This example
reaches CGNAT peers plus a `10.0.0.0/8` subnet-router range and accepts whatever routes the tailnet
advertises:

```yaml
apiVersion: tailscale.rajsingh.info/v1alpha1
kind: EgressGroup
metadata:
  name: payments
spec:
  selector:
    namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: payments
    podSelector:
      matchLabels:
        tailgate.dev/egress: "true"
  routes:                    # tailnet CIDRs to steer onto members (CGNAT + ULA are always steered)
    - 10.0.0.0/8
  acceptRoutes: true         # accept subnet-router + app-connector routes (default true)
```

```bash
kubectl apply -f payments-egress.yaml
kubectl get eg
# NAME       PODS   DNS    AGE
# payments   4      false  30s
```

Any pod matching the selector now reaches `100.64.0.0/10`, the IPv6 ULA, and `10.0.0.0/8` natively
through the shared gateway — no sidecar, no per-pod annotation, no application changes. A pod that
doesn't match is untouched. The minimal group is just a selector — members reach CGNAT peers and the
ULA, with the gateway placed node-local automatically.

### What members reach

There is no `mode` field; behaviour follows what you set:

| You set | Members reach |
|---------|---------------|
| *(just a selector)* | Tailnet peers by CGNAT IP (`100.64.0.0/10`) and the IPv6 ULA. |
| `routes: [...]` | The above, plus the advertised subnet-router / app-connector CIDRs you list. |
| `exitNode: {...}` | Full tunnel — `0.0.0.0/0` and `::/0` through the chosen exit node (composes with the above). |
| `dns: {enabled: true}` | Native tailnet DNS (see below). |

When a group enables `dns` and isn't using an exit node, the gateway also mirrors the tailnet routes
it can reach onto members, so app-connector and advertised CIDRs route without being listed in
`routes` — egress follows what DNS resolves. Force it on or off with `mirrorRoutes: true|false`.

### Exit nodes

`exitNode.name` uses the same value space as `tailscale set --exit-node`: a tailnet IP, a MagicDNS
name, a StableNodeID, or `auto` / `auto:any` to let an eligible exit node be picked automatically.

```yaml
spec:
  exitNode:
    name: "exit-fra1.your-org.ts.net"   # a node ref, or "auto" / "auto:any"
    allowLANAccess: true                # keep direct LAN/cluster reach while full-tunnelling
```

A full tunnel goes in a dedicated policy table (never the pod's `main` table), and the agent keeps the
cluster pod/service CIDRs on the primary CNI so kube-DNS and the API server stay reachable. The
gateway *uses* an exit node; it never advertises itself as one.

`name: "auto"` resolves to a concrete eligible exit node (one advertising `0.0.0.0/0`, preferring an
online node), because the declarative `tailscaled` config can't carry `auto` directly. The chosen node
is reported in `status.resolvedExitNode`, and the operator re-resolves periodically so a node going
offline fails over without an edit.

### Native tailnet DNS

By default a member keeps cluster DNS. Set `dns.enabled` and a mutating webhook gives matched members
native tailnet resolution — no per-pod config:

```yaml
spec:
  selector:
    podSelector: {matchLabels: {tailgate.dev/egress: "true"}}
  dns:
    enabled: true
```

Their resolver becomes the gateway's MagicDNS at `100.100.100.100` — which serves the whole tailnet
namespace (MagicDNS `*.ts.net`, the tailnet's split-DNS domains, app-connector names, and global
forwarding) — with the auto-detected cluster resolver kept as a secondary for `cluster.local`. No new
routes are needed: `100.100.100.100` is inside the CGNAT range already steered onto members.

The webhook self-signs its serving certificate (no cert-manager dependency) and runs
`failurePolicy: Ignore`, so a webhook outage degrades to cluster DNS rather than blocking pod
creation. Override the secondary resolver and search list with `dns.clusterDNS` / `dns.searchDomains`
/ `dns.ndots`.

### Live reconcile

The gateway is driven entirely by the declarative `tailscaled` config the operator renders from the
spec. Editing the spec re-renders the ConfigMap; the gateway watches the file by content hash and
calls LocalAPI `ReloadConfig`, so `tailscaled` re-applies its prefs in place — same pod, same node
identity, `restartCount` unchanged.

```bash
# add a subnet-router range
kubectl patch eg payments --type=merge -p '{"spec":{"routes":["10.0.0.0/8","192.168.0.0/16"]}}'

# select an exit node (or change it, or remove it)
kubectl patch eg payments --type=merge -p '{"spec":{"exitNode":{"name":"auto"}}}'

# stop accepting advertised routes
kubectl patch eg payments --type=merge -p '{"spec":{"acceptRoutes":false}}'
```

Route, exit-node, and DNS changes reload the gateway config without flapping the pod, so traffic in
flight survives the reconcile. (Tags ride on the authkey rather than the config, so a tag change is
deliberately not a hot-reload field.)

## Development

The dev/test loop runs against a local [kind](https://kind.sigs.k8s.io) cluster and a **per-run
ephemeral tailnet** created and destroyed through the Tailscale org API, so each run is hermetic.

```bash
# build the binaries (operator, agent, gateway, cni) and load images into a kind cluster
hack/build.sh <tag> <kind-cluster>

# spin up a kind cluster of a given IP family, deploy tailgate, run the datapath tests
hack/e2e.sh v4          # IPv4 cluster
hack/e2e.sh dual        # dual-stack
hack/e2e.sh v4 --keep   # leave the cluster up afterwards
```

The same datapath test runs for every family — it curls the peer over **both** v4 and v6 through the
always-dual-stack veth, proving family-independence regardless of the cluster's own primary family.

The e2e suite (`test/e2e/*.go`, build tag `e2e`) authenticates to the org-tailnet create/delete API
via GitHub OIDC **Workload Identity Federation** in CI (no long-lived secret); locally it falls back
to `TS_ORG_OAUTH_CLIENT_ID` / `TS_ORG_OAUTH_CLIENT_SECRET`. Each test creates, runs in, and deletes
its own ephemeral tailnet, covering the full stack against real Tailscale control:

| Test | Proves |
|------|--------|
| `TestEgressDatapath` | member → node-local gateway (veth + nftables MASQUERADE) → CGNAT peer, over both v4 and v6; the gateway programs a native `inet tailgate` nft table |
| `TestSubnetRouterReachability` | a member with `routes` set reaches a backend inside an advertised CIDR; a non-member is denied |
| `TestAppConnectorReachability` | a member's traffic to an app-connector preset's published CIDRs is intercepted through the gateway |
| `TestExitNodeFullTunnel` | exit-node selection + member full-tunnel routing (policy table + cluster carve-outs); kube-DNS still resolves |
| `TestExitNodeAutoSelect` | `exitNode.name: auto` resolves a concrete exit node into `status.resolvedExitNode` and installs the member full tunnel |
| `TestMagicDNSThroughGateway` | a member pointed only at `100.100.100.100` resolves a peer's `*.ts.net` name through the gateway |
| `TestNativeDNSWebhook` | `dns.enabled` makes the webhook inject native DNS into a plain member pod, which then resolves and reaches a peer by name; a non-member is untouched |
| `TestMirrorRoutes` | a member with no `spec.routes` reaches an app-connector CIDR via routes mirrored from the gateway netmap |
| `TestAgentRestartNoReWire` | an agent rolling update adopts existing wirings (ts0 ifindex unchanged) — no egress blip |
| `TestConfigReconcileNoRestart` | flipping `acceptRoutes` and selecting an `exitNode` on a running group reloads prefs with the gateway pod unchanged (same UID, `restartCount=0`) |

Policy throughout the e2e is expressed with the **grants** field in the policy file (`{src, dst, ip}`,
plus grants-`via` for routing through a specific tagged node).

## License

[Apache 2.0](LICENSE)
