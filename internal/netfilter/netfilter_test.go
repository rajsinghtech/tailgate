//go:build linux

package netfilter

import (
	"bytes"
	"os"
	"runtime"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	testBridge = "tgbr0"
	testTun    = "tailscale0"
	testMark   = uint32(0x7717)
	testTable  = 7717
)

// inNetNS runs fn inside a fresh network namespace so the test never touches the host.
// Requires root (CAP_NET_ADMIN); skips otherwise.
func inNetNS(t *testing.T, fn func()) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to create a network namespace")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatalf("get netns: %v", err)
	}
	defer func() { _ = netns.Set(orig); orig.Close() }()
	ns, err := netns.New() // creates AND enters a new netns
	if err != nil {
		t.Fatalf("new netns: %v", err)
	}
	defer ns.Close()
	fn()
}

func tailgateRules(t *testing.T, chain string) []*nftables.Rule {
	t.Helper()
	c, err := nftables.New()
	if err != nil {
		t.Fatalf("nftables conn: %v", err)
	}
	tbl := &nftables.Table{Family: nftables.TableFamilyINet, Name: tableName}
	rules, err := c.GetRules(tbl, &nftables.Chain{Table: tbl, Name: chain})
	if err != nil {
		t.Fatalf("get rules %s: %v", chain, err)
	}
	return rules
}

// TestSetupMASQUERADE_Idempotent proves re-running setup (a gateway restart) leaves exactly
// one rule per chain — the old `iptables -A` appended and duplicated on every start.
func TestSetupMASQUERADE_Idempotent(t *testing.T) {
	inNetNS(t, func() {
		dp, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := dp.SetupMASQUERADE(testBridge, testMark, testTun); err != nil {
				t.Fatalf("SetupMASQUERADE #%d: %v", i, err)
			}
		}
		if n := len(tailgateRules(t, "mark-in")); n != 1 {
			t.Fatalf("mark-in: want 1 rule, got %d (duplication regression)", n)
		}
		if n := len(tailgateRules(t, "snat-out")); n != 1 {
			t.Fatalf("snat-out: want 1 rule, got %d (duplication regression)", n)
		}
	})
}

// TestRuleContents asserts the mark + masquerade semantics survive into the installed nft
// ruleset (right interfaces, native-endian mark, masquerade verdict).
func TestRuleContents(t *testing.T) {
	inNetNS(t, func() {
		dp, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := dp.SetupMASQUERADE(testBridge, testMark, testTun); err != nil {
			t.Fatalf("SetupMASQUERADE: %v", err)
		}

		mark := tailgateRules(t, "mark-in")[0]
		if !hasCmpData(mark.Exprs, ifname(testBridge)) {
			t.Errorf("mark-in: missing iifname %q match", testBridge)
		}
		wantMark := binaryutil.NativeEndian.PutUint32(testMark)
		if !hasImmediate(mark.Exprs, wantMark) {
			t.Errorf("mark-in: missing native-endian mark %x", wantMark)
		}

		snat := tailgateRules(t, "snat-out")[0]
		if !hasCmpData(snat.Exprs, ifname(testTun)) {
			t.Errorf("snat-out: missing oifname %q match", testTun)
		}
		if !hasExpr[*expr.Masq](snat.Exprs) {
			t.Errorf("snat-out: missing masquerade")
		}
	})
}

// TestSetupPolicyRouting asserts the fwmark->table rules land for both families.
func TestSetupPolicyRouting(t *testing.T) {
	inNetNS(t, func() {
		// SetupPolicyRouting needs the TUN to exist and be up (as it is after tailscaled
		// starts); a dummy link stands in.
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: testTun}}); err != nil {
			t.Fatalf("add dummy %s: %v", testTun, err)
		}
		tun, err := netlink.LinkByName(testTun)
		if err != nil {
			t.Fatalf("link %s: %v", testTun, err)
		}
		if err := netlink.LinkSetUp(tun); err != nil {
			t.Fatalf("link up %s: %v", testTun, err)
		}
		if err := SetupPolicyRouting(testMark, testTable, testTun); err != nil {
			t.Fatalf("SetupPolicyRouting: %v", err)
		}
		for _, fam := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
			rules, err := netlink.RuleList(fam)
			if err != nil {
				t.Fatalf("rule list (family %d): %v", fam, err)
			}
			found := false
			for _, r := range rules {
				if r.Mark == testMark && r.Table == testTable {
					found = true
				}
			}
			if !found {
				t.Errorf("family %d: no fwmark=%#x table=%d rule", fam, testMark, testTable)
			}
		}
	})
}

func hasExpr[T expr.Any](exprs []expr.Any) bool {
	for _, e := range exprs {
		if _, ok := e.(T); ok {
			return true
		}
	}
	return false
}

func hasCmpData(exprs []expr.Any, data []byte) bool {
	for _, e := range exprs {
		if c, ok := e.(*expr.Cmp); ok && bytes.Equal(c.Data, data) {
			return true
		}
	}
	return false
}

func hasImmediate(exprs []expr.Any, data []byte) bool {
	for _, e := range exprs {
		if im, ok := e.(*expr.Immediate); ok && bytes.Equal(im.Data, data) {
			return true
		}
	}
	return false
}
