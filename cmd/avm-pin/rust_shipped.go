//go:build !example_fixture

package main

// A shipped `avm-pin` honours no fixture seam. Every suite-only input -- the byte
// store the maintenance suite serves, and the trust the fixture key was signed
// under -- is added by a `//go:build example_fixture` file, never by a runtime
// flag or an environment variable, so the shipped binary cannot be talked into
// honouring one: the grammar that would accept it is not compiled in.
//
// The writer's fixture transport is a build tag for the same reason the rust-dev
// helper's is (ADR 0017, decision D1): the pin suites must drive *this* writer's
// code over the fixture's bytes, and a flag would let a maintainer run a pin from
// fixture bytes by accident.

import (
	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/toolchain"
)

// rustToolchainSeams is the closed list a build honours, and whether the caller
// asked to print it. The shipped build has neither the question nor a name.
type rustToolchainSeams struct {
	Requested bool
	Names     []string
}

// rustToolchainFixtureArguments carries the fixture grammar. It is empty here, so
// the shipped binary's parser has no fixture flag to refuse and none to accept.
type rustToolchainFixtureArguments struct{}

// seams reports no seam: a shipped build cannot be asked to list one.
func (rustToolchainFixtureArguments) seams() (rustToolchainSeams, error) {
	return rustToolchainSeams{}, nil
}

// transport is the production byte source: a nil Transport asks the toolchain
// package for its own HTTPS client, so this binary never names one.
func (rustToolchainFixtureArguments) transport() (toolchain.Transport, error) {
	return nil, nil
}

// trust is the reviewed trust, and the only trust a shipped build can use.
func (rustToolchainFixtureArguments) trust() (toolchain.TrustPolicy, error) {
	return toolchain.ReviewedTrust, nil
}
