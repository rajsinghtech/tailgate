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

func TestTailnetV4(t *testing.T) {
	if got := tailnetV4([]string{"fd7a:115c:a1e0::1", "100.64.0.5"}); got != "100.64.0.5" {
		t.Errorf("tailnetV4 = %q, want 100.64.0.5", got)
	}
	if got := tailnetV4([]string{"fd7a:115c:a1e0::1"}); got != "" {
		t.Errorf("tailnetV4 = %q, want empty", got)
	}
}
