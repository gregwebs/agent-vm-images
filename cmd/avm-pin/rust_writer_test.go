//go:build !example_fixture

package main

// Two rows that used to be *declared divergences* between this writer and the
// Python stager it replaced, witnessed inside the maintenance suite by running
// both. #245 deleted the Python, so the divergence no longer exists: what is left
// is this binary's own behaviour, and these assertions are what fails when it
// changes. The alternative -- deleting the declaration with its witness -- would
// have left two reading rules with nothing holding them.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCurrentOnAnUnusablePinIsOneRefusal holds the refusal shape of `current`.
//
// The Python entrypoint called its loader with nothing above it, so an OS-level
// read failure escaped as a traceback with status 1. This verb reports one
// `rust_toolchain: ` line with status 1 instead. The *reason* is the point: a
// directory where `pin.json` must be a file is a missing input, not a crash.
func TestCurrentOnAnUnusablePinIsOneRefusal(t *testing.T) {
	binary := buildAVMPin(t, "")
	unusable := t.TempDir()
	if err := os.Mkdir(filepath.Join(unusable, "pin.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, status := runAVMPin(t, binary, "rust-toolchain", "current", unusable)
	if status != 1 {
		t.Fatalf("status is %d, want 1: %s", status, output)
	}
	if !strings.Contains(output, "rust_toolchain: missing input: pin.json") {
		t.Fatalf("the refusal is not the loader's one line: %q", output)
	}
	if strings.Contains(output, "Traceback") || strings.Count(strings.TrimSpace(output), "\n") > 0 {
		t.Fatalf("the refusal is not a single line: %q", output)
	}
}

// TestStageRefusesAnEmptyReviewerPair holds the usage rule for the renewal flags.
//
// The Python stager treated only an absent reviewer as absent, so an empty one
// reached the loader and was refused there (status 1). This verb refuses the pair
// up front (status 2), before any fetch, because the flag was given and is empty.
// `script/dev/upgrade-rust-toolchain.sh` refuses an empty `--reviewer` as usage
// before either writer runs, so this row is the direct verb's own.
func TestStageRefusesAnEmptyReviewerPair(t *testing.T) {
	binary := buildAVMPin(t, "")
	root := t.TempDir()
	out := filepath.Join(root, "candidate")
	output, status := runAVMPin(t, binary, "rust-toolchain", "stage",
		"--channel", "9.9.9",
		"--rustup", "1.28.2",
		"--key", filepath.Join(root, "absent.asc"),
		"--current", filepath.Join(root, "absent"),
		"--out", out,
		"--dockerfile", filepath.Join(root, "Dockerfile"),
		"--verify-bootstrap-exception", "2026-01-02",
		"--reviewer", "")
	if status != 2 {
		t.Fatalf("status is %d, want 2: %s", status, output)
	}
	if !strings.Contains(output,
		"--verify-bootstrap-exception and --reviewer are given together") {
		t.Fatalf("the refusal does not name the flag pair: %q", output)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("the refused invocation created a candidate directory: %v", err)
	}
}

// TestStageRefusesInGitHubActionsBeforeAnyFetch holds the CLI's own CI rule.
//
// The maintenance suite's `publishing under GITHUB_ACTIONS is refused` row goes
// through the shell wrapper, which refuses first: a writer whose `GitHubActions`
// dependency were no longer supplied by this CLI would leave that row green, and
// `TestStageRefusesGitHubActions` in the layer's package tests the dependency when
// a caller supplies it, not whether the CLI supplies it. So this row drives the
// direct verb against a fixture store that holds no bytes: the only way to get the
// CI message is to refuse before staging, because a staging run would report
// `refused endpoint` first.
func TestStageRefusesInGitHubActionsBeforeAnyFetch(t *testing.T) {
	binary := buildAVMPin(t, "example_fixture")
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	// A store holding one URL that is not the pin's: the fixture build needs a
	// non-empty store to build its transport, and a staging run would report
	// `refused endpoint` for every fetch instead of the CI text.
	document := `{"key":"","fingerprint":"` + strings.Repeat("A", 40) +
		`","files":{"https://example.invalid/decoy":"eA=="}}`
	if err := os.WriteFile(filepath.Join(dist, "dist.json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "candidate")
	output, status := runAVMPinEnv(t, binary, []string{
		"GITHUB_ACTIONS=true",
		"FIXTURE_DIST=" + dist,
	}, "rust-toolchain", "stage",
		"--channel", "1.98.1",
		"--rustup", "1.28.2",
		"--key", filepath.Join(root, "absent.asc"),
		"--current", filepath.Join(root, "absent"),
		"--out", out,
		"--dockerfile", filepath.Join(root, "Dockerfile"))
	if status != 1 {
		t.Fatalf("status is %d, want 1: %s", status, output)
	}
	if !strings.Contains(output, "stage mode is refused in GitHub Actions") {
		t.Fatalf("the refusal is not the CI rule: %q", output)
	}
	if strings.Contains(output, "refused endpoint") || strings.Contains(output, "connection") {
		t.Fatalf("the refusal names a transport failure, so staging started: %q", output)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("the refused invocation created a candidate directory: %v", err)
	}
}
