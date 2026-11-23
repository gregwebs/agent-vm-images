//go:build !example_fixture

package main

import (
	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/fetch"
	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/toolchain"
)

// A shipped build honours no fixture seam (ADR 0017, decision D1). Every
// suite-only input -- the reviewed trust it checks against, the upstream it
// fetches from, the program it runs a bootstrap with -- is added by a
// `//go:build example_fixture` file, never by a runtime flag or an environment
// variable. So the shipped binary cannot be talked into honouring one: the grammar
// that would accept it is not compiled in. What keeps the tag off the image build
// is the builder stage's instruction allowlist (no SHELL, ADD, ENV or ARG beside
// its pinned FROM and reviewed RUN), asserted by script/test/example-layers.sh.
//
// fixtureSeams is the closed list this build honours: none. A test builds both
// variants and requires every seam name to be absent from this one.
var fixtureSeams []string

// fixtureArguments carries the fixture grammar. It is empty here, so the shipped
// binary's parser has no fixture flag to refuse and none to accept.
type fixtureArguments struct{}

// fixtureSeamsRequested reports whether the caller asked this build to print the
// seams it honours. A shipped build has no such flag, so nothing can ask.
func (fixtureArguments) fixtureSeamsRequested() bool { return false }

// trust is the reviewed trust, and the only trust a shipped build can use. The
// pin's own trust fields are compared against it; they never set it.
func (fixtureArguments) trust() (toolchain.TrustPolicy, error) {
	return toolchain.ReviewedTrust, nil
}

// transport is the production fetcher. The factory argument is the pin's exact
// URL closure, which a shipped build does not need: every HTTPS URL the pin names
// is fetched from its committed origin.
func (fixtureArguments) transport() (toolchain.TransportFactory, error) {
	return func([]toolchain.CanonicalURL) (toolchain.Transport, error) {
		return fetch.NewClient(), nil
	}, nil
}

// runner executes the bootstrap and the installed tools as the pinned paths name
// them.
func (fixtureArguments) runner() (toolchain.Runner, error) {
	return toolchain.ExecRunner{}, nil
}
