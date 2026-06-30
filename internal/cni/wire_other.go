//go:build !linux

package cni

import "github.com/rajsinghtech/tailgate/internal/netinfo"

// SetupMemberIfMember is a no-op on non-Linux (dev/CI platforms where the CNI
// plugin compiles but doesn't run netlink ops).
func SetupMemberIfMember(_ string, _ netinfo.PodNetInfo) {}

// deleteHostPeer is a no-op on non-Linux.
func deleteHostPeer(_ string) {}
