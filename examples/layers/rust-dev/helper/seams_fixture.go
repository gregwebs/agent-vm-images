//go:build example_fixture

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/fetch"
	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/toolchain"
)

// This file exists only in a `-tags example_fixture` build, and it is the only
// way a suite-only input reaches this program (ADR 0017, decision D1). The tag
// protects a seam three ways at once:
//
//   - It is a compile-time boundary, so the code that honours a fixture is not in
//     the shipped binary. A seam that exists only in a differently built binary is
//     stronger than a flag a build argument could reach.
//   - The layer's builder stage is a closed instruction set -- its pinned FROM and
//     its one reviewed RUN -- which script/test/example-layers.sh enforces per
//     instruction class. Nothing in it can add a tag, so an example image build
//     cannot produce this binary.
//   - The host wrapper that does build it, `script/build/example-helper.sh
//     --fixture`, says so on its command line and writes its own file name, so a
//     fixture binary is always a deliberate act by a suite.
//
// Slice 1 of #187 landed the tag and its grammar, slice 2 the reviewed trust, and
// slice 3 the upstream transport and bootstrap exec.
type fixtureArguments struct {
	FixtureSeams         bool    `arg:"--fixture-seams" help:"print the suite-only inputs this build honours; writes nothing"`
	FixtureTrust         *string `arg:"--fixture-trust" placeholder:"FINGERPRINT" help:"use this fingerprint as the primary key and the only signer"`
	FixtureUpstream      *string `arg:"--fixture-upstream" placeholder:"URL" help:"rewrite the pin's exact URLs onto this loopback origin"`
	FixtureBootstrapExec *string `arg:"--fixture-bootstrap-exec" placeholder:"FILE" help:"run the installed bootstrap through this program"`
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

// transport is the loopback seam. NewLoopbackClient accepts only the origin's
// strict `http://127.0.0.1:<port>` shape and rewrites only the exact canonical
// URLs the pin names; a different path on the pinned host is refused, not
// rewritten (Q1 of #231). The listener is the suite's, never this binary's.
func (fixture fixtureArguments) transport() (toolchain.TransportFactory, error) {
	if fixture.FixtureUpstream == nil {
		return func([]toolchain.CanonicalURL) (toolchain.Transport, error) {
			return fetch.NewClient(), nil
		}, nil
	}
	origin := *fixture.FixtureUpstream
	return func(pinned []toolchain.CanonicalURL) (toolchain.Transport, error) {
		urls := make([]string, 0, len(pinned))
		for _, url := range pinned {
			urls = append(urls, string(url))
		}
		return fetch.NewLoopbackClient(origin, urls)
	}, nil
}

// runner is the bootstrap-exec seam: it intercepts only the bootstrap call (the
// pinned rustup-init, which lives outside CARGO_HOME/bin) and leaves every
// rustup/cargo/rustc/rustfmt call direct.
func (fixture fixtureArguments) runner() (toolchain.Runner, error) {
	if fixture.FixtureBootstrapExec == nil {
		return toolchain.ExecRunner{}, nil
	}
	wrapper := *fixture.FixtureBootstrapExec
	if !filepath.IsAbs(wrapper) {
		return nil, fmt.Errorf("--fixture-bootstrap-exec must be an absolute path: %s", wrapper)
	}
	info, err := os.Stat(wrapper)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("--fixture-bootstrap-exec is not an executable file: %s", wrapper)
	}
	return bootstrapWrapperRunner{wrapper: wrapper}, nil
}

// bootstrapWrapperRunner prefixes the bootstrap invocation with the fixture tool.
type bootstrapWrapperRunner struct {
	wrapper string
}

func (r bootstrapWrapperRunner) Run(ctx context.Context, argv, env []string,
	cwd string) (toolchain.Result, error) {
	if len(argv) > 0 && filepath.Base(argv[0]) == "rustup-init" && !inCargoBin(argv[0], env) {
		wrapped := append([]string{r.wrapper, argv[0]}, argv[1:]...)
		return (toolchain.ExecRunner{}).Run(ctx, wrapped, env, cwd)
	}
	return (toolchain.ExecRunner{}).Run(ctx, argv, env, cwd)
}

// inCargoBin reports whether path is under the child environment's CARGO_HOME/bin.
func inCargoBin(path string, env []string) bool {
	for _, entry := range env {
		value, found := strings.CutPrefix(entry, "CARGO_HOME=")
		if !found {
			continue
		}
		return filepath.Dir(path) == filepath.Join(value, "bin")
	}
	return false
}
