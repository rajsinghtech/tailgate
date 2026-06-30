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
func CheckMembership(ctx context.Context, kc *KubeClient, args CNIArgs) (string, error) {
	podName := args["K8S_POD_NAME"]
	podNs := args["K8S_POD_NAMESPACE"]
	if podName == "" || podNs == "" {
		return "", fmt.Errorf("CNI args missing K8S_POD_NAME/K8S_POD_NAMESPACE")
	}

	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	pod, err := kc.GetPod(ctx, podName, podNs)
	if err != nil {
		return "", fmt.Errorf("get pod: %w", err)
	}
	ns, err := kc.GetNamespace(ctx, podNs)
	if err != nil {
		return "", fmt.Errorf("get namespace: %w", err)
	}
	groups, err := kc.ListEgressGroups(ctx)
	if err != nil {
		return "", fmt.Errorf("list egressgroups: %w", err)
	}
	return wiring.MatchGroup(pod, ns.Labels, groups), nil
}
