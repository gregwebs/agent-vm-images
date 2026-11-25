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
// a caller supplies it, not whether the CLI supplies it.
//
// "Before any fetch" is what this row is for, so it is watched from outside the
// process: the fixture store appends every URL it is asked for to
// FIXTURE_FETCH_LOG. The same invocation runs twice — once with the gate closed
// and once with it open — so the control proves the prerequisites are real (it
// reaches the transport) and the row proves the refusal happens instead. A silent,
// successful fetch before the refusal would leave nothing on disk and would pass a
// text-only assertion; it cannot pass this one.
func TestStageRefusesInGitHubActionsBeforeAnyFetch(t *testing.T) {
	binary := buildAVMPin(t, "example_fixture")
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	if err := os.MkdirAll(dist, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "https://static.rust-lang.org/dist/channel-rust-1.98.1.toml"
	document := `{"key":"","fingerprint":"` + strings.Repeat("A", 40) +
		`","files":{"` + manifest + `":"eA=="}}`
	if err := os.WriteFile(filepath.Join(dist, "dist.json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	// Valid prerequisites: a real key file and a Dockerfile, so a run that gets
	// past the rule reaches the transport instead of failing on an absent input.
	key, err := os.ReadFile(filepath.Join("..", "..", "examples", "layers", "rust-dev",
		"data", "rust-toolchain", "rust-release-key.asc"))
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "rust-release-key.asc")
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	dockerfile := filepath.Join(root, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("ARG RUST_TOOLCHAIN=1.98.1\nFROM fixture\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	type attempt struct {
		status  int
		output  string
		fetched []string
	}
	run := func(githubActions string) attempt {
		// A fresh TMPDIR per attempt and a per-attempt counter file, so the counter
		// is the only place a fetch can show up.
		fetchLog := filepath.Join(root, "fetches-"+githubActions+".log")
		output, status := runAVMPinEnv(t, binary, []string{
			"GITHUB_ACTIONS=" + githubActions,
			"FIXTURE_DIST=" + dist,
			"FIXTURE_FETCH_LOG=" + fetchLog,
			"TMPDIR=" + t.TempDir(),
		}, "rust-toolchain", "stage",
			"--channel", "1.98.1",
			"--rustup", "1.28.2",
			"--key", keyPath,
			"--current", filepath.Join(root, "absent"),
			"--out", filepath.Join(root, "candidate-"+githubActions),
			"--dockerfile", dockerfile)
		result := attempt{status: status, output: output}
		if raw, err := os.ReadFile(fetchLog); err == nil {
			result.fetched = strings.Fields(string(raw))
		}
		return result
	}

	// Control: the same invocation with the gate open reaches the transport, so the
	// prerequisites are real and the counter records attempts.
	control := run("")
	if control.status == 0 {
		t.Fatalf("the control staged without the gate, so it is not a control")
	}
	if len(control.fetched) == 0 {
		t.Fatalf("the control never reached the transport (status %d), so the row below "+
			"proves nothing", control.status)
	}

	gated := run("true")
	if gated.status != 1 {
		t.Fatalf("status is %d, want 1: %s", gated.status, gated.output)
	}
	if !strings.Contains(gated.output, "stage mode is refused in GitHub Actions") {
		t.Fatalf("the refusal is not the CI rule: %q", gated.output)
	}
	if len(gated.fetched) > 0 {
		t.Fatalf("the CI refusal came after %d fetch(es): %v", len(gated.fetched),
			gated.fetched)
	}
	if _, err := os.Stat(filepath.Join(root, "candidate-true")); !os.IsNotExist(err) {
		t.Fatalf("the refused invocation created a candidate directory: %v", err)
	}
}
