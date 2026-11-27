//go:build !example_fixture

package main

// A shipped `avm-pin` honours no fixture seam. The maintainer stager's suite-only
// input — the origin the pinned `go` child resolves through, and the verifier key it
// uses — is added by a `//go:build example_fixture` file, never by a runtime flag or an
// environment variable, so the shipped binary cannot be talked into staging from a
// loopback recorder: the grammar that would accept it is not compiled in.
//
// The tag is the same one the go-dev helper's installer uses (ADR 0017, decision D1),
// for the same reason: a suite has to drive *this* writer's code over fixture bytes, and
// a flag would let a maintainer stage a pin from a test proxy by accident.

import (
	"encoding/json"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
)

// goplsFixtureSeams is the closed list a build honours, and whether the caller asked
// to print it. The shipped build has neither the question nor a name.
type goplsFixtureSeams struct {
	Requested bool
	Names     []string
}

// goplsFixtureArguments carries the fixture grammar. It is empty here, so the shipped
// binary's parser has no fixture flag to refuse and none to accept.
type goplsFixtureArguments struct{}

// seams reports no seam: a shipped build cannot be asked to list one.
func (goplsFixtureArguments) seams() (goplsFixtureSeams, error) { return goplsFixtureSeams{}, nil }

// environment is the production origin: the reviewed proxy and the reviewed verifier
// key, constructed here and by no ambient value.
func (goplsFixtureArguments) environment() (gotool.Environment, error) {
	return gotool.Install(), nil
}

// transport is the reviewed proxy/CA passthrough from this process's environment. It
// carries a route and a CA, never a content decision.
func (goplsFixtureArguments) transport() map[string]string {
	return gotool.ProductionTransport()
}

// decodeDocument decodes one retained record for the proposal comparison.
func decodeDocument(raw []byte, document *map[string]any) error {
	return json.Unmarshal(raw, document)
}
