// Package netinfo is the CNI<->agent contract: the chained CNI plugin records each
// pod's network-namespace path keyed by PodIP; the agent reads it to wire member pods
// to their node-local gateway.
package netinfo

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Dir is the host directory (shared via hostPath) holding per-pod net-info files.
var Dir = "/run/tailgate/pods"

// PodNetInfo is what the CNI records for a pod.
type PodNetInfo struct {
	PodIP  string `json:"podIP"`
	PodUID string `json:"podUID,omitempty"`
	Netns  string `json:"netns"`
	IfName string `json:"ifName"`
	// GwName is the host-side gateway peer name when the CNI plugin pre-created
	// ts0 using a non-IP key (e.g. pod UID under Multus without prevResult). Empty
	// means derive names from PodIP (legacy path).
	GwName string `json:"gwName,omitempty"`
}

type PrewireInfo struct {
	PodUID string `json:"podUID"`
	GwName string `json:"gwName"`
}

var PrewireDir = "/run/tailgate/prewire"

func WritePrewire(p PrewireInfo) error {
	if err := os.MkdirAll(PrewireDir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(PrewireDir, ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), filepath.Join(PrewireDir, p.PodUID))
}

func ReadPrewire(podUID string) (PrewireInfo, error) {
	var p PrewireInfo
	b, err := os.ReadFile(filepath.Join(PrewireDir, podUID))
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

func RemovePrewire(podUID string) error {
	if podUID == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(PrewireDir, podUID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Write atomically records n keyed by n.PodIP.
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
		tmp.Close()
		return err
	}
	tmp.Close()
	return os.Rename(tmp.Name(), filepath.Join(Dir, n.PodIP))
}

// Read returns the recorded info for podIP.
func Read(podIP string) (PodNetInfo, error) {
	var n PodNetInfo
	b, err := os.ReadFile(filepath.Join(Dir, podIP))
	if err != nil {
		return n, err
	}
	return n, json.Unmarshal(b, &n)
}

// Remove deletes the record for podIP (no error if absent).
func Remove(podIP string) error {
	if err := os.Remove(filepath.Join(Dir, podIP)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
