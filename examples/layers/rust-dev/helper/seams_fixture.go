//go:build example_fixture

package main

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
//     --fixture`, says so on its command line, so a fixture binary is always a
//     deliberate act by a suite.
//
// Slice 1 of #187 lands the tag and its grammar; the three seams -- the reviewed
// trust, the upstream transport and the bootstrap exec -- arrive with the verbs
// that take them (slices 2 and 3), each as a flag declared here.
type fixtureArguments struct {
	FixtureSeams bool `arg:"--fixture-seams" help:"print the suite-only inputs this build honours; writes nothing"`
}

// fixtureSeams names the inputs a fixture build will honour. The shipped build
// carries the same names nowhere, and no verb of this slice reads them yet: the
// list is what the ADR's seam rule is held to, so slice 2's flag is added here and
// the absence test in seams_test.go covers it without being rewritten.
var fixtureSeams = []string{"fixture-trust", "fixture-upstream", "fixture-bootstrap-exec"}

// fixtureSeamsRequested reports whether the caller asked for the seam list.
func (fixture fixtureArguments) fixtureSeamsRequested() bool { return fixture.FixtureSeams }
