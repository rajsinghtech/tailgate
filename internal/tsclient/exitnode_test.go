package tsclient

import "testing"

func TestAdvertisesDefault(t *testing.T) {
	if !advertisesDefault([]string{"192.168.0.0/24", "0.0.0.0/0", "::/0"}) {
		t.Error("should detect 0.0.0.0/0")
	}
	if advertisesDefault([]string{"10.0.0.0/8"}) {
		t.Error("non-exit-node should not match")
	}
}

func TestMatchesExitFilter(t *testing.T) {
	tags := []string{"tag:exit-de", "tag:prod"}
	cases := []struct {
		tag, region string
		want        bool
	}{
		{"", "", true},             // auto:any
		{"tag:prod", "", true},     // tag match
		{"tag:missing", "", false}, // tag miss
		{"", "de", true},           // region -> tag:exit-de
		{"", "DE", true},           // case-insensitive
		{"", "us", false},          // region miss
		{"tag:prod", "de", true},   // both match
		{"tag:prod", "us", false},  // region miss wins
	}
	for _, c := range cases {
		if got := matchesExitFilter(tags, c.tag, c.region); got != c.want {
			t.Errorf("matchesExitFilter(tag=%q region=%q) = %v, want %v", c.tag, c.region, got, c.want)
		}
	}
}

func TestTailnetV4(t *testing.T) {
	if got := tailnetV4([]string{"fd7a:115c:a1e0::1", "100.64.0.5"}); got != "100.64.0.5" {
		t.Errorf("tailnetV4 = %q, want 100.64.0.5", got)
	}
	if got := tailnetV4([]string{"fd7a:115c:a1e0::1"}); got != "" {
		t.Errorf("tailnetV4 = %q, want empty", got)
	}
}
