//go:build example_fixture

package main

// This file exists only in a `-tags example_fixture` build, and it is the only way a
// suite-only input reaches the stager (ADR 0017, decision D1). The tag protects the
// seam three ways: it is a compile-time boundary, the fixture build is named on the
// command line, and the shipped build has no such grammar at all — gopls_shipped.go is
// the other half, and cmd/avm-pin/gopls_seams_test.go holds both.
//
// Two properties bound what even this variant may do. The proxy it is given must be a
// loopback origin, and the sum database must be exactly `off`: those are the declared
// fixture substitutions, and a fixture binary pointed anywhere else is a mistake this
// build refuses instead of serving. The recorder is the suite's listener, never this
// program's.

import (
	"encoding/json"
	"fmt"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
)

// goplsFixtureArguments is the suite-only grammar: the origin the pinned `go` child
// resolves through, the verifier key it uses, and the seam list.
type goplsFixtureArguments struct {
	FixtureSeams bool    `arg:"--fixture-seams" help:"print the suite-only inputs this build honours; writes nothing"`
	FixtureProxy *string `arg:"--fixture-proxy" placeholder:"URL" help:"point the pinned go child at this loopback GOPROXY"`
	FixtureSumDB *string `arg:"--fixture-sumdb" placeholder:"VALUE" help:"the child's GOSUMDB; only off is accepted"`
}

// goplsFixtureSeams is the closed list this build honours.
type goplsFixtureSeams struct {
	Requested bool
	Names     []string
}

var goplsFixtureSeamNames = []string{"fixture-seams", "fixture-proxy", "fixture-sumdb"}

// seams reports the names this build honours, or nothing.
func (fixture goplsFixtureArguments) seams() (goplsFixtureSeams, error) {
	if !fixture.FixtureSeams {
		return goplsFixtureSeams{}, nil
	}
	return goplsFixtureSeams{Requested: true, Names: goplsFixtureSeamNames}, nil
}

// environment is the loopback seam: the child resolves through the suite's recorder and
// GOSUMDB is off, because the synthetic modules are not in the real transparency log and
// this record is never evidence of live sum-database authentication.
func (fixture goplsFixtureArguments) environment() (gotool.Environment, error) {
	if fixture.FixtureProxy == nil {
		return gotool.Install(), nil
	}
	if err := gotool.RequireLoopback("--fixture-proxy", *fixture.FixtureProxy); err != nil {
		return gotool.Environment{}, err
	}
	sumdb := "off"
	if fixture.FixtureSumDB != nil {
		if *fixture.FixtureSumDB != "off" {
			return gotool.Environment{}, fmt.Errorf("--fixture-sumdb accepts only off, not %q",
				*fixture.FixtureSumDB)
		}
		sumdb = *fixture.FixtureSumDB
	}
	return gotool.NewEnvironment(*fixture.FixtureProxy, sumdb), nil
}

// transport is empty in a fixture build: the point of the fixture is that the child
// reaches exactly the recorder named on the command line and nothing this process's
// environment also carries.
func (goplsFixtureArguments) transport() map[string]string { return nil }

// decodeDocument decodes one retained record for the proposal comparison.
func decodeDocument(raw []byte, document *map[string]any) error {
	return json.Unmarshal(raw, document)
}
