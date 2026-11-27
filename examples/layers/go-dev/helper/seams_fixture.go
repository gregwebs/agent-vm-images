//go:build example_fixture

package main

import (
	"fmt"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/install"
)

// This file exists only in a `-tags example_fixture` build, and it is the only way a
// suite-only input reaches the installer (ADR 0017, decision D1). The tag protects
// the seam three ways: it is a compile-time boundary, the layer's builder stage is a
// closed instruction set that cannot add a tag, and the host wrapper that builds this
// variant says `--fixture` on its command line and gives the binary its own name.
//
// Two properties bound what even this variant may do. The proxy it is given must be
// a loopback origin, and the sum database must be exactly `off`: those are the two
// declared fixture substitutions of this suite, and a fixture binary pointed
// anywhere else is a mistake this build refuses instead of serving. The recorder is
// the suite's listener, never this program's.
type fixtureArguments struct {
	FixtureSeams bool    `arg:"--fixture-seams" help:"print the suite-only inputs this build honours; writes nothing"`
	FixtureProxy *string `arg:"--fixture-proxy" placeholder:"URL" help:"point the pinned go child at this loopback GOPROXY"`
	FixtureSumDB *string `arg:"--fixture-sumdb" placeholder:"VALUE" help:"the child's GOSUMDB; only off is accepted"`
	FixtureFault *string `arg:"--fixture-fault" placeholder:"PHASE" help:"fail publication at PHASE: binary-published or notice-written"`
}

// fixtureSeams names the inputs a fixture build honours. The shipped build carries
// the same names nowhere; seams_test.go holds both halves.
var fixtureSeams = []string{"fixture-seams", "fixture-proxy", "fixture-sumdb", "fixture-fault"}

// fixtureSeamsRequested reports whether the caller asked for the seam list.
func (fixture fixtureArguments) fixtureSeamsRequested() bool { return fixture.FixtureSeams }

// environment is the loopback seam: the child's GOPROXY is the suite's recorder and
// GOSUMDB is off, because the synthetic modules are not in the real transparency log
// and this record is never evidence of live sum-database authentication.
func (fixture fixtureArguments) environment() (gotool.Environment, error) {
	if fixture.FixtureProxy == nil {
		return gotool.Install(), nil
	}
	if err := gotool.RequireLoopback("--fixture-proxy", *fixture.FixtureProxy); err != nil {
		return gotool.Environment{}, err
	}
	sumdb := "off"
	if fixture.FixtureSumDB != nil {
		if *fixture.FixtureSumDB != "off" {
			return gotool.Environment{}, fmt.Errorf(
				"--fixture-sumdb accepts only off, not %q", *fixture.FixtureSumDB)
		}
		sumdb = *fixture.FixtureSumDB
	}
	return gotool.NewEnvironment(*fixture.FixtureProxy, sumdb), nil
}

// transport is empty in a fixture build: the point of the fixture is that the child
// reaches exactly the recorder named on the command line and nothing this process's
// environment also carries.
func (fixtureArguments) transport() map[string]string { return nil }

// fault returns the one-shot publication failure the suite asked for, or nil. A
// single firing keeps a same-target retry a retry rather than a second injection.
func (fixture fixtureArguments) fault() func(install.Phase) error {
	if fixture.FixtureFault == nil {
		return nil
	}
	phase := *fixture.FixtureFault
	fired := false
	return func(current install.Phase) error {
		if fired {
			return nil
		}
		switch {
		case phase == "binary-published" && current == install.PhaseBinaryPublished:
			fired = true
			return fmt.Errorf("injected failure after the binary was published")
		case phase == "notice-written" && current == install.PhaseNoticeWritten:
			fired = true
			return fmt.Errorf("injected failure after the notice bytes were written")
		}
		return nil
	}
}

// runFixtureSeams prints the seam names, one per line, and writes nothing.
func runFixtureSeams(fixture fixtureArguments) int {
	fmt.Println(strings.Join(fixtureSeams, "\n"))
	return 0
}
