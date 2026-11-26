//go:build !example_fixture

package main

import (
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/install"
)

// A shipped build honours no fixture seam (ADR 0017, decision D1). Every
// suite-only input the installer has — the proxy the pinned child may reach, the sum
// database it verifies against, and a provoked publication failure — is added by a
// `//go:build example_fixture` file, never by a runtime flag or an environment
// variable. So the shipped binary cannot be talked into honouring one: the grammar
// that would accept it is not compiled in. What keeps the tag off the image build is
// the builder stage's closed instruction set (its pinned FROM and one reviewed RUN),
// asserted by script/test/example-layers.sh.
//
// fixtureSeams is the closed list this build honours: none. A test builds both
// variants and requires every seam name to be absent from this one.
var fixtureSeams []string

// fixtureArguments carries the fixture grammar. It is empty here, so the shipped
// binary's parser has no fixture flag to refuse and none to accept.
type fixtureArguments struct{}

// fixtureSeamsRequested reports whether the caller asked this build to print the
// seams it honours. A shipped build has no such flag, so nothing can ask, and the
// refusal below is unreachable from the grammar.
func (fixtureArguments) fixtureSeamsRequested() bool { return false }

// environment is the production installation environment: the reviewed proxy and
// sum-database key, constructed here and not by any ambient value.
func (fixtureArguments) environment() (gotool.Environment, error) { return gotool.Install(), nil }

// transport is the reviewed proxy/CA passthrough from this process's environment.
func (fixtureArguments) transport() map[string]string { return gotool.ProductionTransport() }

// fault is nil: a shipped build has no way to ask publication to fail.
func (fixtureArguments) fault() func(install.Phase) error { return nil }

// runFixtureSeams is reachable only from the fixture grammar, so this build refuses
// it rather than printing an empty list that would look like a seam report.
func runFixtureSeams(fixtureArguments) int {
	return refuse("this build honours no fixture seam: " + usageText)
}
