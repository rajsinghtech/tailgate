package cni

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

// CNIDir is the host directory (shared via hostPath) where the agent writes
// credentials for the CNI plugin to query the kube API. The agent refreshes
// these files periodically to handle bound-token rotation.
const CNIDir = "/run/tailgate/cni"

// KubeCreds holds the pieces the CNI plugin needs to talk to the API server.
type KubeCreds struct {
	Server string // https://<host>:<port>
	Token  string
	CACert []byte
}

// LoadKubeCreds reads server, token, and ca.crt from dir. Returns an error if
// any file is missing (the agent hasn't written them yet — degraded mode).
func LoadKubeCreds(dir string) (*KubeCreds, error) {
	server, err := os.ReadFile(filepath.Join(dir, "server"))
	if err != nil {
		return nil, fmt.Errorf("read server: %w", err)
	}
	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read ca.crt: %w", err)
	}
	return &KubeCreds{
		Server: string(server),
		Token:  string(token),
		CACert: ca,
	}, nil
}

// KubeClient is a minimal HTTP client for the subset of API calls the CNI
// plugin needs: get a Pod, get a Namespace, list EgressGroups. It avoids
// pulling client-go/controller-runtime into the CNI binary.
type KubeClient struct {
	server string
	token  string
	hc     *http.Client
}

func NewKubeClient(creds *KubeCreds) (*KubeClient, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(creds.CACert) {
		return nil, fmt.Errorf("parse ca.crt")
	}
	return &KubeClient{
		server: creds.Server,
		token:  creds.Token,
		hc: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool},
			},
		},
	}, nil
}

func (k *KubeClient) get(ctx context.Context, path string, out any) error {
	url := k.server + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Accept", "application/json")
	resp, err := k.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("api %s: %d %s", path, resp.StatusCode, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// GetPod fetches a single pod by name/namespace.
func (k *KubeClient) GetPod(ctx context.Context, name, namespace string) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := k.get(ctx, fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", namespace, name), &pod); err != nil {
		return nil, err
	}
	return &pod, nil
}

// GetNamespace fetches a namespace (for its labels, used by NamespaceSelector).
func (k *KubeClient) GetNamespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	var ns corev1.Namespace
	if err := k.get(ctx, fmt.Sprintf("/api/v1/namespaces/%s", name), &ns); err != nil {
		return nil, err
	}
	return &ns, nil
}

// ListEgressGroups lists all EgressGroups (cluster-scoped CRD).
func (k *KubeClient) ListEgressGroups(ctx context.Context) ([]egressv1.EgressGroup, error) {
	var list egressv1.EgressGroupList
	if err := k.get(ctx, "/apis/tailscale.rajsingh.info/v1alpha1/egressgroups", &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}
