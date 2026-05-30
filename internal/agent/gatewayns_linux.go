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

// netnsForPodUID finds a process belonging to the pod with the given UID (requires
// hostPID so /proc shows node processes) and returns its netns path. Used for both
// the gateway pod and member pods.
func netnsForPodUID(podUID string) (string, error) {
	want := normalizeUID(podUID)
	entries, err := os.ReadDir("/proc")
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
		b, err := os.ReadFile(filepath.Join("/proc", pid, "cgroup"))
		if err != nil {
			continue
		}
		if strings.Contains(normalizeUID(string(b)), want) {
			return filepath.Join("/proc", pid, "ns", "net"), nil
		}
	}
	return "", fmt.Errorf("no process found for pod uid %s (hostPID enabled?)", podUID)
}
