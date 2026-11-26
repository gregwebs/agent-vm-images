//go:build !example_fixture

package main

// The seam tag's two halves. A shipped build must carry no suite-only input: no
// grammar that accepts one, and no name in its bytes. The fixture build must carry
// them, because a control that only proves an absence proves nothing — the positive
// half is what makes the absence mean something.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seamNames are the suite-only inputs a fixture build honours. They are written out
// rather than read from the tagged file, because a shipped build cannot compile that
// file: the list is the contract this test holds.
var seamNames = []string{"fixture-seams", "fixture-proxy", "fixture-sumdb", "fixture-fault"}

// buildTagged compiles this module with the given build tags.
func buildTagged(t *testing.T, tags string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "go-dev-helper")
	argv := []string{"build", "-trimpath", "-buildvcs=false"}
	if tags != "" {
		argv = append(argv, "-tags", tags)
	}
	argv = append(argv, "-o", binary, ".")
	command := exec.Command("go", argv...)
	command.Env = append(os.Environ(), "GOFLAGS=-mod=vendor", "GOPROXY=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	return binary
}

// TestShippedBuildHonoursNoSeam: every seam name the fixture build reports is absent
// from the shipped build's grammar and bytes.
func TestShippedBuildHonoursNoSeam(t *testing.T) {
	fixture := buildTagged(t, "example_fixture")
	stdout, stderr, status := runBinary(t, fixture, "--fixture-seams")
	if status != 0 {
		t.Fatalf("the fixture build refused --fixture-seams with status %d: %s", status, stderr)
	}
	if len(fixtureSeams) != 0 {
		t.Fatalf("the shipped build honours fixture seams: %v", fixtureSeams)
	}
	seams := strings.Fields(stdout)
	if len(seams) != len(seamNames) {
		t.Fatalf("the fixture build reported %v, want %v", seams, seamNames)
	}
	for index, name := range seamNames {
		if seams[index] != name {
			t.Fatalf("seam %d is %q, want %q", index, seams[index], name)
		}
	}

	shipped := build(t)
	_, shippedStderr, shippedStatus := runBinary(t, shipped, "--fixture-seams")
	if shippedStatus != 1 || !strings.Contains(shippedStderr, "usage: "+usageText) {
		t.Fatalf("the shipped build answered --fixture-seams with status %d (%q), want the "+
			"usage refusal 1: the flag is not in its grammar", shippedStatus, shippedStderr)
	}
	raw, err := os.ReadFile(shipped)
	if err != nil {
		t.Fatalf("read the shipped binary: %v", err)
	}
	fixtureRaw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read the fixture binary: %v", err)
	}
	for _, name := range seamNames {
		if !strings.Contains(string(fixtureRaw), name) {
			t.Fatalf("the fixture binary does not carry %q, so the shipped-build absence "+
				"below proves nothing", name)
		}
		if strings.Contains(string(raw), name) {
			t.Fatalf("the shipped binary carries the suite-only input %q", name)
		}
	}
}

// goRootForTest is this host's Go root, which the target rules accept.
func goRootForTest(t *testing.T) string {
	t.Helper()
	command := exec.Command("go", "env", "GOROOT")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go env GOROOT: %v", err)
	}
	return strings.TrimSpace(string(output))
}

// TestFixtureProxyMustBeLoopback: even the fixture build refuses an origin that is
// not the suite's recorder, so a fixture run cannot reach a real proxy by typo.
func TestFixtureProxyMustBeLoopback(t *testing.T) {
	binary := buildTagged(t, "example_fixture")
	committed := committedData(t)
	base := []string{"install", committed, "--expect-gopls", "v0.23.0",
		"--expect-go", "1.27.3", "--go-root", goRootForTest(t), "--arch", "amd64",
		"--bin-dir", t.TempDir(), "--share-dir", t.TempDir()}
	for origin, fragment := range map[string]string{
		"https://proxy.golang.org": "--fixture-proxy must be http://IP:PORT",
		"http://example.test:1":    "--fixture-proxy must name the loopback address",
		"http://127.0.0.1:1/path":  "--fixture-proxy must be http://IP:PORT",
	} {
		_, stderr, status := runBinary(t, binary,
			append(base, "--fixture-proxy", origin)...)
		if status != 1 || !strings.Contains(stderr, fragment) {
			t.Fatalf("%s: status %d, stderr %q, want a refusal naming %q", origin, status,
				stderr, fragment)
		}
	}
	_, stderr, status := runBinary(t, binary,
		append(base, "--fixture-proxy", "http://127.0.0.1:1", "--fixture-sumdb",
			"sum.golang.org")...)
	if status != 1 || !strings.Contains(stderr, "--fixture-sumdb accepts only off") {
		t.Fatalf("a real sum-database key was accepted: status %d, stderr %q", status, stderr)
	}
}
