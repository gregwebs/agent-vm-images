//go:build !example_fixture

package main

// The fixture seam is a build tag, so this file's job is to show the seam is
// absent from the shipped binary rather than merely unused (ADR 0017, decision
// D1). Two builds are made from this same module: the shipped one, and one with
// `-tags example_fixture`, which is the control that proves the checks can fail.
// The assertions are over the compiled bytes and over the parser's grammar, not
// over a runtime branch, so a seam that leaked into the shipped build fails here
// even if nothing calls it.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seamNames are the suite-only inputs a fixture build honours. They are written
// out rather than read from the tagged file, because a shipped build cannot
// compile that file: the list is the contract this test holds.
var seamNames = []string{"fixture-trust", "fixture-upstream", "fixture-bootstrap-exec"}

func TestShippedBinaryCarriesNoFixtureSeam(t *testing.T) {
	if len(fixtureSeams) != 0 {
		t.Fatalf("the shipped build honours fixture seams: %v", fixtureSeams)
	}
	shippedPath := build(t, "")
	taggedPath := build(t, "example_fixture")
	shipped, err := os.ReadFile(shippedPath)
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := os.ReadFile(taggedPath)
	if err != nil {
		t.Fatal(err)
	}
	// The control first: the fixture build must carry the names, or the absence
	// below would pass for a program that names no seam at all.
	for _, name := range seamNames {
		if !bytes.Contains(tagged, []byte(name)) {
			t.Errorf("the example_fixture build does not carry the seam %q, so the absence check below proves nothing", name)
		}
	}
	for _, name := range seamNames {
		if bytes.Contains(shipped, []byte(name)) {
			t.Errorf("the shipped binary carries the fixture seam %q", name)
		}
	}
	// A flag is a second way in, so the grammar is held too: the shipped binary
	// must refuse a flag it has no field for, and report no seam name while it
	// does.
	output, status := runBinary(t, shippedPath, "--fixture-seams")
	if status != 1 {
		t.Fatalf("the shipped binary answered --fixture-seams with status %d, want 1:\n%s", status, output)
	}
	if !strings.Contains(output, "usage: "+usageText) {
		t.Fatalf("the shipped binary did not report usage for --fixture-seams:\n%s", output)
	}
	if strings.Contains(output, "fixture-") {
		t.Fatalf("the shipped binary's refusal names a fixture seam:\n%s", output)
	}
	output, status = runBinary(t, taggedPath, "--fixture-seams")
	if status != 0 {
		t.Fatalf("the example_fixture build refused --fixture-seams with status %d:\n%s", status, output)
	}
	for _, name := range seamNames {
		if !strings.Contains(output, name) {
			t.Errorf("the example_fixture build did not list the seam %q:\n%s", name, output)
		}
	}
}

// TestFixtureBuildHonoursOnlyTheReviewedTrust is slice 2's seam: the fixture
// build's `--fixture-trust` replaces the compiled trust, and it is a top-level
// option, so it is accepted before the verb and after it.
func TestFixtureBuildHonoursOnlyTheReviewedTrust(t *testing.T) {
	shippedPath := build(t, "")
	taggedPath := build(t, "example_fixture")
	const fixtureFingerprint = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	committed := committedPinDir(t)

	// The shipped build has no such flag: it refuses it, prints no channel and
	// names no seam.
	output, status := runBinary(t, shippedPath, "--fixture-trust", fixtureFingerprint,
		"check", committed)
	if status != 1 {
		t.Fatalf("the shipped binary answered --fixture-trust with status %d:\n%s", status,
			output)
	}
	if !strings.Contains(output, "usage: "+usageText) || strings.Contains(output, "fixture-") {
		t.Fatalf("the shipped refusal is not a bare usage line:\n%s", output)
	}
	output, status = runBinary(t, shippedPath, "check", committed, "--fixture-trust",
		fixtureFingerprint)
	if status != 2 {
		t.Fatalf("the shipped binary answered a post-verb --fixture-trust with status %d:\n%s",
			status, output)
	}

	// The two slice-3 seams are equally absent from the shipped grammar, both
	// before the verb and after it.
	for _, flag := range []string{"--fixture-upstream", "--fixture-bootstrap-exec"} {
		output, status = runBinary(t, shippedPath, flag, "/tmp", "install", committed,
			"--arch", "amd64", "--expect-channel", "1.98.1", "--rustup-home",
			filepath.Join(t.TempDir(), "r"), "--cargo-home", filepath.Join(t.TempDir(), "c"))
		if status != 1 || !strings.Contains(output, "usage: "+usageText) ||
			strings.Contains(output, "fixture-") {
			t.Fatalf("shipped %s before the verb: status %d:\n%s", flag, status, output)
		}
		output, status = runBinary(t, shippedPath, "install", committed, "--arch", "amd64",
			"--expect-channel", "1.98.1", "--rustup-home", filepath.Join(t.TempDir(), "r"),
			"--cargo-home", filepath.Join(t.TempDir(), "c"), flag, "/tmp")
		if status != 2 || !strings.Contains(output, "usage: "+usageText) ||
			strings.Contains(output, "fixture-") {
			t.Fatalf("shipped %s after the verb: status %d:\n%s", flag, status, output)
		}
	}
	// The fixture build lists the seam.
	output, status = runBinary(t, taggedPath, "--fixture-seams")
	if status != 0 || !strings.Contains(output, "fixture-trust") {
		t.Fatalf("the fixture build did not list fixture-trust: %d %s", status, output)
	}
	// With the fixture trust, the committed pin is not the reviewed trust any
	// more, so both positions refuse with the trust-root rule rather than
	// accepting it.
	for _, args := range [][]string{
		{"--fixture-trust", fixtureFingerprint, "check", committed},
		{"check", committed, "--fixture-trust", fixtureFingerprint},
	} {
		output, status = runBinary(t, taggedPath, args...)
		if status != 1 || !strings.Contains(output, "is not the reviewed primary key") {
			t.Fatalf("%v: status %d:\n%s", args, status, output)
		}
	}
	// Without the seam the fixture build behaves as the shipped one does.
	output, status = runBinary(t, taggedPath, "check", committed)
	if status != 0 || output != "1.98.1\n" {
		t.Fatalf("the fixture build refused the committed pin: %d %q", status, output)
	}
}
