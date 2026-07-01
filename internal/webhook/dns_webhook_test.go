package webhook

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
)

func TestApplyMemberDNS(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		pod := &corev1.Pod{}
		applyMemberDNS(pod, &egressv1.MemberDNS{Enabled: true}, "payments", "10.96.0.10", "corp.ts.net")
		if pod.Spec.DNSPolicy != corev1.DNSNone {
			t.Fatalf("DNSPolicy = %q, want None", pod.Spec.DNSPolicy)
		}
		ns := pod.Spec.DNSConfig.Nameservers
		if len(ns) != 2 || ns[0] != "100.100.100.100" || ns[1] != "10.96.0.10" {
			t.Fatalf("nameservers = %v, want [100.100.100.100 10.96.0.10]", ns)
		}
		if got := pod.Spec.DNSConfig.Searches; len(got) != 4 || got[0] != "corp.ts.net" || got[1] != "payments.svc.cluster.local" {
			t.Fatalf("searches = %v, want [corp.ts.net payments.svc.cluster.local ...]", got)
		}
		opt := pod.Spec.DNSConfig.Options
		if len(opt) != 1 || opt[0].Name != "ndots" || opt[0].Value == nil || *opt[0].Value != "5" {
			t.Fatalf("options = %v, want ndots=5", opt)
		}
	})

	t.Run("no cluster DNS → quad100 only", func(t *testing.T) {
		pod := &corev1.Pod{}
		applyMemberDNS(pod, &egressv1.MemberDNS{Enabled: true}, "default", "", "corp.ts.net")
		if ns := pod.Spec.DNSConfig.Nameservers; len(ns) != 1 || ns[0] != "100.100.100.100" {
			t.Fatalf("nameservers = %v, want [100.100.100.100]", ns)
		}
	})

	t.Run("no tailnet → cluster search only", func(t *testing.T) {
		pod := &corev1.Pod{}
		applyMemberDNS(pod, &egressv1.MemberDNS{Enabled: true}, "default", "10.96.0.10", "")
		if got := pod.Spec.DNSConfig.Searches; len(got) != 3 || got[0] != "default.svc.cluster.local" {
			t.Fatalf("searches = %v, want cluster-only (no tailnet)", got)
		}
	})

	t.Run("overrides", func(t *testing.T) {
		n := int32(2)
		pod := &corev1.Pod{}
		applyMemberDNS(pod, &egressv1.MemberDNS{
			Enabled:       true,
			SearchDomains: []string{"corp.example"},
			Ndots:         &n,
		}, "default", "10.0.0.10", "corp.ts.net")
		if got := pod.Spec.DNSConfig.Searches; len(got) != 2 || got[0] != "corp.ts.net" || got[1] != "corp.example" {
			t.Fatalf("searches = %v, want [corp.ts.net corp.example]", got)
		}
		if *pod.Spec.DNSConfig.Options[0].Value != "2" {
			t.Fatalf("ndots = %s, want 2", *pod.Spec.DNSConfig.Options[0].Value)
		}
	})
}

func TestSelectorMatches(t *testing.T) {
	podLabels := map[string]string{"app": "web", "egress": "yes"}
	nsLabels := map[string]string{"team": "payments"}
	sel := func(pod, ns map[string]string) egressv1.EgressSelector {
		s := egressv1.EgressSelector{}
		if pod != nil {
			s.PodSelector = &metav1.LabelSelector{MatchLabels: pod}
		}
		if ns != nil {
			s.NamespaceSelector = &metav1.LabelSelector{MatchLabels: ns}
		}
		return s
	}
	cases := []struct {
		name string
		sel  egressv1.EgressSelector
		want bool
	}{
		{"pod match", sel(map[string]string{"app": "web"}, nil), true},
		{"pod mismatch", sel(map[string]string{"app": "db"}, nil), false},
		{"ns + pod match", sel(map[string]string{"egress": "yes"}, map[string]string{"team": "payments"}), true},
		{"ns mismatch", sel(map[string]string{"app": "web"}, map[string]string{"team": "other"}), false},
		{"ns only match", sel(nil, map[string]string{"team": "payments"}), true},
		{"empty selector matches nothing", sel(nil, nil), false},
	}
	for _, c := range cases {
		if got := selectorMatches(podLabels, nsLabels, c.sel); got != c.want {
			t.Errorf("%s: selectorMatches = %v, want %v", c.name, got, c.want)
		}
	}
}
