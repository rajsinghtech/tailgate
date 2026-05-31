//go:build linux

// Package netfilter programs the gateway's forward datapath — NAT/mark via nftables and
// policy routing via netlink — entirely with Go libraries, no iptables/ip shell-outs. This
// is what makes the gateway platform-agnostic: it talks to nf_tables directly, so it works
// on hosts that have nf_tables but NOT the legacy iptable_nat/iptable_mangle modules (Talos
// + Cilium), where the old `iptables` calls silently no-op'd ("Table does not exist") and
// broke member egress.
package netfilter

import (
	"fmt"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
)

// tableName is the single inet table this gateway owns in its (isolated) pod netns. Being
// in its own netns, it never collides with Cilium's host nftables.
const tableName = "tailgate"

// Datapath owns the gateway's nftables state via the native google/nftables API.
type Datapath struct {
	conn *nftables.Conn
}

// New opens an nftables netlink connection in the current netns. It fails loudly when
// nf_tables is unavailable, instead of the old silent iptables shell-out that returned an
// ignored error on nf_tables-only kernels.
func New() (*Datapath, error) {
	c, err := nftables.New()
	if err != nil {
		return nil, fmt.Errorf("open nftables (kernel needs CONFIG_NF_TABLES): %w", err)
	}
	return &Datapath{conn: c}, nil
}

// SetupMASQUERADE installs the gateway's forward datapath idempotently in a dedicated inet
// table:
//   - mark-in (filter/prerouting): member traffic arriving on bridgeIf gets fwmark, so the
//     policy-routing rule (see SetupPolicyRouting) steers it into the TUN table. Marking at
//     prerouting is seen by the routing decision that follows.
//   - snat-out (nat/postrouting): everything egressing tunIf is masqueraded onto the
//     gateway's tailnet IP (tailscaled drops un-SNAT'd member sources).
//
// The whole table is replaced atomically each call, so a gateway restart never duplicates
// rules (the old `iptables -A` appended on every start).
func (d *Datapath) SetupMASQUERADE(bridgeIf string, fwmark uint32, tunIf string) error {
	tbl := &nftables.Table{Family: nftables.TableFamilyINet, Name: tableName}

	// Best-effort drop any prior incarnation so this is idempotent; ignore the error since
	// the table may not exist on first start.
	d.conn.DelTable(tbl)
	_ = d.conn.Flush()

	t := d.conn.AddTable(tbl)
	accept := nftables.ChainPolicyAccept

	markCh := d.conn.AddChain(&nftables.Chain{
		Name:     "mark-in",
		Table:    t,
		Type:     nftables.ChainTypeFilter,
		Hooknum:  nftables.ChainHookPrerouting,
		Priority: nftables.ChainPriorityMangle,
		Policy:   &accept,
	})
	d.conn.AddRule(&nftables.Rule{Table: t, Chain: markCh, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(bridgeIf)},
		// load fwmark constant, then meta mark set (native byte order — the netlink fwmark
		// rule matches the host-native mark).
		&expr.Immediate{Register: 1, Data: binaryutil.NativeEndian.PutUint32(fwmark)},
		&expr.Meta{Key: expr.MetaKeyMARK, SourceRegister: true, Register: 1},
	}})

	natCh := d.conn.AddChain(&nftables.Chain{
		Name:     "snat-out",
		Table:    t,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
		Policy:   &accept,
	})
	d.conn.AddRule(&nftables.Rule{Table: t, Chain: natCh, Exprs: []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifname(tunIf)},
		&expr.Masq{},
	}})

	if err := d.conn.Flush(); err != nil {
		return fmt.Errorf("install nftables datapath: %w", err)
	}
	return nil
}

// ifname renders an interface name as a NUL-terminated buffer so the iif/oifname Cmp is an
// exact match (the kernel stores names in a fixed IFNAMSIZ buffer).
func ifname(s string) []byte { return []byte(s + "\x00") }
