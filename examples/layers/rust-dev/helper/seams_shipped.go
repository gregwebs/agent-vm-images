//go:build !example_fixture

package main

// A shipped build honours no fixture seam (ADR 0017, decision D1). Every
// suite-only input -- the reviewed trust it checks against, the upstream it
// fetches from, the program it runs a bootstrap with -- is added by a
// `//go:build example_fixture` file, never by a runtime flag or an environment
// variable a build argument could reach. So the shipped binary cannot be talked
// into honouring one: the grammar that would accept it is not compiled in.
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
