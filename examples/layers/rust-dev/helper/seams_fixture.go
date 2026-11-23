//go:build example_fixture

package main

import (
	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/toolchain"
)

// This file exists only in a `-tags example_fixture` build, and it is the only
// way a suite-only input reaches this program (ADR 0017, decision D1). The tag
// protects a seam three ways at once:
//
//   - It is a compile-time boundary, so the code that honours a fixture is not in
//     the shipped binary. A seam that exists only in a differently built binary is
//     stronger than a flag a build argument could reach.
//   - The layer's builder stage is a closed instruction set — its pinned FROM and
//     its one reviewed RUN — which script/test/example-layers.sh enforces per
//     instruction class. Nothing in it can add a tag, so an example image build
//     cannot produce this binary.
//   - The host wrapper that does build it, `script/build/example-helper.sh
//     --fixture`, says so on its command line and writes its own file name, so a
//     fixture binary is always a deliberate act by a suite.
//
// Slice 1 of #187 landed the tag and its grammar. Slice 2 adds the reviewed trust
// seam; the upstream transport and the bootstrap exec arrive with #231.
type fixtureArguments struct {
	FixtureSeams bool    `arg:"--fixture-seams" help:"print the suite-only inputs this build honours; writes nothing"`
	FixtureTrust *string `arg:"--fixture-trust" placeholder:"FINGERPRINT" help:"use this fingerprint as the primary key and the only signer"`
}

// fixtureSeams names the inputs a fixture build honours. The shipped build
// carries the same names nowhere; seams_test.go holds both halves.
var fixtureSeams = []string{"fixture-trust", "fixture-upstream", "fixture-bootstrap-exec"}

// fixtureSeamsRequested reports whether the caller asked for the seam list.
func (fixture fixtureArguments) fixtureSeamsRequested() bool { return fixture.FixtureSeams }

// trust is the seam: with --fixture-trust FINGERPRINT the primary key and the
// signer set are both that fingerprint, which is what the pins suite's
// `driver.build` TrustPolicy does. Without it the reviewed trust applies, so the
// fixture binary can also stand in for the shipped one.
func (fixture fixtureArguments) trust() (toolchain.TrustPolicy, error) {
	if fixture.FixtureTrust == nil {
		return toolchain.ReviewedTrust, nil
	}
	fingerprint, err := toolchain.ParseFingerprint(*fixture.FixtureTrust, "--fixture-trust")
	if err != nil {
		return toolchain.TrustPolicy{}, err
	}
	return toolchain.TrustRoot(fingerprint, []toolchain.Fingerprint{fingerprint}), nil
}
