// Package webhook is the operator's mutating admission webhook. When an EgressGroup sets
// dns.enabled, member pods it selects are given native tailnet DNS: their resolver is the
// gateway's MagicDNS (100.100.100.100), which already serves the whole tailnet namespace
// (MagicDNS, split-DNS, app-connector names, global forwarding), with the in-cluster
// resolver kept as a secondary for cluster.local.
package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

// quad100 is the MagicDNS service IP every tailscaled serves; it sits inside 100.64.0.0/10,
// which the agent already steers onto every member's ts0 — so no new route is needed.
const quad100 = "100.100.100.100"

// DNSMutator injects native-tailnet-DNS config into member pods of dns-enabled EgressGroups.
type DNSMutator struct {
	Client  client.Client
	Decoder admission.Decoder

	mu         sync.Mutex
	clusterDNS string // memoized kube-dns ClusterIP
}

// Handle implements admission.Handler.
func (m *DNSMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := m.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	dns := m.matchDNS(ctx, pod, req.Namespace)
	if dns == nil {
		return admission.Allowed("no dns-enabled EgressGroup matches")
	}
	// Idempotent: leave a pod that already points at the gateway resolver alone.
	if pod.Spec.DNSPolicy == corev1.DNSNone && pod.Spec.DNSConfig != nil &&
		len(pod.Spec.DNSConfig.Nameservers) > 0 && pod.Spec.DNSConfig.Nameservers[0] == quad100 {
		return admission.Allowed("dns already injected")
	}

	clusterDNS := dns.ClusterDNS
	if clusterDNS == "" {
		clusterDNS = m.detectClusterDNS(ctx)
	}
	applyMemberDNS(pod, dns, req.Namespace, clusterDNS)

	out, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	logf.FromContext(ctx).Info("injected native tailnet DNS",
		"namespace", req.Namespace, "generateName", pod.GenerateName, "nameservers", pod.Spec.DNSConfig.Nameservers)
	return admission.PatchResponseFromRaw(req.Object.Raw, out)
}

// applyMemberDNS rewrites the pod's resolver to native tailnet DNS: 100.100.100.100 primary
// (the gateway MagicDNS, which serves the whole tailnet namespace), the cluster resolver
// secondary for cluster.local, with the standard Kubernetes search list + ndots.
func applyMemberDNS(pod *corev1.Pod, dns *egressv1.MemberDNS, ns, clusterDNS string) {
	nameservers := []string{quad100}
	if clusterDNS != "" {
		nameservers = append(nameservers, clusterDNS) // secondary: cluster.local over the CNI
	}
	searches := dns.SearchDomains
	if len(searches) == 0 {
		searches = []string{ns + ".svc.cluster.local", "svc.cluster.local", "cluster.local"}
	}
	ndots := "5"
	if dns.Ndots != nil {
		ndots = strconv.Itoa(int(*dns.Ndots))
	}
	pod.Spec.DNSPolicy = corev1.DNSNone
	pod.Spec.DNSConfig = &corev1.PodDNSConfig{
		Nameservers: nameservers,
		Searches:    searches,
		Options:     []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndots}},
	}
}

// matchDNS returns the MemberDNS of the first dns-enabled EgressGroup whose selector matches.
func (m *DNSMutator) matchDNS(ctx context.Context, pod *corev1.Pod, ns string) *egressv1.MemberDNS {
	var groups egressv1.EgressGroupList
	if err := m.Client.List(ctx, &groups); err != nil {
		logf.FromContext(ctx).Error(err, "list EgressGroups")
		return nil
	}
	var nsLabels map[string]string
	gotNS := false
	for i := range groups.Items {
		g := &groups.Items[i]
		if g.Spec.DNS == nil || !g.Spec.DNS.Enabled {
			continue
		}
		if !gotNS {
			nsLabels, gotNS = m.nsLabels(ctx, ns), true
		}
		if selectorMatches(pod.Labels, nsLabels, g.Spec.Selector) {
			return g.Spec.DNS
		}
	}
	return nil
}

func (m *DNSMutator) nsLabels(ctx context.Context, ns string) map[string]string {
	var nsObj corev1.Namespace
	if err := m.Client.Get(ctx, client.ObjectKey{Name: ns}, &nsObj); err != nil {
		return map[string]string{}
	}
	return nsObj.Labels
}

// detectClusterDNS memoizes the kube-dns/CoreDNS Service ClusterIP (the cluster resolver).
func (m *DNSMutator) detectClusterDNS(ctx context.Context) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.clusterDNS != "" {
		return m.clusterDNS
	}
	var svc corev1.Service
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "kube-dns"}, &svc); err == nil &&
		svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != "None" {
		m.clusterDNS = svc.Spec.ClusterIP
	} else {
		logf.FromContext(ctx).Info("could not auto-detect kube-dns ClusterIP; cluster.local resolution may degrade — set spec.dns.clusterDNS")
	}
	return m.clusterDNS
}

// selectorMatches mirrors the agent's matchGroup for a single group's selector: a nil
// PodSelector + nil NamespaceSelector matches nothing (avoid grabbing every pod).
func selectorMatches(podLabels, nsLabels map[string]string, sel egressv1.EgressSelector) bool {
	if sel.NamespaceSelector != nil {
		s, err := metav1.LabelSelectorAsSelector(sel.NamespaceSelector)
		if err != nil || !s.Matches(labels.Set(nsLabels)) {
			return false
		}
	}
	if sel.PodSelector != nil {
		s, err := metav1.LabelSelectorAsSelector(sel.PodSelector)
		if err != nil || !s.Matches(labels.Set(podLabels)) {
			return false
		}
		return true
	}
	return sel.NamespaceSelector != nil
}
