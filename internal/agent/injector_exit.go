package agent

// Exit-node full-tunnel member routing (platform-independent plan). The Linux applier in
// injector_exit_linux.go turns these decisions into policy rules in the member netns:
// cluster CIDRs go to the main table (the CNI's routes — kube-dns/API/pods/services never
// blackhole), everything else from the pod goes to a dedicated table whose only routes are
// 0.0.0.0/0 + ::/0 via the gateway, which forwards to the chosen exit node. This never
// touches the main table, so it cannot regress CGNAT/subnet mode.
const (
	exitTable     = 7717 // dedicated table: default routes via the gateway
	mainTable     = 254  // RT_TABLE_MAIN
	exitCarvePrio = 7700 // base priority for `to <clusterCIDR> lookup main` carve-outs
	exitFromPrio  = 7750 // priority for the catch-all `from all lookup exitTable`
)

// ExitOpts requests exit-node full-tunnel routing for a member. ClusterCIDRs are the
// in-cluster ranges (pod, service, node) kept on the primary CNI; all other egress is
// tunneled through the gateway to the exit node.
type ExitOpts struct {
	ClusterCIDRs []string
}

// exitDecision is one routing intent.
type exitDecision struct {
	CIDR   string // carve-out CIDR; "" for the catch-all
	ToMain bool   // true: `to CIDR lookup main`; false: catch-all `from all lookup exitTable`
}

// exitRulePlan returns the policy decisions for a member's exit-node setup: every cluster
// CIDR is carved to the main table, then a single catch-all sends the rest to the exit
// table. Carve-outs MUST sort before the catch-all (lower priority number).
func exitRulePlan(clusterCIDRs []string) []exitDecision {
	out := make([]exitDecision, 0, len(clusterCIDRs)+1)
	for _, c := range clusterCIDRs {
		out = append(out, exitDecision{CIDR: c, ToMain: true})
	}
	out = append(out, exitDecision{ToMain: false}) // catch-all -> exit table (gateway)
	return out
}
