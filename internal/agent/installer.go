package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const cniPluginType = "tailgate-cni"

// InstallCNI copies the plugin binary into binDir and appends a chained
// {"type":"tailgate-cni"} entry to the lexically-first .conflist in confDir.
// Idempotent. Returns nil (logs) on read-only dirs so the agent doesn't crash-loop.
func InstallCNI(srcBin, binDir, confDir string) error {
	if err := copyFile(srcBin, filepath.Join(binDir, cniPluginType), 0o755); err != nil {
		return fmt.Errorf("install cni binary: %w", err)
	}
	return patchConflist(confDir)
}

func patchConflist(confDir string) error {
	path, err := firstConflist(confDir)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	plugins, _ := doc["plugins"].([]any)
	for _, p := range plugins {
		if m, ok := p.(map[string]any); ok && m["type"] == cniPluginType {
			return nil // already chained
		}
	}
	doc["plugins"] = append(plugins, map[string]any{"type": cniPluginType})
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, out, 0o644)
}

func firstConflist(confDir string) (string, error) {
	ents, err := os.ReadDir(confDir)
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
		return "", fmt.Errorf("no .conflist in %s", confDir)
	}
	sort.Strings(lists)
	return filepath.Join(confDir, lists[0]), nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	b, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	return atomicWrite(dst, b, mode)
}

func atomicWrite(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	_ = os.Chmod(tmp.Name(), mode)
	return os.Rename(tmp.Name(), path)
}
