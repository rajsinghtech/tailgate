# tailgate

<p align="center">
  <strong>Native Tailscale egress for groups of Kubernetes pods — through a shared node-local gateway, not a tailscaled per pod.</strong>
</p>

<p align="center">
  <a href="https://github.com/rajsinghtech/tailgate/actions/workflows/test.yml"><img src="https://github.com/rajsinghtech/tailgate/actions/workflows/test.yml/badge.svg" alt="CI"></a>
  <a href="https://goreportcard.com/report/github.com/rajsinghtech/tailgate"><img src="https://goreportcard.com/badge/github.com/rajsinghtech/tailgate" alt="Go Report Card"></a>
  <a href="https://github.com/users/rajsinghtech/packages/container/package/tailgate-operator"><img src="https://img.shields.io/badge/ghcr.io-tailgate-blue?logo=docker&logoColor=white" alt="GHCR"></a>
  <a href="https://github.com/rajsinghtech/tailgate/pkgs/container/charts%2Ftailgate-operator"><img src="https://img.shields.io/badge/helm-OCI-0F1689?logo=helm&logoColor=white" alt="Helm"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-green" alt="License"></a>
</p>

`tailgate` is a Kubernetes operator that gives a *group* of pods native **L3 egress** onto a
[Tailscale](https://tailscale.com) tailnet. Match a workload with a label and its pods reach the whole
tailnet by IP — any peer, advertised subnet route, app-connector range, or exit node, over any
protocol — with no Service to declare per destination. Each `EgressGroup` shares one `tailscaled`
gateway, and a node agent stitches member pods into it over a veth pair.

The contrast with destination-by-destination egress: instead of exposing each tailnet target as its
own Kubernetes Service forwarded at L4, a member routes to the entire tailnet natively, the same way a
laptop running Tailscale does.

## Features

- **Whole-tailnet egress, natively** — members reach any tailnet peer (`100.64.0.0/10` and the IPv6 ULA), advertised subnet-router CIDR, app-connector range, or full-tunnel exit node, by IP and over any protocol (TCP, UDP, ICMP, …). The gateway is a kernel-mode `tailscaled`, so this is real L3 routing, not an L4 port forward — and there is no per-destination Service to declare.
- **Pods don't add tailnet devices** — the gateway is a shared node-local `tailscaled`, so scaling a workload from 3 to 3,000 pods adds no devices; the device count tracks nodes, not pods. All member egress on a node carries the gateway's tag as its source identity.
- **Auto-follow scheduling** — the gateway runs **only on nodes that have member pods**, not every node in the cluster. The controller watches member pods and dynamically scopes the gateway DaemonSet via `nodeAffinity`. A group whose pods run on 3 of 50 nodes gets 3 gateway pods, not 50. Set `spec.gateway.nodeSelector` to pin the gateway to specific nodes instead (required for gVisor groups where CNI pre-wire needs the gateway running before member pods boot).
- **Dual-stack** — the pod↔gateway veth is dual-stack, so members reach peers over IPv4 and IPv6 regardless of the cluster's own IP family.
- **Live reconcile** — flip `acceptRoutes`, swap the exit node, or adjust DNS on a running group and the gateway reloads its config in place; member tunnels stay up.
- **Native tailnet DNS** — opt a group into MagicDNS, the tailnet's split-DNS domains, app-connector names, and global forwarding via a mutating webhook, with `cluster.local` preserved.
- **No sidecar, no per-pod config** — membership is a label selector; the agent attaches each selected pod to its node-local gateway over a dedicated `ts0` veth and leaves the pod's primary `eth0` and cluster networking in place.

## Custom resources

| CRD | Scope | Short | Description |
|-----|-------|-------|-------------|
| `EgressGroup` | Cluster | `eg` | A set of pods that egress onto the tailnet through a shared node-local gateway |

API group: `tailscale.rajsingh.info/v1alpha1`.

## How it works

```
               ┌──────────────────────────────────────────────┐
 EgressGroup ─►│  tailgate-operator (controller-runtime)      │
 (selector,    │  reconcile → authkey Secret (OAuth, tagged)  │
  tags,        │            + tailscaled config ConfigMap      │
  exitNode,    │            + per-group gateway DaemonSet      │
  acceptRoutes,│  watches member pods → scopes the DaemonSet  │
  dns,         │  to nodes with members (nodeAffinity)        │
  gateway)     └───────────────┬──────────────────────────────┘
                               │ mints a tagged authkey, renders
                               │ tailscaled.json from the spec
                               ▼
   ┌──────── node (with member pods) ──────────────────────────┐
   │  tailgate-agent (DaemonSet, hostPID/hostNetwork)           │
   │   • installs tailgate-cni as binary/NAD or chained CNI     │
   │   • watches Pods + EgressGroups; for each MEMBER pod:      │
   │       veth-stitches it into the node's gateway netns +     │
   │       injects 100.64/10, the ULA + mirrored routes         │
   │                                                            │
   │  tailgate-gateway (per-group, own netns)                   │
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

- **`tailgate-operator`** (Deployment) — a controller-runtime reconciler. For each `EgressGroup` it mints an OAuth authkey tagged with the group's `spec.tags` (default `tag:k8s`) into a Secret, renders a declarative `tailscaled` config (`ipn.ConfigVAlpha`) from the spec into a ConfigMap, and creates the per-group gateway DaemonSet **scoped to nodes with member pods** (auto-follow) or a static node selector. It watches member pods and re-scopes the DaemonSet as pods schedule and drain. Everything it creates is owner-referenced for garbage collection, and a finalizer deletes the gateway's tailnet device on teardown.
- **`tailgate-gateway`** (per-group, privileged) — the group's shared tailnet node, **one per node that has member pods** (auto-follow) or one per matching node (static pin). It runs the official `tailscale/tailscale` image's `tailscaled` in kernel-TUN mode inside its **own** pod network namespace, so each group's `tailscale0` is isolated and the agent can stitch member veths into it. It enables IP forwarding, MASQUERADEs forwarded member traffic onto `tailscale0` (source identity = the group's tag), and `fwmark`s member traffic into a policy table that routes through `tailscale0` — so `tailscaled` sends each destination where its netmap says (a CGNAT peer, an accepted subnet/app-connector CIDR, or the exit node for `0.0.0.0/0`). It watches its config file and calls LocalAPI `ReloadConfig` on change, with no restart, and persists state so the node identity is stable across restarts.
- **`tailgate-agent`** (DaemonSet, privileged + `hostPID` + `hostNetwork`) — installs the `tailgate-cni` binary when requested, then watches Pods and `EgressGroup`s. For each pod a group selects, it veth-stitches the pod into that node's group-gateway namespace and installs the tailnet routes (`100.64.0.0/10`, the ULA, and the routes the gateway accepts, mirrored from its netmap) toward the gateway. Membership is a label selector evaluated by the agent.

A shared per-group gateway keeps the tailnet device count proportional to the number of groups × nodes-with-members, rather than the number of pods, and preserves the group's tag as the source identity for all member traffic.

## Install

`tailgate` needs a Tailscale **OAuth client** (with the `auth_keys` write scope) so the operator can
mint an authkey for each gateway. Tag the client with an operator identity and let it own whatever tag
your gateways carry — the same shape the Tailscale operator uses with `tag:k8s-operator` / `tag:k8s`:

```jsonc
// tailnet policy file
"tagOwners": {
  "tag:k8s-operator": [],
  "tag:k8s":          ["tag:k8s-operator"],
}
```

Create the OAuth client under **Settings → OAuth clients** with the `auth_keys` write scope and the
`tag:k8s-operator` tag. Gateways are tagged via `spec.tags` on each `EgressGroup` — set it to a tag the
client owns (e.g. `["tag:k8s"]`, or `["tag:k8s", "tag:us-east"]` to add your own dimensions). If you
omit `spec.tags`, a gateway defaults to `tag:k8s`.

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
Secret. The agent reads `TAILGATE_CLUSTER_CIDRS` (Helm: `agent.clusterCIDRs`) — your in-cluster pod
and service ranges, kept on the primary CNI and never steered onto the tailnet. This keeps cluster
networking intact two ways: a member on an exit node still reaches kube-DNS and the API server
instead of blackholing through the full tunnel, and any accepted/mirrored tailnet route that overlaps
a cluster range is dropped before it could capture pod/service traffic. Set it to your real pod
**and** service CIDRs, e.g. `10.244.0.0/16,10.96.0.0/12`.

### CNI install modes, Multus/Cilium, and gVisor

`tailgate` has two different wiring paths:

- **Async agent wiring** (default): the agent creates/moves `ts0` after the pod is Running. This works for normal Linux/runc pods and is safest for general clusters.
- **CNI pre-wiring**: `tailgate-cni` creates `ts0` during CNI ADD, before the sandbox boots. This is required for gVisor/runsc because gVisor's userspace netstack scrapes interfaces, addresses, and routes only at sandbox start and does not hot-plug interfaces added later.

Helm controls CNI behavior with `agent.cniMode`:

| Mode | Meaning | Use when |
|------|---------|----------|
| `disabled` | Do not install `tailgate-cni`; async agent wiring only. | Default/safest for normal pods. |
| `binary` | Copy `tailgate-cni` to `/opt/cni/bin`, but do not mutate any conflist. | Multus clusters; invoke `tailgate-cni` through a `NetworkAttachmentDefinition` for only the pods that need pre-wiring. |
| `chained` | Copy the binary and append `tailgate-cni` to the primary CNI conflist. | Only on clusters where the primary CNI directly supports this chaining model. Do **not** use on Multus+Cilium delegate conflists. |

On clusters where **Multus is the top-level CNI and Cilium is the primary delegate**, do **not** chain `tailgate-cni` into Cilium's delegate conflist. Multus expects the delegate result to carry the primary network info; inserting a non-primary plugin into the delegate conflist can break sandbox creation for unrelated pods. Use `agent.cniMode: binary` and a Multus NAD instead.

Example NAD for gVisor pods:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: tailgate
  namespace: default
spec:
  config: |
    {
      "cniVersion": "0.3.1",
      "name": "tailgate",
      "type": "tailgate-cni"
    }
```

> **The NAD is not created by tailgate.** The consumer (or a cluster admin) must create it in each
> namespace that runs tailscale-egress pods. For example, bhaiya (v0.10.36+) creates it automatically
> on the first tailscale-egress workspace provision.

Then annotate only the gVisor pods that need pre-wiring and label them so an `EgressGroup` selects them:

```yaml
metadata:
  annotations:
    k8s.v1.cni.cncf.io/networks: tailgate
  labels:
    tailgate.rajsingh.info/egress: robbinsdale
spec:
  runtimeClassName: gvisor
```

With this path, `tailgate-cni` runs as a secondary Multus attachment, creates `ts0` before gVisor starts, and writes a prewire record keyed by pod UID. The agent later adopts that pre-created peer and moves it into the node-local gateway netns. The pod keeps Cilium's primary `eth0` untouched.

## Usage

Label the workload you want to egress, then declare an `EgressGroup` that selects it — a selector and
a tag are all you need:

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
  tags:                      # gateway tailnet tag(s); the OAuth client must own these
    - tag:k8s
```

```bash
kubectl apply -f payments-egress.yaml
kubectl get eg
# NAME       PODS   DNS    AGE
# payments   4      false  30s
```

Any pod matching the selector now reaches the **whole tailnet** natively through the shared gateway —
CGNAT peers (`100.64.0.0/10` + the IPv6 ULA) and, because the gateway accepts and mirrors advertised
routes by default, every subnet-router and app-connector CIDR the tailnet exposes — no sidecar, no
per-pod annotation, no per-destination Service. A pod that doesn't match is untouched.

### What members reach

There is no `mode` field and no per-CIDR list; behaviour follows one knob:

| You set | Members reach |
|---------|---------------|
| *(just a selector)* | The whole tailnet: peers by CGNAT IP (`100.64.0.0/10` + ULA), plus every subnet-router / app-connector route the gateway's tag is granted to reach. |
| `acceptRoutes: false` | CGNAT peers + ULA only — the gateway stops pulling in advertised routes. |
| `exitNode: {...}` | Full tunnel — `0.0.0.0/0` and `::/0` through the chosen exit node. |
| `dns: {enabled: true}` | Native tailnet DNS (see below). |

`acceptRoutes` (default true) is the gateway's `--accept-routes`: the gateway accepts the
subnet-router and app-connector routes advertised on the tailnet, and the agent steers them onto
member pods — so a member reaches exactly what the gateway can, like any Tailscale client. **To
restrict a group to a subset, scope its tag with grants** in the tailnet policy (real policy
enforcement), rather than a per-CIDR field on the CRD. Set `acceptRoutes: false` to keep members to
CGNAT peers only. (Cluster pod/service CIDRs are always carved out — see `agent.clusterCIDRs` below.)

### Gateway scheduling

By default the gateway **auto-follows** member pods: it runs only on nodes that currently have
member pods, and the controller re-scopes the DaemonSet as pods schedule and drain. A group whose
pods run on 3 of 50 nodes gets 3 gateway pods, not 50 — the tailnet device count tracks
nodes-with-members, not the cluster size.

```bash
kubectl get eg
# NAME       PODS   NODES   DNS    AGE
# payments   4      2       false  30s
```

Set `spec.gateway.nodeSelector` to pin the gateway to specific nodes instead of auto-following.
This is **required for gVisor groups** — the CNI pre-wire path needs the gateway running before
member pods boot, but auto-follow only lands the gateway after a member pod is already on a node.
With a static node selector the gateway schedules on all matching nodes immediately:

```yaml
spec:
  gateway:
    nodeSelector:
      nodepool: gvisor-pool
```

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
# select an exit node (or change it, or remove it)
kubectl patch eg payments --type=merge -p '{"spec":{"exitNode":{"name":"auto"}}}'

# stop accepting advertised routes (members fall back to CGNAT peers only)
kubectl patch eg payments --type=merge -p '{"spec":{"acceptRoutes":false}}'
```

Accept-routes, exit-node, and DNS changes reload the gateway config without flapping the pod, so traffic in
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
| `TestMirrorRoutes` | a member with just a selector reaches an app-connector CIDR via routes the agent mirrors from the gateway netmap (no per-CIDR config) |
| `TestAgentRestartNoReWire` | an agent rolling update adopts existing wirings (ts0 ifindex unchanged) — no egress blip |
| `TestConfigReconcileNoRestart` | flipping `acceptRoutes` and selecting an `exitNode` on a running group reloads prefs with the gateway pod unchanged (same UID, `restartCount=0`) |

Policy throughout the e2e is expressed with the **grants** field in the policy file (`{src, dst, ip}`,
plus grants-`via` for routing through a specific tagged node).

## License

[Apache 2.0](LICENSE)
