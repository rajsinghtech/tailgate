//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// mkproc creates procRoot/<pid>/cgroup containing the pod UID, and (if alive) an ns/net entry.
func mkproc(t *testing.T, root, pid, uid string, alive bool) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(filepath.Join(dir, "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	cg := "0::/kubepods.slice/kubepods-besteffort-pod" + uid + ".slice/cri-containerd-x.scope\n"
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cg), 0o644); err != nil {
		t.Fatal(err)
	}
	if alive {
		if err := os.WriteFile(filepath.Join(dir, "ns", "net"), []byte("netns"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A zombie/exited process whose cgroup still matches but whose ns/net is gone must be
// skipped, not returned — otherwise every member sharing that (gateway) netns fails to wire.
func TestNetnsForPodUIDSkipsDeadNetns(t *testing.T) {
	root := t.TempDir()
	uid := "abc-123-def-456"
	mkproc(t, root, "1000", uid, false)         // zombie, sorts first ("1000" < "999"), no ns/net
	mkproc(t, root, "999", uid, true)           // live process with a usable ns/net
	mkproc(t, root, "1", "zzz-other-uid", true) // unrelated pod, must not match

	got, err := netnsForPodUIDIn(root, uid)
	if err != nil {
		t.Fatalf("netnsForPodUIDIn: %v", err)
	}
	if want := filepath.Join(root, "999", "ns", "net"); got != want {
		t.Errorf("netnsForPodUIDIn = %q, want %q (must skip the zombie 1000)", got, want)
	}
}

func TestNetnsForPodUIDNoLiveMatch(t *testing.T) {
	root := t.TempDir()
	uid := "deadbeef"
	mkproc(t, root, "1000", uid, false) // only a zombie matches
	if _, err := netnsForPodUIDIn(root, uid); err == nil {
		t.Fatal("expected an error when only a dead/zombie process matches")
	}
}
