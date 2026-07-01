//go:build linux

// tailgate-agent is the node-local DaemonSet entrypoint: it wires member pods to
// their node-local group gateway (veth + routes), driven by EgressGroup + Pod state.
package main

import (
	"log/slog"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/agent"
)

// clusterCIDRs parses TAILGATE_CLUSTER_CIDRS (comma-separated pod/service/node ranges)
// kept on the primary CNI for exit-node members so cluster traffic never blackholes.
func clusterCIDRs() []string {
	v := strings.TrimSpace(os.Getenv("TAILGATE_CLUSTER_CIDRS"))
	if v == "" {
		return nil
	}
	var out []string
	for _, c := range strings.Split(v, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		log.Error("scheme", "err", err)
		os.Exit(1)
	}
	if err := egressv1.AddToScheme(scheme); err != nil {
		log.Error("scheme", "err", err)
		os.Exit(1)
	}
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		log.Error("client", "err", err)
		os.Exit(1)
	}

	node := os.Getenv("NODE_NAME")
	if node == "" {
		log.Error("NODE_NAME unset")
		os.Exit(1)
	}

	// Install the CNI plugin binary + chain it into the node's conflist before
	// starting the wiring loop. This is what makes ts0 appear at CNI ADD time
	// (before the sandbox boots), which is required for gVisor. Idempotent; safe
	// if the dirs are read-only (logs + continues — non-gVisor pods still work
	// via the async agent path). Set TAILGATE_INSTALL_CNI=false to skip (e.g. in
	// CI environments without gVisor where the async agent path is sufficient).
	if getenv("TAILGATE_INSTALL_CNI", "true") == "true" {
		cniBin := getenv("CNI_BIN_DIR", "/host/opt/cni/bin")
		cniConf := getenv("CNI_CONF_DIR", "/host/etc/cni/net.d")
		if err := agent.InstallCNI("/usr/local/bin/tailgate-cni", cniBin, cniConf); err != nil {
			log.Warn("install cni (non-gVisor pods will use async wiring)", "err", err)
		} else {
			log.Info("cni plugin installed", "bin", cniBin, "conf", cniConf)
		}
	} else {
		log.Info("cni install skipped (TAILGATE_INSTALL_CNI=false)")
	}

	a := &agent.Agent{
		C:            c,
		Node:         node,
		GatewayNS:    getenv("TAILGATE_NAMESPACE", "tailgate-system"),
		Log:          log,
		ClusterCIDRs: clusterCIDRs(),
		// The agent writes these into /run/tailgate/cni/ on every sync pass so
		// the CNI plugin can query the kube API at CNI ADD time (pre-sandbox).
		ServerURL:  ctrl.GetConfigOrDie().Host,
		SATokenDir: getenv("TAILGATE_SA_DIR", "/var/run/secrets/kubernetes.io/serviceaccount"),
	}
	log.Info("starting tailgate-agent", "node", node, "gatewayNS", a.GatewayNS, "clusterCIDRs", a.ClusterCIDRs, "serverURL", a.ServerURL)
	a.Run(ctrl.SetupSignalHandler(), 3*time.Second)
}
