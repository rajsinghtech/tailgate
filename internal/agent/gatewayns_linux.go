//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// normalizeUID lowercases and strips non-alphanumerics so a pod UID matches however
// the cgroup driver formatted it (dashes vs underscores, .slice suffixes, etc.).
func normalizeUID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// netnsForPodUID finds a LIVE process belonging to the pod with the given UID (requires
// hostPID so /proc shows node processes) and returns its netns path. Used for both the
// gateway pod and member pods.
func netnsForPodUID(podUID string) (string, error) {
	return netnsForPodUIDIn("/proc", podUID)
}

// netnsForPodUIDIn is netnsForPodUID against an explicit /proc root (for tests).
//
// A pod's processes all share one netns, so any live one is correct. We must NOT return
// the first cgroup match blindly: a zombie or just-exited process (e.g. a `kubectl exec`
// into the gateway) lingers in /proc with a readable cgroup but no usable ns/net. Returning
// such a path makes every member that shares this (gateway) netns fail to wire with
// "open netns ...: no such file or directory". So skip any match whose ns/net can't be
// stat'd and keep scanning for a live one.
func netnsForPodUIDIn(procRoot, podUID string) (string, error) {
	want := normalizeUID(podUID)
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := e.Name()
		if pid[0] < '0' || pid[0] > '9' {
			continue
		}
		b, err := os.ReadFile(filepath.Join(procRoot, pid, "cgroup"))
		if err != nil {
			continue
		}
		if !strings.Contains(normalizeUID(string(b)), want) {
			continue
		}
		nsPath := filepath.Join(procRoot, pid, "ns", "net")
		if _, err := os.Stat(nsPath); err != nil {
			continue // dead/zombie process: cgroup lingers but the netns is gone
		}
		return nsPath, nil
	}
	return "", fmt.Errorf("no live process with a netns found for pod uid %s (hostPID enabled?)", podUID)
}
