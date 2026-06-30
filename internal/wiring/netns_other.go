//go:build !linux

package wiring

import "errors"

// WireState is a no-op on non-Linux.
type WireState int

const (
	WireNone WireState = iota
	WireCNI
	WireFull
)

func WithNetNS(_ string, _ func() error) error  { return errors.New("linux only") }
func CheckWireState(_, _, _ string) WireState   { return WireNone }
func SetupMember(_, _ string, _ []string) error { return errors.New("linux only") }
func DeleteHostPeer(_ string)                   {}
func LinkExistsOnHost(_ string) bool            { return false }
