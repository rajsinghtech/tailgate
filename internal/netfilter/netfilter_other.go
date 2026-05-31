//go:build !linux

// Stub so the module still builds on non-linux (the gateway only ever runs on linux).
package netfilter

import "errors"

var errUnsupported = errors.New("tailgate netfilter requires linux")

// Datapath is a non-linux stub.
type Datapath struct{}

func New() (*Datapath, error)                                 { return nil, errUnsupported }
func (*Datapath) SetupMASQUERADE(string, uint32, string) error { return errUnsupported }
func SetupPolicyRouting(uint32, int, string) error            { return errUnsupported }
func EnableForwardingAndRelaxRPFilter() error                 { return errUnsupported }
