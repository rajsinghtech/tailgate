# tailgate

<p align="center">
  <strong>Native Tailscale egress for Kubernetes pods — one shared gateway per group, not one node per pod.</strong>
</p>

<p align="center">
  <a href="https://github.com/rajsinghtech/tailgate/actions/workflows/test.yml"><img src="https://github.com/rajsinghtech/tailgate/actions/workflows/test.yml/badge.svg" alt="CI"></a>
  <a href="https://goreportcard.com/report/github.com/rajsinghtech/tailgate"><img src="https://goreportcard.com/badge/github.com/rajsinghtech/tailgate" alt="Go Report Card"></a>
  <a href="https://github.com/users/rajsinghtech/packages/container/package/tailgate-operator"><img src="https://img.shields.io/badge/ghcr.io-tailgate-blue?logo=docker&logoColor=white" alt="GHCR"></a>
  <a href="https://github.com/rajsinghtech/tailgate/pkgs/container/charts%2Ftailgate-operator"><img src="https://img.shields.io/badge/helm-OCI-0F1689?logo=helm&logoColor=white" alt="Helm"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-green" alt="License"></a>
</p>

`tailgate` is a Kubernetes operator that puts a *set* of pods onto a [Tailscale](https://tailscale.com)
tailnet for **egress** — reaching CGNAT peers, advertised subnet routes, app connectors, and exit
nodes — without giving every pod its own `tailscaled`. Each `EgressGroup` gets **one shared
tailscaled gateway**; a node agent veth-stitches member pods into that gateway's network namespace.
The tailnet sees `O(groups)` devices, not `O(pods)`.

- **One device per group, not per pod** — scale a workload from 3 to 3,000 pods and the tailnet gains no new nodes for that group. Member pod churn never adds or removes gateway devices from the tailnet.
- **Native L3 egress** — the gateway is a kernel-TUN `tailscaled`, so members get whole-CIDR, all-protocol egress (TCP/UDP/ICMP/…) at the network layer, rather than redirecting traffic through an application-level socket proxy.
- **Reach everything the tailnet exposes** — CGNAT peers (`100.64.0.0/10` + the IPv6 ULA), advertised subnet-router CIDRs, app-connector ranges, and full-tunnel through a chosen exit node.
- **IPv6 / dual-stack** — the pod↔gateway veth is dual-stack, enabling members to reach peers over both their CGNAT v4 and ULA v6 regardless of the cluster's own IP family.
- **Live reconcile, no pod flap** — change `acceptRoutes`, swap the `exitNode`, or adjust DNS on a running group and the gateway hot-reloads its config in place, keeping member tunnels up through the reconcile.
- **Native tailnet DNS** — set `dns.enabled` and members resolve like native Tailscale clients (MagicDNS `*.ts.net`, the tailnet's split-DNS domains, app-connector names, global forwarding) via the gateway's `100.100.100.100`, with `cluster.local` kept on CoreDNS. A mutating webhook injects the resolver — no per-pod config.
- **No Multus, no Spiderpool, no second NIC** — a single chained route-only CNI plugin injects tailnet routes onto each member pod's existing `eth0`.

## Custom Resources

| CRD | Scope | Short | Description |
|-----|-------|-------|-------------|
| `EgressGroup` | Cluster | `eg` | A set of pods that egress onto the tailnet through one shared gateway |

API group: `tailscale.rajsingh.info/v1alpha1`.

## Architecture

```
              ┌──────────────────────────────────────────────┐
 EgressGroup ─►│  tailgate-operator (controller-runtime)      │
 (selector,    │  reconcile → authkey Secret (OAuth, tagged)  │
  mode, routes,│            + tailscaled config ConfigMap      │
  exitNode,    │            + per-group gateway DaemonSet      │
  acceptRoutes)└───────────────┬──────────────────────────────┘
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

Three binaries make up the system (all published to `ghcr.io/rajsinghtech/`):

- **`tailgate-operator`** (Deployment) — a controller-runtime reconciler. For each `EgressGroup` it mints a per-group OAuth authkey tagged `tag:egress-<group>` into a Secret, renders a declarative `tailscaled` config (`ipn.ConfigVAlpha`, `alpha0`) from the spec into a ConfigMap, and stamps out the gateway DaemonSet. Everything it creates is owner-referenced for garbage collection; a finalizer deletes the gateway's tailnet device on teardown.
- **`tailgate-gateway`** (per-group DaemonSet, privileged) — the *one shared tailnet node* for the group. It runs the official `tailscale/tailscale` image's `tailscaled` in kernel-TUN mode inside its **own** pod netns (not `hostNetwork`, so each group's `tailscale0` is isolated and the agent can stitch member veths in). It enables IP forwarding, MASQUERADEs forwarded member traffic onto `tailscale0` (SNAT to the group's tag), and `fwmark`s member traffic into a policy table whose default routes through `tailscale0` — so `tailscaled` routes each destination per its netmap (CGNAT peer, accepted subnet/app-connector CIDR, or the exit node for `0.0.0.0/0`). It watches the config file and calls LocalAPI `ReloadConfig` on change — **no restart**. Node-local so a member always has a same-node gateway to wire to; persisted state keeps the node identity stable across restarts.
- **`tailgate-agent`** (DaemonSet, privileged + `hostPID` + `hostNetwork`) — self-installs the chained route-only CNI plugin (`tailgate-cni`) into the node's conflist, then watches Pods and `EgressGroup`s. For each pod a group selects, it veth-stitches the pod into that node's group gateway netns and injects the tailnet routes (`100.64.0.0/10`, the ULA, and `spec.routes`) toward the gateway. Selection is *data* (an informer), not network plumbing — there is no per-pod annotation or NAD to manage.

Why this shape: tailgate groups pods behind a shared gateway to keep the device count `O(groups)` rather than `O(pods)`, bounding growth by workload count instead of pod count, and each per-group gateway preserves the group's tag as the source identity for all member traffic. Other egress paths make different tradeoffs: the operator's ProxyGroup egress offers centralized control but does not preserve each pod's individual source identity on the tailnet and does not support 4via6; a subnet router holds a WireGuard peer for every node its ACL reaches, which can become a scaling constraint in large tailnets; per-pod `tailscaled` preserves identity but grows the device count and control-plane churn with pod count. `tailgate` keeps a small, fixed set of gateway devices (`O(groups)`), bounded by group rather than tailnet, while still giving members native whole-CIDR L3.

## Install

`tailgate` needs a Tailscale **OAuth client** (with the `auth_keys` write scope and an owner of the
`tag:egress-*` tags) so the operator can mint per-group authkeys. Create one in the Tailscale admin
console under **Settings → OAuth clients**, and make sure your tailnet policy file owns the tags, e.g.:

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

Then install the operator, agent, and CRD with Helm from the GHCR OCI registry:

```bash
helm install tailgate oci://ghcr.io/rajsinghtech/charts/tailgate-operator \
  --namespace tailgate-system
```

> The gateway pods are privileged (kernel TUN + iptables + sysctls) and the agent runs
> `hostNetwork`/`hostPID`. On clusters with Pod Security Admission, label the namespace
> `pod-security.kubernetes.io/enforce: privileged`.

### Install from raw manifests

If you aren't using Helm, apply the CRD and the bundled manifests directly (operator Deployment,
agent DaemonSet, ServiceAccount, and ClusterRole/Binding in `tailgate-system`):

```bash
kubectl apply -f config/crd/tailscale.rajsingh.info_egressgroups.yaml
kubectl apply -f deploy/manifests/tailgate.yaml
```

The operator Deployment reads `GW_IMAGE` (the gateway image it stamps into per-group DaemonSets) and
the `TS_TAILNET` / `TS_OAUTH_CLIENT_ID` / `TS_OAUTH_CLIENT_SECRET` keys from the
`tailgate-tailnet-creds` Secret above. The agent reads `TAILGATE_CLUSTER_CIDRS` — the in-cluster
pod/service ranges to keep on the primary CNI for exit-node members so kube-DNS and the API server
never blackhole through the full tunnel (set it to your real pod **and** service CIDRs, e.g.
`10.244.0.0/16,10.96.0.0/12`).

## Quickstart

Label the workload you want to egress, then declare an `EgressGroup` that selects it. This example
reaches CGNAT peers plus a `10.0.0.0/8` subnet-router range, accepts whatever routes the tailnet
advertises, and full-tunnels everything else through a chosen exit node:

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
    - 10.0.0.0/8             # non-empty routes ⇒ subnet reach — there is no separate "mode" field
  acceptRoutes: true         # gateway accepts subnet-router + app-connector routes (default true)
  exitNode:                  # optional — route members' default route through this tailnet node
    nodeID: exit-fra1.your-org.ts.net
    allowLANAccess: true
  dns:                       # optional — give members native tailnet DNS (see below)
    enabled: true
```

```bash
kubectl apply -f payments-egress.yaml
kubectl get eg
# NAME       PODS   DNS    AGE
# payments   4      true   30s
```

Any pod matching the selector now reaches `100.64.0.0/10`, the IPv6 ULA, and `10.0.0.0/8`
**natively** through the shared gateway — no sidecar, no per-pod annotation, no app changes. A pod
that doesn't match is untouched.

The minimal group is just a selector — members reach CGNAT peers + the ULA, with the gateway placed
node-local automatically:

```yaml
apiVersion: tailscale.rajsingh.info/v1alpha1
kind: EgressGroup
metadata:
  name: peers
spec:
  selector:
    podSelector:
      matchLabels:
        tailgate.dev/egress: "true"
```

## Live reconcile — no pod flap

The gateway is driven entirely by a declarative `tailscaled` config file the operator renders from
the `EgressGroup` spec. Change the spec and the operator re-renders the ConfigMap; the gateway's
entrypoint watches the file (by content hash) and calls LocalAPI `ReloadConfig`. `tailscaled`
re-applies its prefs **in place** — same pod, same node identity, `restartCount` unchanged.

That means these are all live edits that reload the gateway config in place, avoiding a pod restart:

```bash
# add a subnet-router range
kubectl patch eg payments --type=merge -p '{"spec":{"routes":["10.0.0.0/8","192.168.0.0/16"]}}'

# select an exit node (or change it, or remove it)
kubectl patch eg payments --type=merge \
  -p '{"spec":{"exitNode":{"nodeID":"exit-ams1.your-org.ts.net","allowLANAccess":true}}}'

# stop accepting advertised routes
kubectl patch eg payments --type=merge -p '{"spec":{"acceptRoutes":false}}'
```

`cidr` / `exit-node` / `dns` changes re-render the config and reload prefs without flapping the pod,
so traffic in flight survives the reconcile. When the config reloads successfully, editing a
group's reachability avoids a pod restart. (Because tags ride on the authkey rather than the config,
they're deliberately *not* a hot-reload field.)

## What members reach

You write intent; there is no `mode` field — behaviour follows what you set:

| You set | Members reach |
|---------|---------------|
| *(just a selector)* | Tailnet peers by CGNAT IP (`100.64.0.0/10`) + the IPv6 ULA. |
| `routes: [...]` | The above, plus the advertised subnet-router / app-connector CIDRs you list. |
| `exitNode: {...}` | Full-tunnel — `0.0.0.0/0` + `::/0` through the chosen exit node (composes with the above). |
| `dns: {enabled: true}` | Native tailnet DNS (see below). |

`spec.exitNode` puts the full-tunnel default route in a dedicated policy table (never the pod's
`main` table), and the agent keeps the cluster pod/service CIDRs on the primary CNI so kube-DNS and
the API server stay reachable. The gateway *uses* an exit node; it never advertises itself as one.

## Native tailnet DNS

By default a member keeps cluster DNS. Set `dns.enabled` and the operator's mutating webhook gives
matched members native tailnet resolution — no per-pod config:

```yaml
spec:
  selector:
    podSelector: {matchLabels: {tailgate.dev/egress: "true"}}
  dns:
    enabled: true
```

Their resolver becomes the gateway's MagicDNS at `100.100.100.100` — which already serves the whole
tailnet namespace (MagicDNS `*.ts.net`, the tailnet's split-DNS domains, app-connector names, and
global forwarding) — with the auto-detected cluster resolver kept as a secondary for `cluster.local`.
No new routes are needed: `100.100.100.100` is inside the CGNAT range already steered onto members.

The webhook self-signs its serving certificate (no cert-manager dependency) and runs
`failurePolicy: Ignore`, so a webhook outage degrades to cluster DNS rather than blocking pod
creation. Override the secondary resolver / search list with `dns.clusterDNS` / `dns.searchDomains` /
`dns.ndots`; opt an individual pod out with the label `tailgate.dev/no-dns-inject`.

## Development

The dev/test loop runs against a local [kind](https://kind.sigs.k8s.io) cluster and a **per-run
ephemeral tailnet** created and destroyed through the Tailscale org API, so each run is hermetic and
leaves nothing behind.

```bash
# build all four binaries (operator, agent, gateway, cni), build + load images into a kind cluster
hack/build.sh <tag> <kind-cluster>
```

The e2e harness spins up a kind cluster for a given IP family, deploys tailgate, and runs the
datapath tests (which own the ephemeral-tailnet lifecycle):

```bash
hack/e2e.sh v4          # IPv4 kind cluster
hack/e2e.sh dual        # dual-stack
hack/e2e.sh v4 --keep   # leave the cluster up afterwards
```

The same datapath test runs for every family — it curls the peer over **both** v4 and v6 through the
always-dual-stack veth, proving family-independence regardless of the cluster's own primary family.

The e2e suite (`test/e2e/*.go`, build tag `e2e`) authenticates to the org-tailnet create/delete API
via GitHub OIDC **Workload Identity Federation** in CI (no long-lived secret); locally it falls back
to `TS_ORG_OAUTH_CLIENT_ID` / `TS_ORG_OAUTH_CLIENT_SECRET`. Each test creates, runs in, and deletes
its own ephemeral tailnet. It covers the full stack against a real tailnet:

| Test | Proves |
|------|--------|
| `TestEgressDatapath` | member → node-local gateway (veth + nftables MASQUERADE) → CGNAT peer, over both v4 and v6; the gateway programs a native `inet tailgate` nft table |
| `TestSubnetRouterReachability` | a member with `routes` set reaches a backend IP inside an advertised CIDR; non-member denied |
| `TestAppConnectorReachability` | member's traffic to a real app-connector preset's published CIDRs is intercepted through the gateway |
| `TestExitNodeFullTunnel` | exit-node selection + member full-tunnel routing (table 7717 + cluster carve-outs); kube-DNS still resolves |
| `TestMagicDNSThroughGateway` | a member pointed only at `100.100.100.100` resolves a peer's `*.ts.net` name through the gateway |
| `TestNativeDNSWebhook` | `dns.enabled` makes the webhook inject native DNS into a plain member pod, which then resolves + reaches a peer by name; non-member untouched |
| `TestAgentRestartNoReWire` | an agent rolling update adopts existing wirings (ts0 ifindex unchanged) — no egress blip |
| `TestConfigReconcileNoRestart` | flipping `acceptRoutes` + selecting an `exitNode` on a running group reloads prefs with the gateway pod unchanged (same UID, `restartCount=0`) |
| `TestRenderGatewayConfigRoundTrips` | the rendered `tailscaled.json` round-trips through the real `conffile.Load` |

Policy throughout the e2e is expressed using the **grants** field in the policy file (`{src, dst, ip}`,
plus grants-`via` for routing through a specific tagged node).

## License

[Apache 2.0](LICENSE)
