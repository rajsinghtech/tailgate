# Agent Notes

## gVisor and CNI pre-wiring

- gVisor/runsc does not hot-plug interfaces into its userspace netstack. It scrapes interfaces, addresses, and routes from the pod netns at sandbox start.
- For gVisor pods, `ts0` must exist during CNI ADD. Agent-only async wiring after `PodRunning` is invisible inside gVisor.
- The supported Multus path is `agent.cniMode: binary` plus a `NetworkAttachmentDefinition` that invokes `tailgate-cni` only for annotated pods.
- Do not globally chain `tailgate-cni` into Cilium's delegate conflist when Multus is the top-level CNI. Multus expects the delegate result to preserve primary network info; inserting a secondary plugin there can break unrelated pod sandbox creation.
- In Multus/NAD mode, `tailgate-cni` may be invoked without `prevResult`. It uses `K8S_POD_UID` as the stable key, creates `ts0` pre-sandbox, and writes `/run/tailgate/prewire/<podUID>` so the agent can adopt the host-side peer after the pod is Running.
- If a cluster gets stuck with `plugin type="tailgate-cni" failed (add): failed to unmarshal raw result: unexpected end of JSON input`, remove `tailgate-cni` from the node CNI conflists and remove `/opt/cni/bin/tailgate-cni`; then roll an agent with `TAILGATE_INSTALL_CNI=disabled` or `binary`.

## NAD ownership

- tailgate does **not** create the `NetworkAttachmentDefinition`. The consumer (e.g. bhaiya, or a cluster admin) must create it in each namespace that runs tailscale-egress pods.
- The NAD config must be `{"cniVersion":"0.3.1","name":"tailgate","type":"tailgate-cni"}` and the NAD name must match the pod's `k8s.v1.cni.cncf.io/networks` annotation value.
- bhaiya (v0.10.36+) creates the NAD automatically in its namespace on the first tailscale-egress workspace provision (idempotent — `AlreadyExists` is a no-op). The bhaiya Role must grant `create/get/list/watch` on `k8s.cni.cncf.io/network-attachment-definitions`.

## Release/deployment notes

- Chart values use `agent.cniMode`, not a boolean. Valid values: `disabled`, `binary`, `chained`.
- `binary` installs `/opt/cni/bin/tailgate-cni` without mutating CNI config.
- `chained` mutates the primary conflist and should only be used on CNI setups proven to support that model.
- E2E kind tests intentionally set `TAILGATE_INSTALL_CNI=false` because they do not run gVisor and validate the async agent path.
