// tailgate-cni is the chained, route-only CNI plugin binary. It records each pod's
// netns for the agent and passes the previous plugin's result through.
package main

import (
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/version"

	tcni "github.com/rajsinghtech/tailgate/internal/cni"
)

func main() {
	skel.PluginMainFuncs(
		skel.CNIFuncs{Add: tcni.CmdAdd, Del: tcni.CmdDel, Check: tcni.CmdCheck},
		version.All,
		"tailgate-cni",
	)
}
