package agent

import (
	"net/netip"
	"testing"
)

// The exit-node plan must carve EVERY cluster CIDR to the main table and add exactly one
// catch-all to the exit table — and the catch-all must come last (highest priority number)
// so cluster carve-outs win. A missing carve-out would blackhole kube-dns/API/pods.
func TestExitRulePlan(t *testing.T) {
	cidrs := []string{"10.244.0.0/16", "10.96.0.0/12", "fd00:10:96::/112"}
	plan := exitRulePlan(cidrs)

	if len(plan) != len(cidrs)+1 {
		t.Fatalf("expected %d decisions, got %d", len(cidrs)+1, len(plan))
	}
	for i, c := range cidrs {
		if !plan[i].ToMain || plan[i].CIDR != c {
			t.Errorf("decision %d = %+v, want carve-out %s", i, plan[i], c)
		}
		if _, err := netip.ParsePrefix(c); err != nil {
			t.Errorf("carve CIDR %q invalid: %v", c, err)
		}
	}
	last := plan[len(plan)-1]
	if last.ToMain || last.CIDR != "" {
		t.Errorf("final decision must be the catch-all to the exit table, got %+v", last)
	}
	if exitCarvePrio+len(cidrs) > exitFromPrio {
		t.Errorf("carve priorities (%d..%d) overrun the catch-all priority %d", exitCarvePrio, exitCarvePrio+len(cidrs)-1, exitFromPrio)
	}
	if exitTable == mainTable {
		t.Error("exit table must not be the main table")
	}
}
