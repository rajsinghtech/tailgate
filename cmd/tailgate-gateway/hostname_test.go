//go:build linux

package main

import "testing"

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"tank":                     "tank",
		"Stone":                    "stone",
		"ip-10-0-1-5.ec2.internal": "ip-10-0-1-5-ec2-internal",
		"node_01":                  "node-01",
		"--weird--":                "weird",
		"  ":                       "",
		"":                         "",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithNode(t *testing.T) {
	if got := withNode("tailgate-gw-payments", "tank"); got != "tailgate-gw-payments-tank" {
		t.Errorf("withNode = %q, want tailgate-gw-payments-tank", got)
	}
	// empty/whitespace node leaves the base unchanged
	if got := withNode("tailgate-gw-x", "   "); got != "tailgate-gw-x" {
		t.Errorf("withNode(empty node) = %q, want tailgate-gw-x", got)
	}
	// long node names are sanitized + truncated to keep the hostname <= 63 (DNS label limit)
	long := "ip-10-100-200-30.us-west-2.compute.internal.extra.long.suffix.here"
	got := withNode("tailgate-gw-some-long-group-name", long)
	if len(got) > 63 {
		t.Errorf("withNode produced %d chars (> 63): %q", len(got), got)
	}
}
