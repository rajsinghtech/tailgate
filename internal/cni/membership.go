package cni

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rajsinghtech/tailgate/internal/wiring"
)

// CNIArgs is the key=value map the kubelet/CRI passes to the plugin via
// skel.CmdArgs.Args (comma-separated). It always includes K8S_POD_NAME,
// K8S_POD_NAMESPACE, K8S_POD_UID on Kubernetes.
type CNIArgs map[string]string

// ParseCNIArgs parses the comma-separated key=value args string.
func ParseCNIArgs(raw string) CNIArgs {
	out := CNIArgs{}
	for _, kv := range strings.Split(raw, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

// CheckMembership queries the kube API for the pod's labels + namespace labels
// and matches them against all EgressGroups. Returns the group name or "".
//
// This is called from CNI ADD to determine whether to create ts0 in the pod
// netns BEFORE the sandbox boots (required for gVisor). If the API is unreachable
// or the agent hasn't written credentials yet, this returns "" — the pod starts
// without ts0 and the agent will wire it async (works for non-gVisor pods).
//
// Fast path: lists EgressGroups first and returns "" immediately if there are
// none (the common case for most pods in a cluster), avoiding the per-pod and
// per-namespace lookups entirely. The total budget is capped at 2 seconds so
// the CNI ADD path is never blocked for long even on a slow control plane.
func CheckMembership(ctx context.Context, kc *KubeClient, args CNIArgs) (string, error) {
	podName := args["K8S_POD_NAME"]
	podNs := args["K8S_POD_NAMESPACE"]
	if podName == "" || podNs == "" {
		return "", fmt.Errorf("CNI args missing K8S_POD_NAME/K8S_POD_NAMESPACE")
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// Fast path: if there are no EgressGroups at all, this pod can't be a member.
	// This is the common case (most pods in a cluster aren't tailgate members),
	// so we bail out with a single cheap list call instead of 3 API round-trips.
	groups, err := kc.ListEgressGroups(ctx)
	if err != nil {
		return "", fmt.Errorf("list egressgroups: %w", err)
	}
	if len(groups) == 0 {
		return "", nil
	}
	pod, err := kc.GetPod(ctx, podName, podNs)
	if err != nil {
		return "", fmt.Errorf("get pod: %w", err)
	}
	ns, err := kc.GetNamespace(ctx, podNs)
	if err != nil {
		return "", fmt.Errorf("get namespace: %w", err)
	}
	return wiring.MatchGroup(pod, ns.Labels, groups), nil
}
