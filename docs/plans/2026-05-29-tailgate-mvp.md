# tailgate MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A pod selected into an `EgressGroup` reaches tailnet CGNAT peers (`100.64.0.0/10`) and advertised subnet-router CIDRs through a single per-group kernel-TUN gateway, with **no Multus/Spiderpool** — proving "scale a workload 3→3,000 pods = 0 new tailnet devices."

**Architecture:** A standalone controller-runtime operator reconciles an `EgressGroup` CRD into (1) a per-group **gateway** Deployment (official-image `tailscaled` in kernel-TUN mode = subnet router that MASQUERADEs cluster traffic onto `tailscale0`) fronted by a ClusterIP, and (2) a per-group OAuth-minted authkey Secret + `tag:egress-<group>`. A node **agent** DaemonSet self-installs a chained **route-only CNI** plugin (records each pod's netns) and, driven by a pod informer scoped to group selectors, injects `100.64.0.0/10` + advertised CIDRs into *member* pods' existing `eth0` toward the gateway ClusterIP. Selection is data (informer), not network plumbing. Source identity is the gateway's `tag:egress-<group>` (SNAT). This is the `datapath: kernel` + `attach: routed` + SNAT-to-tag path from the design doc (`code/DESIGN.md` §4.4, §5, §9.1).

**Testing (two-tier, per-task — see "Testing strategy" below):** every task ends green on a real cluster. A fast inner loop (controller-runtime **fake client** for reconcile + **envtest** for API semantics) plus a **per-task e2e gate on a real `kind` cluster** for anything that must actually *run* (gateway pod Ready, CNI installed on the node, routes injected in a pod netns, real egress to a tailnet peer). Stack = plain Go `testing` + `gomega` matchers + the `sigs.k8s.io/kind` Go library, **dogfooding the agent DaemonSet** to install the CNI — the same shape as the Tailscale operator's own e2e (`cmd/k8s-operator/e2e/`). Datapath tests run against a **per-run ephemeral tailnet** created via the Tailscale org API (`POST /api/v2/organizations/-/tailnets`) and deleted after — CI authenticates by **GitHub OIDC token-exchange** so there's *no long-lived secret* (the `tailbridge` integration-test pattern: `github.com/rajsinghtech/tailbridge` `integration_test/`). The kind harness + reusable `test/e2e/framework` + `test/e2e/tailnet` packages are **built first (Task 0b)** so every later task has them.

**Tech Stack:** Go 1.26; `sigs.k8s.io/controller-runtime` + `kubebuilder` scaffolding; `tailscale.com/client/tailscale/v2` (OAuth authkey minting); `github.com/containernetworking/cni` (`pkg/skel`, `pkg/types`) + `github.com/vishvananda/netlink` + `github.com/vishvananda/netns` (CNI route inject); `k8s.io/client-go` informers (agent); official `tailscale/tailscale` image (gateway datapath). **Test:** `sigs.k8s.io/controller-runtime/pkg/client/fake` (reconcile) + `sigs.k8s.io/controller-runtime/pkg/envtest` (API) + std `testing` + `github.com/onsi/gomega` matchers (standalone, no ginkgo runner) + `sigs.k8s.io/kind` (programmatic kind) + a `test/e2e/tailnet` helper that creates/applies-ACL/deletes a **per-run ephemeral tailnet** via the Tailscale org API + `tsnet` ephemeral peer (datapath target on that tailnet). Module path stays `raj/personal/code`.

---

## File Structure

| Path | Responsibility |
|---|---|
| `cmd/tailgate-operator/main.go` | Operator entrypoint: manager + `EgressGroupReconciler` wiring |
| `cmd/tailgate-agent/main.go` | Node DaemonSet entrypoint: CNI install + pod informer + route injector + readiness |
| `cmd/tailgate-cni/main.go` | Chained CNI plugin binary: records pod netns, passes prevResult through |
| `cmd/tailgate-gateway/main.go` | Gateway entrypoint: start `tailscaled` (kernel TUN), `up`, set `ip_forward`, install MASQUERADE |
| `api/v1alpha1/groupversion_info.go` | Scheme registration for `egress.tailgate.dev/v1alpha1` |
| `api/v1alpha1/egressgroup_types.go` | `EgressGroup` CRD Go types (MVP subset) |
| `api/v1alpha1/zz_generated.deepcopy.go` | Generated (controller-gen) |
| `internal/controller/egressgroup_controller.go` | Reconcile loop, finalizer, status |
| `internal/controller/gateway.go` | Gateway Deployment + Service builders |
| `internal/controller/authkey.go` | OAuth tag-ensure + authkey mint → Secret |
| `internal/tsclient/tsclient.go` | Interface over the Tailscale API (mockable) |
| `internal/netinfo/netinfo.go` | Shared CNI↔agent handoff: `/var/run/tailgate/<podIP>` JSON read/write |
| `internal/cni/route.go` | CNI `cmdAdd/Del/Check` logic (records netns, no veth) |
| `internal/agent/catalogue.go` | Pod informer → `podIP → {group, netnsPath}` membership map |
| `internal/agent/injector.go` | Add/remove tailnet routes in a member pod's netns |
| `internal/agent/installer.go` | CNI binary + conflist install, reconcile loop, readiness flag |
| `config/crd/` `config/rbac/` `config/manager/` | Generated manifests (kustomize) |
| `deploy/` | Operator Deployment + agent DaemonSet + RBAC + Secret template YAML |
| `internal/controller/egressgroup_controller_test.go` | envtest reconcile tests |
| `internal/controller/authkey_test.go` | authkey mint test (mock tsclient) |
| `internal/cni/route_test.go` | CNI logic test (table-driven + netns) |
| `internal/agent/injector_test.go` | route inject/remove test (real netns) |
| `test/e2e/kind-config.yaml` | kind cluster config (control-plane + 2 workers, kindnet, pinned node image) |
| `test/e2e/framework/framework.go` | **reusable** e2e harness: cluster lifecycle, build+load, wait, exec, `AssertRouteInNetns`, diagnostics |
| `test/e2e/tailnet/tailnet.go` | **per-run ephemeral tailnet** lifecycle (create/apply-ACL/mint-authkey/delete via the Tailscale org API + OIDC token-exchange) — the `tailbridge` pattern |
| `test/e2e/main_test.go` | `TestMain`: ensure cluster + build/load images + create ephemeral tailnet + install operator+agent once; teardown deletes the tailnet |
| `test/e2e/*_test.go` | per-task e2e suites (`//go:build e2e`): harness smoke, crd, reconcile, gateway, attach, datapath |
| `charts/tailgate/` | Helm chart (operator Deployment + agent DaemonSet + RBAC) used by e2e + prod |
| `.github/workflows/ci.yaml` | `unit` job (envtest, always) + `e2e` job (kind, on PR, `if: always()` cleanup) |

**Decomposition rationale:** the operator (control plane), the agent+CNI (node data-plane attach), and the gateway (datapath) are three independently-testable units that change for different reasons. `internal/tsclient` and `internal/netinfo` are the seams that keep the controller mockable and the CNI↔agent contract explicit.

---

## Task 0: Scaffold the project

**Files:**
- Create: `go.mod` (already `raj/personal/code`), `Makefile`, `PROJECT`, `.golangci.yml`, `hack/tools.go`

- [ ] **Step 1: Initialize kubebuilder layout in place**

Run (from `code/`):
```bash
go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5
go get sigs.k8s.io/controller-runtime@v0.19.3
go get k8s.io/client-go@v0.31.3
go get github.com/containernetworking/cni@v1.2.3
go get github.com/vishvananda/netlink@v1.3.0 github.com/vishvananda/netns@v0.0.5
go get tailscale.com@v1.86.0
```

- [ ] **Step 2: Create the Makefile**

```makefile
CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen
ENVTEST ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest

.PHONY: generate manifests test build
generate:
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=tailgate-operator paths=./... output:crd:dir=config/crd output:rbac:dir=config/rbac
test:
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use 1.31.0 -p path)" go test ./... -count=1
build:
	go build -o bin/tailgate-operator ./cmd/tailgate-operator
	go build -o bin/tailgate-agent    ./cmd/tailgate-agent
	go build -o bin/tailgate-cni      ./cmd/tailgate-cni
	go build -o bin/tailgate-gateway  ./cmd/tailgate-gateway
```

- [ ] **Step 3: Create `hack/boilerplate.go.txt`** (empty license header is fine: a single `// Copyright tailgate authors` line).

- [ ] **Step 4: Verify the toolchain builds**

Run: `go build ./... 2>&1 | head` — Expected: no errors (no packages yet → no output).

- [ ] **Step 5: Commit**

```bash
git add code/go.mod code/go.sum code/Makefile code/hack
git commit -m "chore: scaffold tailgate operator toolchain"
```

---

## Testing strategy (two-tier, per-task)

Two gates per task. **Inner loop** (every save, no cluster): Go `testing` table tests for pure logic; controller-runtime **fake client** for reconcile (assert *what objects the reconciler writes* — the Tailscale-operator pattern, runs in ms); **envtest** (real apiserver+etcd, **no** kubelet/CNI/controllers/scheduler) only where CRD validation/defaulting, webhooks, or cache/requeue timing need real API semantics. **Outer gate** (per task, on `kind`): the cheapest *real-cluster* assertion that proves this task's increment — envtest cannot observe pod scheduling, GC cascade, route injection, or egress, so those require kind.

**Task tiers** — which gate proves the increment:

| tier | task kind | gate | example assertion |
|---|---|---|---|
| **A** | pure types/logic (deepcopy, conflist render, `extractIPv4`, `matchGroup`, `masqueradeArgs`, `netinfo`) | unit, no cluster | table test / golden; CNI binary via `cnitool` in a bare `ip netns` |
| **B** | controller reconcile (CR → child objects, status, finalizer, owner refs) | **envtest** (kind only if it must *run*) | child Deployment/Secret created with correct spec; status flips; finalizer added |
| **C** | deployable component (operator Deployment up; agent installs CNI on node) | **kind** | DaemonSet `numberReady==desired`; `/host/opt/cni/bin/tailgate-cni` present; conflist valid+chained |
| **D** | datapath (routes in pod netns; real egress) | **kind** | route in pod netns via `nsenter`; labeled pod reaches a tsnet CGNAT peer; unlabeled does not |

**Per-task e2e map** — every task's done-gate; assertions live in `test/e2e/<area>_test.go` using the framework:

| Task | Tier | e2e / done-gate assertion |
|---|---|---|
| 0b harness | C | `kind` boots from config; a busybox pod schedules + Ready (framework smoke) |
| 1 CRD | B | apply CRD to kind; a valid `EgressGroup` is accepted, an invalid one rejected by CRD validation |
| 2 tsclient | A | interface satisfied (compile) — no cluster |
| 3 authkey | B | envtest: reconcile a stub EG → `tailgate-<g>-authkey` Secret with `TS_AUTHKEY` |
| 4 gateway builders | A | unit: Deployment/Service spec asserts — no cluster |
| 5 reconcile | B+C | envtest: child objects + finalizer; **kind:** apply EG → gateway Deployment `numberReady>=1` |
| 6 gateway entrypoint | C | kind: gateway pod Ready; `tailscale status` inside shows a self IP on the **per-run ephemeral tailnet** (skipped if no org creds/OIDC) |
| 7 netinfo | A | unit: write/read/remove round-trip — no cluster |
| 8 route-only CNI | A | unit + `cnitool` ADD in a bare netns → netinfo file written; prevResult passed through |
| 9 injector | D | kind: label a pod into a group → `AssertRouteInNetns(pod, "100.64.0.0/10", gwIP)`; unlabel → gone |
| 10 catalogue/agent | C+D | kind: agent DaemonSet Ready; a newly-created member pod gets routes within the `Eventually` window |
| 11 CNI installer | C | kind: agent installs binary + chains conflist on each node; restart agent → conflist re-patched; read-only-`/opt/cni/bin` sim → `/readyz` unready (no silent no-attach) |
| 12 full path | D | kind (multi-node + anti-affinity): labeled pod on node A reaches a tsnet ephemeral CGNAT peer via the gateway on node B; unlabeled pod refused |

Discipline (provided by Task 0b): **one shared long-lived kind cluster**; **one namespace per test** (`t.Cleanup` teardown); bounded `Eventually` (90s/2s); datapath tests run **serial**; `kind load` only changed images (digest cache); `DumpDiagnostics` (describe + events + logs + `ip route`) on every failure. Real-tailnet assertions (Tasks 6, 12) run against a **per-run ephemeral tailnet** (Task 0b's `test/e2e/tailnet` helper: create → apply ACL → mint authkeys → run → delete), authenticated by **GitHub OIDC token-exchange** in CI (no long-lived secret) or an org OAuth client locally. If neither is present those tests **skip**; route-*presence* (Task 9) is mocked and always runs, so the bulk passes on forks/PRs without creds.

---

## Task 0b: kind e2e harness + `test/e2e/framework` (build FIRST)

**Files:**
- Create: `test/e2e/kind-config.yaml`, `test/e2e/framework/framework.go`, `test/e2e/main_test.go`, `test/e2e/smoke_test.go`
- Modify: `Makefile` (add `kind-up`/`e2e`/`e2e-clean`), `.github/workflows/ci.yaml`

- [ ] **Step 1: Write `test/e2e/kind-config.yaml`** (multi-node so cross-node datapath is reachable; kindnet kept; pin node image so the `10-kindnet.conflist` shape is deterministic)

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  podSubnet: "10.244.0.0/16"   # kindnet enabled (disableDefaultCNI: false) — pure-L3, preserves pod src
nodes:
  - role: control-plane
  - role: worker
  - role: worker
```

- [ ] **Step 2: Add Makefile targets**

```makefile
KIND_CLUSTER ?= tailgate-e2e
NODE_IMAGE   ?= kindest/node:v1.31.0

.PHONY: kind-up e2e e2e-clean
kind-up:
	kind get clusters | grep -qx $(KIND_CLUSTER) || \
	  kind create cluster --name $(KIND_CLUSTER) --image $(NODE_IMAGE) --config test/e2e/kind-config.yaml --wait 5m
e2e: kind-up
	go test -tags e2e -p 1 -parallel 4 -timeout 25m ./test/e2e/...
e2e-clean:
	kind delete cluster --name $(KIND_CLUSTER)
```

- [ ] **Step 3: Write `test/e2e/framework/framework.go`** (`//go:build e2e`; the load-bearing helpers — cluster connect, wait, exec, the `nsenter` route assertion, diagnostics)

```go
//go:build e2e

package framework

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

type Cluster struct {
	Name string
	Kube client.Client
}

// EnsureCluster connects to the kubeconfig context (kind sets it on create).
func EnsureCluster(name string, scheme *runtime.Scheme) (*Cluster, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	return &Cluster{Name: name, Kube: c}, nil
}

// NewNamespace creates a uniquely-named ns and tears it down on test cleanup.
func (c *Cluster) NewNamespace(t *testing.T, prefix string) string {
	t.Helper()
	ns := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%100000)
	must(t, c.Kube.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	t.Cleanup(func() { _ = c.Kube.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
	return ns
}

func (c *Cluster) WaitPodReady(t *testing.T, ns, name string) {
	t.Helper()
	c.eventually(t, 90*time.Second, func() error {
		var p corev1.Pod
		if err := c.Kube.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &p); err != nil {
			return err
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return nil
			}
		}
		return fmt.Errorf("pod %s/%s not Ready", ns, name)
	})
}

// kubectlExec runs argv in a pod via kubectl (busybox/netshoot member pods have `ip`).
func (c *Cluster) kubectlExec(ns, pod string, argv ...string) (string, error) {
	args := append([]string{"exec", "-n", ns, pod, "--"}, argv...)
	var out bytes.Buffer
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// AssertRouteInNetns asserts `wantDst via wantGw` is present in the pod's route table.
func (c *Cluster) AssertRouteInNetns(t *testing.T, ns, pod, wantDst, wantGw string) {
	t.Helper()
	c.eventually(t, 90*time.Second, func() error {
		out, err := c.kubectlExec(ns, pod, "ip", "route", "show")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, wantDst) && strings.Contains(line, "via "+wantGw) {
				return nil
			}
		}
		return fmt.Errorf("route %q via %q not found in:\n%s", wantDst, wantGw, out)
	})
}

func (c *Cluster) eventually(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := timeNow().Add(timeout) // timeNow injected for determinism; real impl uses time.Now via a var
	var last error
	for timeNow().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		sleep(2 * time.Second)
	}
	c.DumpDiagnostics(t)
	t.Fatalf("eventually timed out: %v", last)
}

// DumpDiagnostics shells `kubectl get events` + pod logs to t.Log on failure.
func (c *Cluster) DumpDiagnostics(t *testing.T) {
	t.Helper()
	out, _ := exec.Command("kubectl", "get", "events", "-A", "--sort-by=.lastTimestamp").CombinedOutput()
	t.Logf("=== events ===\n%s", out)
}

func must(t *testing.T, err error) { t.Helper(); if err != nil { t.Fatal(err) } }
```

(Two small seams to wire in real impl: `timeNow`/`sleep` are package vars defaulting to `time.Now`/`time.Sleep` — keeps `eventually` deterministic in framework unit tests; add `runtime` import for `*runtime.Scheme`. For the cross-node `nsenter` ground-truth path use `docker exec <kindnode> nsenter -t <pid> -n ip route` per the e2e research — add `AssertRouteViaNsenter` later when Task 12 needs node-side truth.)

- [ ] **Step 4: Write `test/e2e/main_test.go`** (`//go:build e2e`; ensure cluster + install operator/agent once via Helm or `kubectl apply config/default`)

```go
//go:build e2e

package e2e

import (
	"os"
	"testing"
	"raj/personal/code/test/e2e/framework"
)

var cluster *framework.Cluster

func TestMain(m *testing.M) {
	c, err := framework.EnsureCluster("tailgate-e2e", newScheme())
	if err != nil { panic(err) }
	cluster = c
	// (Tasks 5/11 add: build+load images and install the operator+agent here once.)
	os.Exit(m.Run())
}
```

- [ ] **Step 5: Write the failing smoke e2e** `test/e2e/smoke_test.go` (`//go:build e2e`)

```go
//go:build e2e

package e2e

import (
	"context"
	"testing"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHarnessSmoke(t *testing.T) {
	ns := cluster.NewNamespace(t, "smoke")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "busybox", Namespace: ns},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "busybox", Image: "busybox:1.36", Command: []string{"sleep", "3600"}}}},
	}
	if err := cluster.Kube.Create(context.Background(), pod); err != nil { t.Fatal(err) }
	cluster.WaitPodReady(t, ns, "busybox") // proves real kubelet + kindnet schedule + run a pod
}
```

- [ ] **Step 6: Run it — verify the harness works end to end**

Run: `make e2e` (boots kind from the config, runs the tagged smoke test)
Expected: `TestHarnessSmoke` PASS — kind cluster came up, busybox scheduled and reached Ready; the namespace is torn down on cleanup.

- [ ] **Step 6b: Write `test/e2e/tailnet/tailnet.go`** (`//go:build e2e`) — per-run ephemeral-tailnet lifecycle, the `tailbridge` pattern ported to Go. The v2 client doesn't expose the org `tailnets` endpoint, so this wraps the raw HTTP calls and owns isolation + teardown.

```go
//go:build e2e
package tailnet

// Ephemeral is a throwaway tailnet for one e2e run. Close() deletes it.
type Ephemeral struct {
	Name         string // dnsName, e.g. tailgate-ci-1234.ts.net
	ClientID     string // OAuth client the create response bundles
	ClientSecret string
	orgToken     string // org access token, for delete
}

// Create POSTs /api/v2/organizations/-/tailnets {displayName,tailnetName} and
// returns the tailnet plus its bundled oauthClient (id/secret). orgToken comes
// from OrgTokenFromOIDC (CI) or OrgTokenFromClient (local).
func Create(ctx context.Context, orgToken, displayName string) (*Ephemeral, error)

// ApplyACL POSTs /api/v2/tailnet/<Name>/acl — set tag:egress-*, tag:ci, grants.
func (e *Ephemeral) ApplyACL(ctx context.Context, hujson []byte) error

// MintAuthKey == tsclient.New(e.Name, e.ClientID, e.ClientSecret).MintAuthKey(tags).
func (e *Ephemeral) MintAuthKey(ctx context.Context, tags []string) (string, error)

// Close DELETEs /api/v2/tailnet/<Name>. Call from TestMain teardown / t.Cleanup.
func (e *Ephemeral) Close(ctx context.Context) error

// OrgTokenFromOIDC POSTs /api/v2/oauth/token-exchange {client_id, jwt} where jwt is
// the GitHub Actions OIDC token (audience = api.tailscale.com/<clientID>) — no static secret.
func OrgTokenFromOIDC(ctx context.Context, clientID, jwt string) (string, error)
// OrgTokenFromClient POSTs /api/v2/oauth/token (client_credentials) for local dev.
func OrgTokenFromClient(ctx context.Context, clientID, secret string) (string, error)
```

Wiring (in `main_test.go`, fleshed out at Task 5 when the operator is first installed): create ONE ephemeral tailnet per e2e run, `ApplyACL`, feed its `ClientID`/`ClientSecret` to the operator install (so the operator mints gateway authkeys against the throwaway tailnet) and to the `tsnet` test peer; `Close` in the `TestMain` teardown. If `TS_OAUTH_*`/OIDC are absent, skip the datapath tests (route-presence still runs mocked).

- [ ] **Step 7: Write `.github/workflows/ci.yaml`** — four jobs, mirroring `tailbridge/.github/workflows/integration-test.yml`:
  - **`unit`** (always, every push): `make test` (envtest). Merge-blocking baseline.
  - **`setup-tailnet`** (`permissions: id-token: write`): fetch the GitHub OIDC JWT (`curl "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=api.tailscale.com/<clientID>"`), `OrgTokenFromOIDC` → org token, `Create` an ephemeral tailnet `tailgate-ci-${{ github.run_id }}`, `ApplyACL`; output its `dnsName` + bundled `oauthClient` id/secret (mask them).
  - **`e2e`** (`needs: setup-tailnet`): `helm/kind-action@v1` (`version: v0.30.0`, `node_image: kindest/node:v1.31.0`, `config: ./test/e2e/kind-config.yaml`); `tailscale/github-action@v4` with the ephemeral tailnet's oauth client so the runner joins it (lets the test process dial the CGNAT peer); `make e2e` with the tailnet creds in env; `if: always()` → upload `DumpDiagnostics`. Image pulls use `pullPolicy: IfNotPresent` (the `kind load` footgun).
  - **`cleanup-tailnet`** (`if: always()`, `needs: [setup-tailnet, e2e]`): `Close` the ephemeral tailnet (`continue-on-error: true`) so a failed run never leaks a tailnet.

- [ ] **Step 8: Commit**

```bash
git add code/test/e2e code/Makefile code/.github/workflows/ci.yaml
git commit -m "test: kind e2e harness + framework + per-run ephemeral-tailnet helper (tailbridge pattern)"
```

---

## Task 1: `EgressGroup` CRD types (MVP subset)

**Files:**
- Create: `api/v1alpha1/groupversion_info.go`, `api/v1alpha1/egressgroup_types.go`
- Test: `api/v1alpha1/egressgroup_types_test.go`

- [ ] **Step 1: Write the failing test**

```go
package v1alpha1

import (
	"testing"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEgressGroupDefaultsAndDeepCopy(t *testing.T) {
	eg := &EgressGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: EgressGroupSpec{
			Selector: EgressSelector{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": "payments"}}},
			Mode:     ModeSubnet,
			Routes:   []string{"10.50.0.0/16"},
			Tailnet:  "example.com",
		},
	}
	cp := eg.DeepCopy()
	if cp.Spec.Routes[0] != "10.50.0.0/16" {
		t.Fatalf("deepcopy lost routes: %+v", cp.Spec.Routes)
	}
	cp.Spec.Routes[0] = "0.0.0.0/0"
	if eg.Spec.Routes[0] == "0.0.0.0/0" {
		t.Fatal("deepcopy did not deep-copy the Routes slice")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./api/v1alpha1/ -run TestEgressGroup -v`
Expected: FAIL — `EgressGroup`/`EgressGroupSpec` undefined.

- [ ] **Step 3: Write `groupversion_info.go`**

```go
// Package v1alpha1 contains the EgressGroup API.
// +kubebuilder:object:generate=true
// +groupName=egress.tailgate.dev
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "egress.tailgate.dev", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() { SchemeBuilder.Register(&EgressGroup{}, &EgressGroupList{}) }
```

- [ ] **Step 4: Write `egressgroup_types.go`** (MVP subset — `subnet`/`cgnat` modes, `kernel` datapath, `routed` attach; 4via6/exit-node fields are stubbed for later phases)

```go
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type EgressMode string

const (
	ModeSubnet EgressMode = "subnet"
	ModeCGNAT  EgressMode = "cgnat"
)

type EgressSelector struct {
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
	// +optional
	PodSelector *metav1.LabelSelector `json:"podSelector,omitempty"`
}

type EgressGroupSpec struct {
	Selector EgressSelector `json:"selector"`

	// +kubebuilder:validation:Enum=subnet;cgnat
	// +kubebuilder:default=cgnat
	Mode EgressMode `json:"mode,omitempty"`

	// +kubebuilder:validation:Enum=kernel;userspace
	// +kubebuilder:default=kernel
	Datapath string `json:"datapath,omitempty"`

	// +kubebuilder:validation:Enum=routed;multus;ebpf;cni-egress;proxy
	// +kubebuilder:default=routed
	Attach string `json:"attach,omitempty"`

	// Routes are advertised subnet CIDRs reachable through this gateway (mode=subnet).
	// CGNAT (100.64.0.0/10) is always reachable and need not be listed.
	// +optional
	Routes []string `json:"routes,omitempty"`

	// Tailnet is the tailnet this group joins (must match operator OAuth scope). Immutable.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="tailnet is immutable"
	Tailnet string `json:"tailnet"`

	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`

	// +optional
	Tags []string `json:"tags,omitempty"` // defaults to ["tag:egress-<name>"]
}

type EgressGroupStatus struct {
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	GatewayHostname string `json:"gatewayHostname,omitempty"`
	// +optional
	AdvertisedRoutes string `json:"advertisedRoutes,omitempty"`
	// +optional
	MatchedPods int32 `json:"matchedPods,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=eg
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Attach",type=string,JSONPath=`.spec.attach`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.matchedPods`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type EgressGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EgressGroupSpec   `json:"spec,omitempty"`
	Status            EgressGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type EgressGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EgressGroup `json:"items"`
}
```

- [ ] **Step 5: Generate deepcopy + CRD manifests**

Run: `make generate manifests`
Expected: `api/v1alpha1/zz_generated.deepcopy.go` and `config/crd/egress.tailgate.dev_egressgroups.yaml` created.

- [ ] **Step 6: Run the test**

Run: `go test ./api/v1alpha1/ -run TestEgressGroup -v` — Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add code/api code/config/crd
git commit -m "feat: EgressGroup CRD (MVP subset: subnet/cgnat, routed attach)"
```

---

## Task 2: tsclient seam (mockable Tailscale API)

**Files:**
- Create: `internal/tsclient/tsclient.go`
- Test: `internal/tsclient/tsclient_test.go`

- [ ] **Step 1: Write the failing test** (asserts the interface shape via a compile-time mock)

```go
package tsclient

import (
	"context"
	"testing"
)

type fake struct{ key string; deleted []string }

func (f *fake) EnsureTag(ctx context.Context, tag, owner string) error { return nil }
func (f *fake) MintAuthKey(ctx context.Context, tags []string) (string, error) { return f.key, nil }
func (f *fake) DeleteDeviceByHostname(ctx context.Context, hostname string) error {
	f.deleted = append(f.deleted, hostname); return nil
}

func TestClientInterfaceSatisfied(t *testing.T) {
	var _ Client = &fake{key: "tskey-abc"}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tsclient/ -v` — Expected: FAIL — `Client` undefined.

- [ ] **Step 3: Write `tsclient.go`** (interface + real impl over `tailscale.com/client/tailscale/v2`)

```go
package tsclient

import (
	"context"
	"fmt"
	"time"

	tsapi "tailscale.com/client/tailscale/v2"
)

// Client is the subset of the Tailscale API tailgate needs. Mockable in tests.
type Client interface {
	EnsureTag(ctx context.Context, tag, ownerTag string) error
	MintAuthKey(ctx context.Context, tags []string) (string, error)
	DeleteDeviceByHostname(ctx context.Context, hostname string) error
}

type apiClient struct{ c *tsapi.Client }

func New(tailnet, oauthClientID, oauthSecret string) Client {
	return &apiClient{c: &tsapi.Client{
		Tailnet: tailnet,
		HTTP:    tsapi.OAuthConfig{ClientID: oauthClientID, ClientSecret: oauthSecret, Scopes: []string{"auth_keys", "devices", "policy_file"}}.HTTPClient(),
	}}
}

func (a *apiClient) MintAuthKey(ctx context.Context, tags []string) (string, error) {
	key, err := a.c.Keys().Create(ctx, tsapi.CreateKeyRequest{
		Capabilities: tsapi.KeyCapabilities{Devices: tsapi.KeyDeviceCapabilities{Create: tsapi.KeyDeviceCreateCapabilities{
			Ephemeral: true, Preauthorized: true, Tags: tags,
		}}},
		ExpirySeconds: int64((24 * time.Hour).Seconds()),
	})
	if err != nil {
		return "", fmt.Errorf("create authkey: %w", err)
	}
	return key.Key, nil
}

func (a *apiClient) DeleteDeviceByHostname(ctx context.Context, hostname string) error {
	devs, err := a.c.Devices().List(ctx)
	if err != nil {
		return err
	}
	for _, d := range devs {
		if d.Hostname == hostname {
			return a.c.Devices().Delete(ctx, d.ID)
		}
	}
	return nil // already gone
}

// EnsureTag is a no-op in MVP: tag owners are managed in the policy file out of band.
// Phase-2 will implement the HuJSON read-modify-write (see DESIGN.md §3.1 tailnit ensureTag).
func (a *apiClient) EnsureTag(ctx context.Context, tag, ownerTag string) error { return nil }
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/tsclient/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add code/internal/tsclient
git commit -m "feat: tsclient seam over Tailscale API (mockable)"
```

> Note: verify `tsapi.CreateKeyRequest`/`KeyCapabilities` field names against the pinned `tailscale.com/client/tailscale/v2` version before Step 3; the shape above matches v2 as of v1.86. If the OAuth helper differs, adapt `New`.

---

## Task 3: authkey mint → Secret

**Files:**
- Create: `internal/controller/authkey.go`
- Test: `internal/controller/authkey_test.go`

- [ ] **Step 1: Write the failing test** (mock tsclient + fake k8s client)

```go
package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type mockTS struct{ minted [][]string }
func (m *mockTS) EnsureTag(ctx context.Context, tag, owner string) error { return nil }
func (m *mockTS) MintAuthKey(ctx context.Context, tags []string) (string, error) {
	m.minted = append(m.minted, tags); return "tskey-xyz", nil
}
func (m *mockTS) DeleteDeviceByHostname(ctx context.Context, h string) error { return nil }

func TestEnsureAuthKeySecret(t *testing.T) {
	k := fake.NewClientBuilder().Build()
	m := &mockTS{}
	r := &EgressGroupReconciler{Client: k, TS: m, Namespace: "tailgate-system"}
	if err := r.ensureAuthKeySecret(context.Background(), "payments", []string{"tag:egress-payments"}); err != nil {
		t.Fatal(err)
	}
	var s corev1.Secret
	if err := k.Get(context.Background(), types.NamespacedName{Name: "tailgate-payments-authkey", Namespace: "tailgate-system"}, &s); err != nil {
		t.Fatalf("secret not created: %v", err)
	}
	if string(s.Data["TS_AUTHKEY"]) != "tskey-xyz" {
		t.Fatalf("wrong key: %q", s.Data["TS_AUTHKEY"])
	}
	// Idempotent: second call must not mint again.
	_ = r.ensureAuthKeySecret(context.Background(), "payments", []string{"tag:egress-payments"})
	if len(m.minted) != 1 {
		t.Fatalf("expected 1 mint, got %d", len(m.minted))
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run TestEnsureAuthKey -v` — Expected: FAIL — `EgressGroupReconciler` undefined.

- [ ] **Step 3: Write `authkey.go`**

```go
package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func authKeySecretName(group string) string { return "tailgate-" + group + "-authkey" }

// ensureAuthKeySecret mints a per-group authkey once and stores it in a Secret.
// Idempotent: if the Secret already has a non-empty TS_AUTHKEY, it does nothing.
func (r *EgressGroupReconciler) ensureAuthKeySecret(ctx context.Context, group string, tags []string) error {
	name := authKeySecretName(group)
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.Namespace}, &existing)
	if err == nil && len(existing.Data["TS_AUTHKEY"]) > 0 {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	key, err := r.TS.MintAuthKey(ctx, tags)
	if err != nil {
		return fmt.Errorf("mint authkey for %q: %w", group, err)
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace},
		Data:       map[string][]byte{"TS_AUTHKEY": []byte(key)},
	}
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, s)
	}
	existing.Data = s.Data
	return r.Update(ctx, &existing)
}
```

- [ ] **Step 4: Add the reconciler struct stub** to `internal/controller/egressgroup_controller.go` so the test compiles:

```go
package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"raj/personal/code/internal/tsclient"
)

type EgressGroupReconciler struct {
	client.Client
	TS        tsclient.Client
	Namespace string
}
```

- [ ] **Step 5: Run the test**

Run: `go test ./internal/controller/ -run TestEnsureAuthKey -v` — Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add code/internal/controller/authkey.go code/internal/controller/egressgroup_controller.go code/internal/controller/authkey_test.go
git commit -m "feat: per-group OAuth authkey mint into Secret (idempotent)"
```

---

## Task 4: gateway Deployment + Service builders

**Files:**
- Create: `internal/controller/gateway.go`
- Test: `internal/controller/gateway_test.go`

- [ ] **Step 1: Write the failing test**

```go
package controller

import (
	"testing"
	egressv1 "raj/personal/code/api/v1alpha1"
)

func TestGatewayDeployment(t *testing.T) {
	eg := &egressv1.EgressGroup{}
	eg.Name = "payments"
	eg.Spec = egressv1.EgressGroupSpec{Mode: egressv1.ModeSubnet, Routes: []string{"10.50.0.0/16"}, Replicas: 2}
	d := gatewayDeployment(eg, "tailgate-system", "tailscale/tailscale:v1.86.0", "tailgate-gateway:dev")
	if *d.Spec.Replicas != 2 {
		t.Fatalf("replicas = %d", *d.Spec.Replicas)
	}
	c := d.Spec.Template.Spec.Containers[0]
	caps := c.SecurityContext.Capabilities.Add
	if !hasCap(caps, "NET_ADMIN") {
		t.Fatal("gateway must have NET_ADMIN")
	}
	if envVal(c.Env, "TS_ROUTES") != "" {
		t.Fatal("MVP SNAT mode must NOT advertise pod routes (TS_ROUTES empty); source-preserve is a later phase")
	}
	if envVal(c.Env, "TS_EXTRA_ARGS") == "" {
		t.Fatal("gateway must accept routes to reach subnet targets")
	}
}
```

(`hasCap`/`envVal` are tiny helpers — define them at the bottom of the test file.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run TestGatewayDeployment -v` — Expected: FAIL — `gatewayDeployment` undefined.

- [ ] **Step 3: Write `gateway.go`**

```go
package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	egressv1 "raj/personal/code/api/v1alpha1"
)

func gatewayName(group string) string { return "tailgate-gw-" + group }

func gatewayLabels(group string) map[string]string {
	return map[string]string{"app.kubernetes.io/managed-by": "tailgate", "tailgate.dev/group": group}
}

func gatewayDeployment(eg *egressv1.EgressGroup, ns, tsImage, gwImage string) *appsv1.Deployment {
	l := gatewayLabels(eg.Name)
	// The gateway runs our entrypoint, which starts tailscaled (kernel TUN), brings the
	// node up as a subnet router that accepts routes, and MASQUERADEs cluster traffic
	// onto tailscale0. MVP = SNAT-to-tag, so we do NOT advertise pod routes (TS_ROUTES empty).
	env := []corev1.EnvVar{
		{Name: "TS_HOSTNAME", Value: gatewayName(eg.Name)},
		{Name: "TS_EXTRA_ARGS", Value: "--accept-routes --advertise-tags=tag:egress-" + eg.Name},
		{Name: "TS_AUTHKEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: authKeySecretName(eg.Name)}, Key: "TS_AUTHKEY"}}},
		{Name: "POD_CIDR_HINT", Value: "100.64.0.0/10"}, // gateway MASQUERADEs anything bound for tailscale0
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName(eg.Name), Namespace: ns, Labels: l},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(eg.Spec.Replicas),
			Selector: &metav1.LabelSelector{MatchLabels: l},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: l},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "gateway",
					Image: gwImage, // our entrypoint; base layer is tsImage
					Env:   env,
					SecurityContext: &corev1.SecurityContext{
						Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN", "NET_RAW"}},
					},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
						Command: []string{"/usr/local/bin/tailgate-gateway", "ready"}}}},
				}}},
			},
		},
	}
}

func gatewayService(eg *egressv1.EgressGroup, ns string) *corev1.Service {
	l := gatewayLabels(eg.Name)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayName(eg.Name), Namespace: ns, Labels: l},
		Spec: corev1.ServiceSpec{
			Selector:              l,
			ClusterIP:             corev1.ClusterIPNone, // headless: members route to it; see injector
			InternalTrafficPolicy: ptr.To(corev1.ServiceInternalTrafficPolicyLocal),
			Ports:                 []corev1.ServicePort{{Name: "wg", Port: 41641, TargetPort: intstr.FromInt(41641)}},
		},
	}
}
```

- [ ] **Step 4: Run the test**

Run: `go test ./internal/controller/ -run TestGatewayDeployment -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add code/internal/controller/gateway.go code/internal/controller/gateway_test.go
git commit -m "feat: gateway Deployment + headless Service builders (kernel-TUN, NET_ADMIN, SNAT-to-tag)"
```

---

## Task 5: Reconcile loop + finalizer (envtest)

**Files:**
- Modify: `internal/controller/egressgroup_controller.go`
- Test: `internal/controller/egressgroup_controller_test.go`

- [ ] **Step 1: Write the failing envtest**

```go
package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	egressv1 "raj/personal/code/api/v1alpha1"
)

func TestReconcileCreatesGatewayAndSecret(t *testing.T) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd"}}
	cfg, err := env.Start(); if err != nil { t.Fatal(err) }
	defer env.Stop()
	scheme := newScheme(t)
	cl, _ := client.New(cfg, client.Options{Scheme: scheme})
	_ = cl.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tailgate-system"}})

	eg := &egressv1.EgressGroup{ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: egressv1.EgressGroupSpec{Mode: egressv1.ModeSubnet, Routes: []string{"10.50.0.0/16"}, Tailnet: "example.com", Replicas: 2}}
	if err := cl.Create(context.Background(), eg); err != nil { t.Fatal(err) }

	r := &EgressGroupReconciler{Client: cl, TS: &mockTS{}, Namespace: "tailgate-system",
		TSImage: "tailscale/tailscale:v1.86.0", GWImage: "tailgate-gateway:dev"}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "payments"}}); err != nil {
		t.Fatal(err)
	}
	var d appsv1.Deployment
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "tailgate-gw-payments", Namespace: "tailgate-system"}, &d); err != nil {
		t.Fatalf("gateway deployment not created: %v", err)
	}
	var s corev1.Secret
	if err := cl.Get(context.Background(), types.NamespacedName{Name: "tailgate-payments-authkey", Namespace: "tailgate-system"}, &s); err != nil {
		t.Fatalf("authkey secret not created: %v", err)
	}
	_ = time.Second
}
```

(`newScheme` registers corev1, appsv1, and `egressv1` — write it in the test file.)

- [ ] **Step 2: Run to verify it fails**

Run: `make test` (sets `KUBEBUILDER_ASSETS`). Filter: `go test ./internal/controller/ -run TestReconcileCreates -v`
Expected: FAIL — `Reconcile` not implemented / fields `TSImage`/`GWImage` missing.

- [ ] **Step 3: Implement `Reconcile`, `SetupWithManager`, finalizer**

```go
package controller

import (
	"context"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	egressv1 "raj/personal/code/api/v1alpha1"
	"raj/personal/code/internal/tsclient"
)

const finalizer = "tailgate.dev/finalizer"

type EgressGroupReconciler struct {
	client.Client
	TS                tsclient.Client
	Namespace         string
	TSImage, GWImage  string
}

func tagsFor(eg *egressv1.EgressGroup) []string {
	if len(eg.Spec.Tags) > 0 {
		return eg.Spec.Tags
	}
	return []string{"tag:egress-" + eg.Name}
}

func (r *EgressGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	var eg egressv1.EgressGroup
	if err := r.Get(ctx, req.NamespacedName, &eg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !eg.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&eg, finalizer) {
			if err := r.TS.DeleteDeviceByHostname(ctx, gatewayName(eg.Name)); err != nil {
				return ctrl.Result{}, err // requeue
			}
			controllerutil.RemoveFinalizer(&eg, finalizer)
			return ctrl.Result{}, r.Update(ctx, &eg)
		}
		return ctrl.Result{}, nil
	}

	if controllerutil.AddFinalizer(&eg, finalizer) {
		if err := r.Update(ctx, &eg); err != nil {
			return ctrl.Result{}, err
		}
	}

	tags := tagsFor(&eg)
	if err := r.TS.EnsureTag(ctx, tags[0], "tag:tailgate"); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureAuthKeySecret(ctx, eg.Name, tags); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyObject(ctx, gatewayService(&eg, r.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyObject(ctx, gatewayDeployment(&eg, r.Namespace, r.TSImage, r.GWImage)); err != nil {
		return ctrl.Result{}, err
	}

	eg.Status.GatewayHostname = gatewayName(eg.Name)
	eg.Status.AdvertisedRoutes = strings.Join(eg.Spec.Routes, ",")
	if err := r.Status().Update(ctx, &eg); err != nil {
		return ctrl.Result{}, err
	}
	l.Info("reconciled", "group", eg.Name)
	return ctrl.Result{}, nil
}

// applyObject is a create-or-update that stamps the owner reference for GC.
func (r *EgressGroupReconciler) applyObject(ctx context.Context, obj client.Object) error {
	existing := obj.DeepCopyObject().(client.Object)
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	return r.Update(ctx, obj)
}

func (r *EgressGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&egressv1.EgressGroup{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Named("egressgroup").
		Complete(r)
}

var _ = runtime.Object(nil) // keep runtime import if unused elsewhere
```

(Set ownerRefs on the gateway/service/secret in their builders via `controllerutil.SetControllerReference(&eg, obj, scheme)` — add that call inside `applyObject` by threading the scheme, or set it in the builders. For brevity, add `controllerutil.SetControllerReference` in `applyObject` using `r.Scheme()` once the manager wires the scheme.)

- [ ] **Step 4: Run the envtest**

Run: `go test ./internal/controller/ -run TestReconcileCreates -v` (with `KUBEBUILDER_ASSETS` set) — Expected: PASS.

- [ ] **Step 4b: E2E gate (kind, tier C — first in-cluster operator run)**

This is where `test/e2e/main_test.go` (Task 0b) starts building+loading the operator image and installing it once (Helm or `kubectl apply config/default`). Add `test/e2e/reconcile_test.go` (`//go:build e2e`): apply a real `EgressGroup` and assert the gateway Deployment reaches ≥1 ready replica.

```go
//go:build e2e
func TestGatewayDeploymentComesUp(t *testing.T) {
	eg := &egressv1.EgressGroup{ObjectMeta: metav1.ObjectMeta{Name: "e2e-gw"},
		Spec: egressv1.EgressGroupSpec{Mode: egressv1.ModeCGNAT, Tailnet: tailnet(), Replicas: 1}}
	must(t, cluster.Kube.Create(context.Background(), eg))
	t.Cleanup(func() { _ = cluster.Kube.Delete(context.Background(), eg) })
	cluster.WaitDeploymentReady(t, "tailgate-system", "tailgate-gw-e2e-gw") // numberReady>=1
}
```

Run: `make e2e`. Expected: PASS — the in-cluster operator reconciles the EG into a gateway Deployment that schedules to Ready. (Gateway reaching *tailnet* Ready is gated behind `TS_AUTHKEY`; without it, assert the Deployment exists + pod scheduled. `WaitDeploymentReady` mirrors `WaitPodReady`.)

- [ ] **Step 5: Commit**

```bash
git add code/internal/controller code/test/e2e/reconcile_test.go
git commit -m "feat: EgressGroup reconcile — gateway + secret + finalizer (device cleanup) + e2e gateway-up"
```

---

## Task 6: gateway entrypoint (`tailgate-gateway`)

**Files:**
- Create: `cmd/tailgate-gateway/main.go`
- Test: `cmd/tailgate-gateway/main_test.go`

- [ ] **Step 1: Write the failing test** (unit-test the iptables/sysctl arg construction, not the live network)

```go
package main

import "testing"

func TestMasqueradeRule(t *testing.T) {
	got := masqueradeArgs("tailscale0")
	want := "-t nat -A POSTROUTING -o tailscale0 -j MASQUERADE"
	if join(got) != want {
		t.Fatalf("got %q want %q", join(got), want)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/tailgate-gateway/ -v` — Expected: FAIL — `masqueradeArgs` undefined.

- [ ] **Step 3: Write `main.go`**

```go
// tailgate-gateway starts tailscaled in kernel-TUN mode, brings the node up as a
// route-accepting subnet router, enables IP forwarding, and MASQUERADEs cluster
// traffic onto tailscale0 (SNAT-to-tag). `tailgate-gateway ready` is the readiness probe.
package main

import (
	"os"
	"os/exec"
	"strings"
)

func masqueradeArgs(dev string) []string {
	return []string{"-t", "nat", "-A", "POSTROUTING", "-o", dev, "-j", "MASQUERADE"}
}
func join(a []string) string { return strings.Join(a, " ") }

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "ready" {
		// ready iff tailscale reports a self IP
		if err := run("tailscale", "status", "--json"); err != nil {
			os.Exit(1)
		}
		return
	}
	must(run("sysctl", "-w", "net.ipv4.ip_forward=1"))
	must(run("sysctl", "-w", "net.ipv6.conf.all.forwarding=1"))
	// tailscaled in the background (kernel TUN). TS_AUTHKEY/TS_EXTRA_ARGS come from env.
	go func() { _ = run("tailscaled", "--state=mem:", "--tun=tailscale0", "--socket=/var/run/tailscale/tailscaled.sock") }()
	must(run("tailscale", "up", "--authkey="+os.Getenv("TS_AUTHKEY"), "--hostname="+os.Getenv("TS_HOSTNAME"), "--accept-routes"))
	must(run("iptables", masqueradeArgs("tailscale0")...))
	must(run("ip6tables", masqueradeArgs("tailscale0")...))
	select {} // block forever
}

func must(err error) { if err != nil { panic(err) } }
```

- [ ] **Step 4: Run the test**

Run: `go test ./cmd/tailgate-gateway/ -v` — Expected: PASS.

- [ ] **Step 5: Create the gateway Dockerfile** `cmd/tailgate-gateway/Dockerfile`:

```dockerfile
FROM tailscale/tailscale:v1.86.0
COPY bin/tailgate-gateway /usr/local/bin/tailgate-gateway
ENTRYPOINT ["/usr/local/bin/tailgate-gateway"]
```

- [ ] **Step 6: Commit**

```bash
git add code/cmd/tailgate-gateway
git commit -m "feat: tailgate-gateway entrypoint (kernel TUN + accept-routes + MASQUERADE)"
```

---

## Task 7: netinfo handoff (shared CNI↔agent contract)

**Files:**
- Create: `internal/netinfo/netinfo.go`
- Test: `internal/netinfo/netinfo_test.go`

- [ ] **Step 1: Write the failing test**

```go
package netinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRead(t *testing.T) {
	dir := t.TempDir()
	Dir = dir
	in := PodNetInfo{PodIP: "10.0.1.5", Netns: "/proc/123/ns/net", IfName: "eth0"}
	if err := Write(in); err != nil { t.Fatal(err) }
	if _, err := os.Stat(filepath.Join(dir, "10.0.1.5")); err != nil { t.Fatalf("file not written: %v", err) }
	out, err := Read("10.0.1.5"); if err != nil { t.Fatal(err) }
	if out.Netns != in.Netns || out.IfName != in.IfName { t.Fatalf("mismatch: %+v", out) }
	if err := Remove("10.0.1.5"); err != nil { t.Fatal(err) }
	if _, err := Read("10.0.1.5"); err == nil { t.Fatal("expected read-after-remove to fail") }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/netinfo/ -v` — Expected: FAIL — undefined.

- [ ] **Step 3: Write `netinfo.go`**

```go
// Package netinfo is the CNI↔agent contract: the CNI plugin records each pod's
// netns path keyed by PodIP; the agent reads it to inject routes into member pods.
package netinfo

import (
	"encoding/json"
	"os"
	"path/filepath"
)

var Dir = "/var/run/tailgate"

type PodNetInfo struct {
	PodIP  string `json:"podIP"`
	Netns  string `json:"netns"`
	IfName string `json:"ifName"`
}

func Write(n PodNetInfo) error {
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(Dir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close(); return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), filepath.Join(Dir, n.PodIP)) // atomic
}

func Read(podIP string) (PodNetInfo, error) {
	var n PodNetInfo
	b, err := os.ReadFile(filepath.Join(Dir, podIP))
	if err != nil {
		return n, err
	}
	return n, json.Unmarshal(b, &n)
}

func Remove(podIP string) error {
	err := os.Remove(filepath.Join(Dir, podIP))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
```

- [ ] **Step 4: Run the test** — Run: `go test ./internal/netinfo/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add code/internal/netinfo
git commit -m "feat: netinfo CNI↔agent handoff (atomic write, podIP-keyed)"
```

---

## Task 8: route-only CNI plugin

**Files:**
- Create: `internal/cni/route.go`, `cmd/tailgate-cni/main.go`
- Test: `internal/cni/route_test.go`

- [ ] **Step 1: Write the failing test** (parse prevResult → PodIP, write netinfo)

```go
package cni

import (
	"testing"
	"raj/personal/code/internal/netinfo"
)

func TestExtractIPv4(t *testing.T) {
	pr := `{"cniVersion":"1.0.0","ips":[{"address":"10.0.1.5/24"}]}`
	ip, err := extractIPv4(pr)
	if err != nil { t.Fatal(err) }
	if ip != "10.0.1.5" { t.Fatalf("got %q", ip) }
}

func TestRecordNetinfo(t *testing.T) {
	netinfo.Dir = t.TempDir()
	if err := record("10.0.1.5", "/proc/9/ns/net", "eth0"); err != nil { t.Fatal(err) }
	got, _ := netinfo.Read("10.0.1.5")
	if got.Netns != "/proc/9/ns/net" { t.Fatalf("%+v", got) }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cni/ -v` — Expected: FAIL — undefined.

- [ ] **Step 3: Write `route.go`** (CNI logic; `cmdAdd` records netns + re-emits prevResult — *no veth*, route inject is the agent's job so it can gate on membership)

```go
package cni

import (
	"encoding/json"
	"fmt"

	"github.com/containernetworking/cni/pkg/skel"
	cnitypes "github.com/containernetworking/cni/pkg/types"
	current "github.com/containernetworking/cni/pkg/types/100"
	"raj/personal/code/internal/netinfo"
)

type netConf struct {
	cnitypes.NetConf
}

func extractIPv4(prevResultJSON string) (string, error) {
	var r current.Result
	if err := json.Unmarshal([]byte(prevResultJSON), &r); err != nil {
		return "", err
	}
	for _, ip := range r.IPs {
		if ip.Address.IP.To4() != nil {
			return ip.Address.IP.String(), nil
		}
	}
	return "", fmt.Errorf("no IPv4 in prevResult")
}

func record(podIP, netns, ifName string) error {
	return netinfo.Write(netinfo.PodNetInfo{PodIP: podIP, Netns: netns, IfName: ifName})
}

func CmdAdd(args *skel.CmdArgs) error {
	var conf netConf
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return err
	}
	if conf.PrevResult == nil {
		return fmt.Errorf("tailgate-cni must be chained (no prevResult)")
	}
	prevJSON, _ := json.Marshal(conf.PrevResult)
	ip, err := extractIPv4(string(prevJSON))
	if err != nil {
		// No IPv4 (host-network pods etc.) — pass through untouched.
		return cnitypes.PrintResult(conf.PrevResult, conf.CNIVersion)
	}
	if err := record(ip, args.Netns, args.IfName); err != nil {
		return err
	}
	return cnitypes.PrintResult(conf.PrevResult, conf.CNIVersion)
}

func CmdDel(args *skel.CmdArgs) error {
	var conf netConf
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return err
	}
	if conf.PrevResult == nil {
		return nil
	}
	prevJSON, _ := json.Marshal(conf.PrevResult)
	if ip, err := extractIPv4(string(prevJSON)); err == nil {
		_ = netinfo.Remove(ip)
	}
	return nil
}

func CmdCheck(args *skel.CmdArgs) error {
	var conf netConf
	if err := json.Unmarshal(args.StdinData, &conf); err != nil {
		return err
	}
	if conf.PrevResult == nil {
		return nil
	}
	return cnitypes.PrintResult(conf.PrevResult, conf.CNIVersion)
}
```

- [ ] **Step 4: Write `cmd/tailgate-cni/main.go`**

```go
package main

import (
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"
	"raj/personal/code/internal/cni"
)

func main() {
	skel.PluginMainFuncs(skel.CNIFuncs{Add: cni.CmdAdd, Del: cni.CmdDel, Check: cni.CmdCheck},
		version.All, "tailgate-cni")
}
```

- [ ] **Step 5: Run the test** — Run: `go test ./internal/cni/ -v` — Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add code/internal/cni code/cmd/tailgate-cni
git commit -m "feat: route-only chained CNI plugin (records netns; route inject is agent-side)"
```

---

## Task 9: agent route injector (member pods only)

**Files:**
- Create: `internal/agent/injector.go`
- Test: `internal/agent/injector_test.go` (real netns — **requires CAP_NET_ADMIN**; gate with a build tag)

- [ ] **Step 1: Write the failing test** (build-tagged `//go:build linux && privileged`)

```go
//go:build linux && privileged

package agent

import (
	"net"
	"testing"
	"github.com/vishvananda/netns"
	"github.com/vishvananda/netlink"
)

func TestInjectRoutesInNetns(t *testing.T) {
	orig, _ := netns.Get(); defer orig.Close()
	tmp, _ := netns.New(); defer tmp.Close() // fresh netns
	netns.Set(orig)
	gw := net.ParseIP("169.254.1.1")
	if err := injectRoutes(tmp, []string{"100.64.0.0/10"}, gw, "lo"); err != nil {
		t.Fatal(err)
	}
	netns.Set(tmp); defer netns.Set(orig)
	routes, _ := netlink.RouteList(nil, netlink.FAMILY_V4)
	found := false
	for _, r := range routes {
		if r.Dst != nil && r.Dst.String() == "100.64.0.0/10" { found = true }
	}
	if !found { t.Fatal("CGNAT route not injected") }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `sudo -E go test -tags 'linux privileged' ./internal/agent/ -run TestInjectRoutes -v` — Expected: FAIL — `injectRoutes` undefined.

- [ ] **Step 3: Write `injector.go`**

```go
package agent

import (
	"net"
	"runtime"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// injectRoutes enters podNS and RouteReplaces each tailnet CIDR via gw on dev.
// RouteReplace is idempotent (no EEXIST on retry). Runs locked to the OS thread
// because setns(2) is per-thread.
func injectRoutes(podNS netns.NsHandle, cidrs []string, gw net.IP, dev string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		return err
	}
	defer netns.Set(orig)
	if err := netns.Set(podNS); err != nil {
		return err
	}
	link, err := netlink.LinkByName(dev)
	if err != nil {
		return err
	}
	for _, c := range cidrs {
		_, dst, err := net.ParseCIDR(c)
		if err != nil {
			return err
		}
		r := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: dst, Gw: gw}
		if err := netlink.RouteReplace(r); err != nil {
			return err
		}
	}
	return nil
}

func removeRoutes(podNS netns.NsHandle, cidrs []string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, _ := netns.Get()
	defer netns.Set(orig)
	if err := netns.Set(podNS); err != nil {
		return err
	}
	for _, c := range cidrs {
		_, dst, err := net.ParseCIDR(c)
		if err != nil {
			continue
		}
		_ = netlink.RouteDel(&netlink.Route{Dst: dst})
	}
	return nil
}
```

- [ ] **Step 4: Run the test**

Run: `sudo -E go test -tags 'linux privileged' ./internal/agent/ -run TestInjectRoutes -v` — Expected: PASS. (In CI without privileges, this test is skipped by the build tag; document that e2e (Task 12) covers it.)

- [ ] **Step 4b: E2E gate (kind, tier D — route in a member pod netns)**

Add `test/e2e/inject_test.go` (`//go:build e2e`): with the agent DaemonSet installed, create a member busybox pod labeled into a group, assert the CGNAT route landed in its netns; a non-member (no label) must NOT have it.

```go
//go:build e2e
func TestRouteInjectedForMemberOnly(t *testing.T) {
	ns := cluster.NewNamespace(t, "inject")
	applyEgressGroup(t, "inj", map[string]string{"egress": "inj"}) // selects egress=inj
	gwIP := gatewayClusterIP(t, "inj")

	member := newBusybox(ns, "member", map[string]string{"egress": "inj"})
	must(t, cluster.Kube.Create(context.Background(), member))
	cluster.WaitPodReady(t, ns, "member")
	cluster.AssertRouteInNetns(t, ns, "member", "100.64.0.0/10", gwIP) // injected

	other := newBusybox(ns, "other", map[string]string{"app": "web"})
	must(t, cluster.Kube.Create(context.Background(), other))
	cluster.WaitPodReady(t, ns, "other")
	cluster.AssertNoRouteInNetns(t, ns, "other", "100.64.0.0/10") // non-member: absent
}
```

Run: `make e2e`. Expected: PASS — the agent's informer saw the member pod, read its `netinfo`, and `injectRoutes` landed `100.64.0.0/10 via <gwIP>` in the netns; the non-member has no such route. (`AssertNoRouteInNetns` is the negative of `AssertRouteInNetns`.)

- [ ] **Step 5: Commit**

```bash
git add code/internal/agent/injector.go code/internal/agent/injector_test.go code/test/e2e/inject_test.go
git commit -m "feat: agent route injector (RouteReplace into member pod netns, LockOSThread) + e2e route-in-netns"
```

---

## Task 10: agent catalogue (informer → membership) + reconcile

**Files:**
- Create: `internal/agent/catalogue.go`
- Test: `internal/agent/catalogue_test.go`

- [ ] **Step 1: Write the failing test** (pure membership logic, no informer)

```go
package agent

import (
	"testing"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	egressv1 "raj/personal/code/api/v1alpha1"
)

func TestMatchGroup(t *testing.T) {
	groups := []egressv1.EgressGroup{{
		ObjectMeta: metav1.ObjectMeta{Name: "payments"},
		Spec: egressv1.EgressGroupSpec{Selector: egressv1.EgressSelector{
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"egress": "payments"}}}},
	}}
	member := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"egress": "payments"}}}
	nonmember := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "web"}}}
	if g := matchGroup(member, groups); g != "payments" { t.Fatalf("member → %q", g) }
	if g := matchGroup(nonmember, groups); g != "" { t.Fatalf("nonmember → %q", g) }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agent/ -run TestMatchGroup -v` — Expected: FAIL — `matchGroup` undefined.

- [ ] **Step 3: Write `catalogue.go`** (membership match + the route set for a group)

```go
package agent

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	egressv1 "raj/personal/code/api/v1alpha1"
)

// matchGroup returns the first group whose podSelector matches the pod, else "".
func matchGroup(pod *corev1.Pod, groups []egressv1.EgressGroup) string {
	for _, g := range groups {
		sel := g.Spec.Selector.PodSelector
		if sel == nil {
			continue
		}
		s, err := metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			continue
		}
		if s.Matches(labels.Set(pod.Labels)) {
			return g.Name
		}
	}
	return ""
}

// routeSet is the tailnet CIDRs a member of group g must route to its gateway.
func routeSet(g egressv1.EgressGroup) []string {
	out := []string{"100.64.0.0/10", "fd7a:115c:a1e0::/48"} // CGNAT + ULA always
	out = append(out, g.Spec.Routes...)                      // advertised subnet CIDRs
	return out
}
```

- [ ] **Step 4: Run the test** — Run: `go test ./internal/agent/ -run TestMatchGroup -v` — Expected: PASS.

- [ ] **Step 5: Write `cmd/tailgate-agent/main.go`** — wire it together (informers for Pods + EgressGroups; on member-pod ready, look up `netinfo.Read(podIP)`, resolve the gateway ClusterIP for the group, `injectRoutes`; on delete, `removeRoutes`). Then install the CNI (Task 11). This is glue — no new logic, so no separate test; covered by e2e (Task 12).

```go
package main

import (
	"context"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"raj/personal/code/internal/agent"
)

func main() {
	cfg, err := rest.InClusterConfig(); if err != nil { panic(err) }
	cs, _ := kubernetes.NewForConfig(cfg)
	a := agent.New(cs, os.Getenv("TAILGATE_NAMESPACE"))
	if err := a.InstallCNI(); err != nil { panic(err) } // Task 11
	a.Run(context.Background()) // informers + inject/remove loop; blocks
}
```

(`agent.New`/`Run`/`InstallCNI` are implemented in `internal/agent`; `Run` builds shared informers, watches Pod add/update/delete, and calls `injectRoutes`/`removeRoutes` for members using `netinfo` + the group's gateway ClusterIP. Keep `Run` thin — the testable units are `matchGroup`, `routeSet`, `injectRoutes` above.)

- [ ] **Step 6: Commit**

```bash
git add code/internal/agent/catalogue.go code/internal/agent/catalogue_test.go code/cmd/tailgate-agent
git commit -m "feat: agent catalogue (selector→group) + member route reconcile loop"
```

---

## Task 11: CNI installer + reconcile loop

**Files:**
- Create: `internal/agent/installer.go`
- Test: `internal/agent/installer_test.go`

- [ ] **Step 1: Write the failing test** (conflist append is idempotent + non-lexical selection)

```go
package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPatchConflistIdempotent(t *testing.T) {
	dir := t.TempDir()
	conf := `{"cniVersion":"1.0.0","name":"calico","plugins":[{"type":"calico"}]}`
	os.WriteFile(filepath.Join(dir, "10-calico.conflist"), []byte(conf), 0o644)

	if err := patchConflist(dir); err != nil { t.Fatal(err) }
	if err := patchConflist(dir); err != nil { t.Fatal(err) } // second call

	b, _ := os.ReadFile(filepath.Join(dir, "10-calico.conflist"))
	var parsed struct{ Plugins []map[string]any `json:"plugins"` }
	json.Unmarshal(b, &parsed)
	count := 0
	for _, p := range parsed.Plugins { if p["type"] == "tailgate-cni" { count++ } }
	if count != 1 { t.Fatalf("expected exactly 1 tailgate-cni entry, got %d", count) }
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agent/ -run TestPatchConflist -v` — Expected: FAIL — `patchConflist` undefined.

- [ ] **Step 3: Write `installer.go`** (atomic, idempotent, non-lexical selection = pick the conflist the running CNI owns by checking which file the kubelet config references; MVP heuristic: lowest-numbered `*.conflist` whose first plugin `type` is a known primary, falling back to lexical with a logged warning)

```go
package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ConfDir = "/host/etc/cni/net.d"

const pluginType = "tailgate-cni"

func selectConflist(dir string) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var lists []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".conflist") {
			lists = append(lists, e.Name())
		}
	}
	if len(lists) == 0 {
		return "", fmt.Errorf("no .conflist in %s (Cilium-no-chaining / GKE-DPv2? use a non-routed attach backend)", dir)
	}
	sort.Strings(lists) // lowest-numbered first; primary CNIs use 10-/05- prefixes
	return filepath.Join(dir, lists[0]), nil
}

func patchConflist(dir string) error {
	path, err := selectConflist(dir)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	plugins, _ := doc["plugins"].([]any)
	for _, p := range plugins {
		if m, ok := p.(map[string]any); ok && m["type"] == pluginType {
			return nil // already chained — idempotent
		}
	}
	doc["plugins"] = append(plugins, map[string]any{"type": pluginType})
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp.Write(out)
	tmp.Close()
	return os.Rename(tmp.Name(), path) // atomic same-dir swap
}
```

- [ ] **Step 4: Add `InstallCNI` + reconcile loop to the agent** (copy the `tailgate-cni` binary into `/host/opt/cni/bin`, then `patchConflist`, then re-patch every 30s to survive control-plane conflist rewrites; set a readiness flag the DaemonSet probe reads). Sketch:

```go
// in installer.go
func (a *Agent) InstallCNI() error {
	if err := copyBinary("/usr/local/bin/tailgate-cni", "/host/opt/cni/bin/tailgate-cni"); err != nil {
		return err
	}
	if err := patchConflist(ConfDir); err != nil {
		return err
	}
	a.cniReady.Store(true)
	go a.repatchLoop() // re-assert every 30s; logs+continues on EROFS
	return nil
}
```

(`copyBinary` is an atomic temp+rename+chmod 0755 with EROFS tolerance, mirroring tailnit's installer. `repatchLoop` ticks 30s and calls `patchConflist`, logging on error. `a.cniReady` is an `atomic.Bool` exposed via an HTTP `/readyz` the DaemonSet readiness probe hits — so a read-only-rootfs silent-no-attach fails the probe instead of looking healthy.)

- [ ] **Step 5: Run the test** — Run: `go test ./internal/agent/ -run TestPatchConflist -v` — Expected: PASS.

- [ ] **Step 5b: E2E gate (kind, tier C — install + repatch on the node)**

Add `test/e2e/installer_test.go` (`//go:build e2e`): assert the agent DaemonSet is Ready on every node, the binary is on the node, the conflist is chained, and the repatch loop restores a stripped entry.

```go
//go:build e2e
func TestCNIInstalledAndRepatched(t *testing.T) {
	cluster.WaitDaemonSetReady(t, "tailgate-system", "tailgate-agent")
	node := "tailgate-e2e-worker"
	out, err := cluster.NodeExec(t, node, "ls", "/opt/cni/bin/tailgate-cni") // docker exec <node>
	must(t, err); _ = out
	cluster.AssertConflistChained(t, node) // grep tailgate-cni in /etc/cni/net.d/*.conflist
	// repatch: strip the entry out-of-band; the agent's 30s loop must re-add it
	_, _ = cluster.NodeExec(t, node, "sh", "-c", `sed -i 's/"tailgate-cni"//' /etc/cni/net.d/*.conflist`)
	cluster.EventuallyConflistChained(t, node) // bounded wait
}
```

Run: `make e2e`. Expected: PASS — binary installed on each node, conflist chained (at `cniVersion 0.3.1` to match kindnet), and a stripped entry restored within the reconcile window. (`NodeExec`/`AssertConflistChained`/`EventuallyConflistChained` are framework helpers over `docker exec <kindnode>`.)

- [ ] **Step 6: Commit**

```bash
git add code/internal/agent/installer.go code/internal/agent/installer_test.go code/test/e2e/installer_test.go
git commit -m "feat: agent CNI installer — atomic idempotent conflist patch + repatch loop + readyz + e2e"
```

---

## Task 12: deploy manifests + kind e2e

**Files:**
- Create: `deploy/operator.yaml`, `deploy/agent-daemonset.yaml`, `deploy/rbac.yaml`, `cmd/tailgate-operator/main.go`
- Test: `test/e2e/egress_test.go`

- [ ] **Step 1: Write `cmd/tailgate-operator/main.go`** (manager wiring)

```go
package main

import (
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	egressv1 "raj/personal/code/api/v1alpha1"
	"raj/personal/code/internal/controller"
	"raj/personal/code/internal/tsclient"
)

func main() {
	ctrl.SetLogger(zap.New())
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = egressv1.AddToScheme(scheme)
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: scheme, LeaderElection: true, LeaderElectionID: "tailgate"})
	if err != nil { panic(err) }
	ts := tsclient.New(os.Getenv("TS_TAILNET"), os.Getenv("TS_OAUTH_CLIENT_ID"), os.Getenv("TS_OAUTH_SECRET"))
	r := &controller.EgressGroupReconciler{Client: mgr.GetClient(), TS: ts,
		Namespace: os.Getenv("POD_NAMESPACE"), TSImage: os.Getenv("TS_IMAGE"), GWImage: os.Getenv("GW_IMAGE")}
	if err := r.SetupWithManager(mgr); err != nil { panic(err) }
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil { panic(err) }
}
```

- [ ] **Step 2: Write the deploy manifests** — operator Deployment (1 replica, leader-elected), agent DaemonSet (`hostNetwork: true`, `privileged`, mounts `/opt/cni/bin`→`/host/opt/cni/bin`, `/etc/cni/net.d`→`/host/etc/cni/net.d`, `/var/run/netns` ro, `/var/run/tailgate`), RBAC (ClusterRole: `egressgroups` all verbs + `pods` get/list/watch + `deployments/services/secrets` in operator ns; agent: `egressgroups`+`pods` list/watch). OAuth creds from a Secret.

- [ ] **Step 3: Write the e2e** (`//go:build e2e`, tier D, full datapath) — uses the Task 0b framework + a **tsnet ephemeral peer** as the CGNAT target (per the e2e research: lightest reliable real target), gated behind `TS_AUTHKEY`. Multi-node kind + pod anti-affinity forces member and gateway onto different nodes (cross-node topology-d).

```go
//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"os"
	"testing"

	"tailscale.com/tsnet"
)

func TestPodReachesCGNATPeerCrossNode(t *testing.T) {
	if tnet == nil { // package-level *tailnet.Ephemeral, created in TestMain when org creds/OIDC exist
		t.Skip("no ephemeral tailnet (no org creds/OIDC) — datapath test skipped; route-presence is covered by Task 9")
	}
	// 1. Mint an authkey on the per-run ephemeral tailnet, stand up an ephemeral tsnet
	//    peer as the CGNAT target on it. Both the peer and the tailnet self-clean.
	key, err := tnet.MintAuthKey(context.Background(), []string{"tag:ci"})
	must(t, err)
	srv := &tsnet.Server{Hostname: "tailgate-e2e-target", Ephemeral: true, AuthKey: key}
	defer srv.Close()
	ln, err := srv.Listen("tcp", ":80")
	must(t, err)
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	peer := selfCGNAT(t, srv) // the 100.x address the peer registered

	ns := cluster.NewNamespace(t, "egress")
	applyEgressGroup(t, "e2e", map[string]string{"egress": "e2e"})

	// 2. Member pod, pinned (anti-affinity) to a different node than the gateway.
	member := newCurlPod(ns, "probe", map[string]string{"egress": "e2e"}, antiAffinityFrom("e2e"))
	must(t, cluster.Kube.Create(context.Background(), member))
	cluster.WaitPodReady(t, ns, "probe")

	// 3. Curl the CGNAT peer THROUGH the gateway; expect "ok".
	out, _, err := cluster.ExecInPod(context.Background(), ns, "probe", "probe",
		"curl", "-sS", "-m", "8", "http://"+peer+"/")
	must(t, err)
	if out != "ok" {
		t.Fatalf("expected ok via tailnet egress, got %q", out)
	}

	// 4. Negative: an unlabeled pod cannot reach the peer (no route → timeout).
	other := newCurlPod(ns, "outsider", map[string]string{"app": "web"}, nil)
	must(t, cluster.Kube.Create(context.Background(), other))
	cluster.WaitPodReady(t, ns, "outsider")
	if _, _, err := cluster.ExecInPod(context.Background(), ns, "outsider", "outsider",
		"curl", "-sS", "-m", "5", "http://"+peer+"/"); err == nil {
		t.Fatal("non-member reached the tailnet peer — selection is broken")
	}
}
```

(`selfCGNAT`, `newCurlPod`, `antiAffinityFrom`, `applyEgressGroup` are e2e test helpers; `ExecInPod`/`WaitPodReady`/`NewNamespace` are framework helpers from Task 0b.)

- [ ] **Step 4: Run the e2e** (manual, gated)

Run: `make build && docker build ... && kind load ... && kubectl apply -f deploy/ && go test -tags e2e ./test/e2e/ -run TestPodReachesCGNATPeer -v`
Expected: PASS — the probe pod (labeled `egress=e2e`) curls a tailnet peer's `100.64.x.y` and gets a response; a pod *without* the label gets connection-refused/timeout (add that negative assertion).

- [ ] **Step 5: Commit**

```bash
git add code/cmd/tailgate-operator code/deploy code/test/e2e
git commit -m "feat: operator entrypoint + deploy manifests + kind e2e (pod→CGNAT via gateway)"
```

---

## Self-Review (completed)

**Spec coverage (vs DESIGN.md MVP / phase 1):**
- `EgressGroup` CRD (subnet/cgnat, kernel datapath, routed attach, SNAT-to-tag) → Task 1. ✓
- Per-group gateway = kernel-TUN subnet router, NET_ADMIN, accept-routes, MASQUERADE → Tasks 4, 6. ✓
- OAuth per-group tag authkey → Tasks 2, 3. ✓
- `attach: routed` = chained route-only CNI + agent route inject on members only → Tasks 7, 8, 9, 10, 11. ✓
- Gateway-side selection via informer (no Multus/Spiderpool/webhook) → Tasks 9, 10. ✓
- CNI hardening (RouteReplace idempotency, real CHECK, repatch loop, readyz for EROFS, non-lexical selection) → Tasks 8, 9, 11. ✓
- "3→3,000 pods = 0 new devices" proof → Task 12 e2e (pods are clients; only the gateway is a device). ✓
- Finalizer deletes the tailnet device → Task 5. ✓

**E2E coverage (per-task gate on kind — the requirement):**
- Reusable harness built first → Task 0b (`test/e2e/framework`, kind-config, Makefile `e2e`, CI). ✓
- Every task has a done-gate at its tier (A=unit, B=envtest, C/D=kind) → the per-task e2e map in "Testing strategy". ✓
- Explicit kind e2e steps in the kind-required tasks: gateway-up (Task 5 Step 4b), route-in-netns (Task 9 Step 4b), CNI install + repatch (Task 11 Step 5b), full datapath (Task 12). ✓
- Two-tier discipline (fast fake-client/envtest inner loop + kind outer gate), shared cluster + per-test ns cleanup; real-tailnet tests run against a **per-run ephemeral tailnet** (`test/e2e/tailnet`, created/deleted via the org API, authed by GitHub OIDC token-exchange — no long-lived secret; the `tailbridge` pattern), skipped if no org creds; route-presence mocked + always-on. ✓
- Dogfoods the real agent DaemonSet to install the CNI on kind nodes (chain onto kindnet at `cniVersion 0.3.1`); routes asserted via the framework (`AssertRouteInNetns`). ✓

**Deferred to follow-on plans (named, not in this MVP):**
- `2026-…-tailgate-4via6.md` — site-id registry + `MapVia` advertisement (mode `4via6`).
- `2026-…-tailgate-exit-node.md` — `ExitNodeRef`, the separate-table/fwmark pod routing, fail-closed.
- `2026-…-tailgate-ha.md` — replica pool + probe-gated rotation + per-group `via:` grants + source-preserving (`routed-4via6`).
- `2026-…-tailgate-attach-backends.md` — `multus`, `ebpf`, `cni-egress`, `proxy` backends + datapath detection.
- `2026-…-tailgate-userspace-datapath.md` — `datapath: userspace` (netstack, no NET_ADMIN).
- `2026-…-tailgate-split-dns.md` — optional per-group resolver + CoreDNS stub.

**Placeholder scan:** no TBD/TODO in steps; every code step has real code; commands have expected output. Two flagged verification points (tsclient v2 field names in Task 2; `controllerutil.SetControllerReference` scheme threading in Task 5) are called out inline with the exact thing to confirm, not left vague.

**Type consistency:** `EgressGroupReconciler` fields (`TS`, `Namespace`, `TSImage`, `GWImage`) consistent across Tasks 3/4/5/12; `gatewayName`/`authKeySecretName`/`tagsFor` defined once and reused; `netinfo.PodNetInfo`/`Read`/`Write`/`Remove` consistent across Tasks 7/8/10; `injectRoutes`/`removeRoutes`/`matchGroup`/`routeSet` consistent across Tasks 9/10. Framework helpers (`EnsureCluster`, `NewNamespace`, `WaitPodReady`/`WaitDeploymentReady`/`WaitDaemonSetReady`, `ExecInPod`, `AssertRouteInNetns`/`AssertNoRouteInNetns`, `NodeExec`/`AssertConflistChained`, `DumpDiagnostics`) are defined in Task 0b and used consistently across the e2e steps in Tasks 5/9/11/12. `WaitDeploymentReady`/`WaitDaemonSetReady`/`AssertNoRouteInNetns`/`NodeExec`/`AssertConflistChained`/`EventuallyConflistChained` are introduced as framework additions in the task that first needs them (5/9/11) — note this in Task 0b's framework as the extensible helper set.
