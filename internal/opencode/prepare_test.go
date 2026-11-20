package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The committed manifest, byte for byte: the staging rewrite is compared with the
// committed bytes, so the fixture has to be the reviewed document.
const committedManifest = `{
  "name": "agent-vm-guest-opencode-plugins",
  "version": "0.0.0",
  "private": true,
  "type": "module",
  "packageManager": "pnpm@11.11.0",
  "dependencies": {
    "@anthropic-ai/claude-agent-sdk": "0.3.281",
    "@openchamber/opencode-claude": "1.4.1"
  }
}
`

// stagedManifest is what `jq --arg version … '.dependencies[…] = $version'`
// produced, and what the encoder has to reproduce: same key order, two-space
// indentation, one trailing newline.
const stagedManifest = `{
  "name": "agent-vm-guest-opencode-plugins",
  "version": "0.0.0",
  "private": true,
  "type": "module",
  "packageManager": "pnpm@11.11.0",
  "dependencies": {
    "@anthropic-ai/claude-agent-sdk": "0.3.281",
    "@openchamber/opencode-claude": "1.4.2"
  }
}
`

const committedWorkspace = "packages:\n  - .\n"
const committedLock = "lockfileVersion: '9.0'\n"
const committedPins = "aa  package.json\n"

// fakeCheck is the reviewed-bytes checker and fakePin the developer-only writer.
// They record their argv, so the tests can see that the writer never ran or that
// the staged project was re-checked.
const fakeNode = `#!/bin/sh
printf 'node|%s\n' "$*" >>"$CALLS"
script=${1##*/}
shift
case "$script" in
    check-lock.mjs)
        if [ -n "${CHECK_FAIL:-}" ]; then echo "check-lock: $CHECK_FAIL" >&2; exit 1; fi
        case "${1:-}" in
            *opencode-prepare-*)
                if [ -n "${STAGED_CHECK_FAIL:-}" ]; then
                    echo 'check-lock: pnpm-lock.yaml: bytes differ from the reviewed pin' >&2
                    exit 1
                fi
                ;;
        esac
        printf '%s\n' "${CHECK_SELECTION:-1.4.1}"
        ;;
    pin-pnpm-inputs.mjs)
        printf 'deadbeef  package.json\n' >"$1/pnpm-inputs.sha256"
        ;;
esac
exit 0
`

const fakePnpm = `#!/bin/sh
printf 'pnpm|%s\n' "$*" >>"$CALLS"
if [ "$1" = --version ]; then
    printf '%s\n' "${MANAGER_VERSION:-11.11.0}"
    exit "${MANAGER_STATUS:-0}"
fi
if [ -n "${INSTALL_FAIL:-}" ]; then
    echo 'pnpm: fixture install failure' >&2
    exit "$INSTALL_FAIL"
fi
printf '%s\n' "$PWD" >"$PWD_FILE"
printf '%s\n' "$HOME" >"$HOME_FILE"
[ -f pnpm-workspace.yaml ] || { echo 'fixture: no staged workspace' >&2; exit 90; }
[ -f pnpm-lock.yaml ] || { echo 'fixture: no staged lock seed' >&2; exit 91; }
printf 'fixture lock\n' >pnpm-lock.yaml
`

// fakeEnvelope is the composed run-pnpm.sh: it records the manager argv it was
// handed and then runs it in the caller's working directory.
const fakeEnvelope = `#!/bin/sh
printf 'run-pnpm|%s\n' "$*" >>"$CALLS"
result=$1
shift 2
exec "$@"
`

// prepareFixture is one preparation case: the authorized contract directory with
// the envelope, the fake manager and checker on PATH, and the committed four
// inputs in the prefix.
type prepareFixture struct {
	*fixture
	contract string
}

func newPrepareFixture(t *testing.T) *prepareFixture {
	t.Helper()
	tree := newFixture(t)
	contract := filepath.Join(tree.root, "contract")
	if err := os.MkdirAll(contract, 0o755); err != nil {
		t.Fatalf("mkdir contract: %v", err)
	}
	if err := os.WriteFile(filepath.Join(contract, "run-pnpm.sh"), []byte(fakeEnvelope), 0o755); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
	tree.program(t, "node", fakeNode)
	tree.program(t, "pnpm", fakePnpm)
	for name, body := range map[string]string{
		"package.json":        committedManifest,
		"pnpm-workspace.yaml": committedWorkspace,
		"pnpm-lock.yaml":      committedLock,
		"pnpm-inputs.sha256":  committedPins,
	} {
		if err := os.WriteFile(filepath.Join(tree.prefix, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	t.Setenv("AGENT_VM_FIXTURE_ROOT", tree.root)
	t.Setenv("PWD_FILE", filepath.Join(tree.root, "pwd"))
	t.Setenv("HOME_FILE", filepath.Join(tree.root, "home"))
	return &prepareFixture{fixture: tree, contract: contract}
}

func (p *prepareFixture) options(arguments ...string) PrepareOptions {
	return PrepareOptions{
		Contract:    p.contract,
		Check:       filepath.Join(p.root, "check-lock.mjs"),
		Pin:         filepath.Join(p.root, "pin-pnpm-inputs.mjs"),
		FixtureRoot: os.Getenv("AGENT_VM_FIXTURE_ROOT"),
		Arguments:   arguments,
		Out:         &strings.Builder{},
	}
}

// run executes one preparation with captured streams.
func (p *prepareFixture) run(t *testing.T, arguments ...string) (string, string, error) {
	t.Helper()
	options := p.options(arguments...)
	out, errOut := &strings.Builder{}, &strings.Builder{}
	options.Out, options.Err = out, errOut
	err := PrepareLock(context.Background(), options)
	return out.String(), errOut.String(), err
}

// committedInputs returns the four reviewed files' bytes.
func (p *prepareFixture) committedInputs(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{"package.json", "pnpm-workspace.yaml", "pnpm-lock.yaml", "pnpm-inputs.sha256"} {
		data, err := os.ReadFile(filepath.Join(p.prefix, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		files[name] = string(data)
	}
	return files
}

func TestPrepareUsage(t *testing.T) {
	tree := newPrepareFixture(t)
	for _, arguments := range [][]string{
		{},
		{tree.prefix},
		{tree.prefix, "1.4.1", "--refresh-lock", "extra"},
	} {
		_, _, err := tree.run(t, arguments...)
		var usage *Usage
		if !errors.As(err, &usage) {
			t.Fatalf("%q: error = %v (%T), want a Usage", arguments, err, err)
		}
		if usage.Text != prepareUsage {
			t.Fatalf("%q: usage = %q, want %q", arguments, usage.Text, prepareUsage)
		}
	}
	tree.expectNoChild(t)
}

// TestPrepareUnknownThirdWordIsSilentUsage pins the predecessor's `exit 2` with no
// diagnostic: an option this driver does not have is a usage refusal, not a
// refused request, and it printed nothing.
func TestPrepareUnknownThirdWordIsSilentUsage(t *testing.T) {
	tree := newPrepareFixture(t)
	_, stderr, err := tree.run(t, tree.prefix, "1.4.1", "--repin")
	var usage *Usage
	if !errors.As(err, &usage) {
		t.Fatalf("error = %v (%T), want a Usage", err, err)
	}
	if usage.Text != "" {
		t.Fatalf("usage text = %q, want the silent exit 2", usage.Text)
	}
	if stderr != "" {
		t.Fatalf("a silent usage refusal wrote %q to stderr", stderr)
	}
	tree.expectNoChild(t)
}

// TestPrepareRefusesNonCanonicalSelection covers the whole reviewed grammar: a
// range, a dist-tag, a `v` prefix, a short form, a leading zero and a multi-line
// value must each be refused before the checker runs.
func TestPrepareRefusesNonCanonicalSelection(t *testing.T) {
	tree := newPrepareFixture(t)
	for _, selection := range []string{
		"latest", "^1.4.1", "v1.4.1", "1.4", "01.4.1", "1.4.1.0",
		"1.4.1\n1.4.2", " 1.4.1", "1.4.1 ", "*", "workspace:*",
	} {
		_, _, err := tree.run(t, tree.prefix, selection)
		mustRefusal(t, err, "prepare-lock: selection must be canonical exact semver")
	}
	tree.expectNoChild(t)
}

// TestPrepareAcceptsCanonicalSelection keeps the grammar from being stricter than
// the reviewed one: a prerelease and a build suffix are exact versions.
func TestPrepareAcceptsCanonicalSelection(t *testing.T) {
	tree := newPrepareFixture(t)
	for _, selection := range []string{"1.4.1", "0.0.0", "1.4.1-rc.1", "1.4.1+build.7", "1.4.1-0.3.0"} {
		t.Setenv("CHECK_SELECTION", selection)
		if _, _, err := tree.run(t, tree.prefix, selection); err != nil {
			t.Fatalf("%q: %v, want the committed selection to be accepted", selection, err)
		}
	}
}

// TestPrepareGuardOrder is the ordering invariant, stated as what it proves: the
// fixture-root authorization runs before the first child (the checker), and the
// preload rejection runs before any Node or pnpm process starts. Neither may move:
// the authorization is what stops an unauthorized prefix reaching the checker or
// the resolution envelope, and a preloaded module can patch node:crypto.
func TestPrepareGuardOrder(t *testing.T) {
	t.Run("fixture root before the first child", func(t *testing.T) {
		tree := newPrepareFixture(t)
		t.Setenv("AGENT_VM_FIXTURE_ROOT", "")
		_, _, err := tree.run(t, tree.prefix, "1.4.1")
		mustUsage(t, err, "non-production prefix requires fixture root")
		tree.expectNoChild(t)
	})
	t.Run("preload before the checker", func(t *testing.T) {
		tree := newPrepareFixture(t)
		t.Setenv("NODE_OPTIONS", "--require=/tmp/inject.js")
		_, _, err := tree.run(t, tree.prefix, "1.4.1")
		mustRefusal(t, err, "manager runtime overrides are unsupported")
		tree.expectNoChild(t)
	})
	for _, key := range []string{"NODE_PATH", "BUN_OPTIONS"} {
		t.Run("preload "+key, func(t *testing.T) {
			tree := newPrepareFixture(t)
			t.Setenv(key, "/tmp/injected")
			_, _, err := tree.run(t, tree.prefix, "1.4.1")
			mustRefusal(t, err, "manager runtime overrides are unsupported")
			tree.expectNoChild(t)
		})
	}
}

// TestPreparePrefixAuthorization covers the physical-path rule: only the
// production prefix and something genuinely beneath the fixture root may be
// prepared.
func TestPreparePrefixAuthorization(t *testing.T) {
	tree := newPrepareFixture(t)
	elsewhere := t.TempDir()
	file := tree.write(t, "not-a-directory", "", 0o644)
	link := filepath.Join(tree.root, "link")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	for _, prefix := range []string{
		file,
		tree.root,
		elsewhere,
		link,
		filepath.Join(elsewhere, "prefix"),
	} {
		_, _, err := tree.run(t, prefix, "1.4.1")
		mustSilent(t, err, 1)
	}
	tree.expectNoChild(t)
	// A root that cannot be resolved is the predecessor's failed `cd` under
	// `set -e`: usage, with a diagnostic naming the root.
	absent := filepath.Join(tree.root, "absent")
	t.Setenv("AGENT_VM_FIXTURE_ROOT", absent)
	_, _, err := tree.run(t, tree.prefix, "1.4.1")
	mustUsage(t, err, "cd: can't cd to "+absent)
	tree.expectNoChild(t)
}

// TestPreparePrefixSiblings keeps the containment rule from being a string prefix
// test: a sibling of the authorized root shares its name and is not inside it.
func TestPreparePrefixSiblings(t *testing.T) {
	tree := newPrepareFixture(t)
	sibling := tree.root + "-sibling"
	if err := os.MkdirAll(filepath.Join(sibling, "prefix"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })
	_, _, err := tree.run(t, filepath.Join(sibling, "prefix"), "1.4.1")
	mustSilent(t, err, 1)
	tree.expectNoChild(t)
}

// TestPrepareRelativePrefixIsAuthorized is the `cd "$PREFIX" && pwd -P` rule: a
// relative prefix resolves against the caller's directory and then has to be inside
// the root like any other. `EvalSymlinks` alone would return a relative path that no
// absolute root can contain.
func TestPrepareRelativePrefixIsAuthorized(t *testing.T) {
	// "" means "this one is absolute": only the relative one is named relatively.
	for _, testCase := range []struct{ name, prefix, root string }{
		{"relative prefix", "prefix", ""},
		{"relative root", "", "."},
		{"both relative", "prefix", "."},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tree := newPrepareFixture(t)
			t.Chdir(tree.root)
			prefix := tree.prefix
			if testCase.prefix != "" {
				prefix = testCase.prefix
			}
			root := tree.root
			if testCase.root != "" {
				root = testCase.root
			}
			t.Setenv("AGENT_VM_FIXTURE_ROOT", root)
			stdout, _, err := tree.run(t, prefix, "1.4.1")
			if err != nil {
				t.Fatalf("a relative prefix inside the root was refused: %v", err)
			}
			if !strings.Contains(stdout, "committed inputs reused") {
				t.Fatalf("stdout = %q, want the no-op line", stdout)
			}
		})
	}
}

// TestPrepareEnvironmentGuard keeps the environment policy in the composed rule
// rather than a second copy: a caller policy value and an unreviewed input file
// are both refused before any child starts.
func TestPrepareEnvironmentGuard(t *testing.T) {
	tree := newPrepareFixture(t)
	t.Setenv("PNPM_CONFIG_TRUST_LOCKFILE", "false")
	_, _, err := tree.run(t, tree.prefix, "1.4.1")
	mustRefusal(t, err, "pnpm environment: unsupported package-manager environment: PNPM_CONFIG_TRUST_LOCKFILE")
	tree.expectNoChild(t)
	_ = os.Unsetenv("PNPM_CONFIG_TRUST_LOCKFILE")

	// The five reviewed defaults are allowed through; anything else is not.
	t.Setenv("PNPM_CONFIG_MINIMUM_RELEASE_AGE", "1440")
	if _, _, err := tree.run(t, tree.prefix, "1.4.1"); err != nil {
		t.Fatalf("a reviewed default was refused: %v", err)
	}
	_ = os.Unsetenv("PNPM_CONFIG_MINIMUM_RELEASE_AGE")

	tree2 := newPrepareFixture(t)
	tree2.write(t, "prefix/.npmrc", "trust-lockfile=true\n", 0o644)
	_, _, err = tree2.run(t, tree2.prefix, "1.4.1")
	mustRefusal(t, err, "pnpm environment: unreviewed package-manager input: .npmrc")
	tree2.expectNoChild(t)
}

// TestPrepareEqualSelectionIsANoOp is the build's path: an empty or equal
// selection validates the committed bytes and starts no manager.
func TestPrepareEqualSelectionIsANoOp(t *testing.T) {
	for _, selection := range []string{"", "1.4.1"} {
		tree := newPrepareFixture(t)
		before := tree.committedInputs(t)
		stdout, stderr, err := tree.run(t, tree.prefix, selection)
		if err != nil {
			t.Fatalf("%q: %v", selection, err)
		}
		if want := "prepare-lock: 1.4.1 unchanged; committed inputs reused\n"; stdout != want {
			t.Fatalf("%q: stdout = %q, want %q", selection, stdout, want)
		}
		if stderr != "" {
			t.Fatalf("%q: stderr = %q, want nothing", selection, stderr)
		}
		tree.expectInvocations(t, "node|"+filepath.Join(tree.root, "check-lock.mjs")+" "+tree.prefix)
		after := tree.committedInputs(t)
		for name, body := range before {
			if after[name] != body {
				t.Fatalf("%s changed on a no-op", name)
			}
		}
	}
}

// TestPrepareChangedSelectionRequiresRefresh is the rule that keeps a build from
// re-pinning what it verifies. The message is one a maintainer and
// upgrade-plugins.sh both point at, so its bytes are frozen.
func TestPrepareChangedSelectionRequiresRefresh(t *testing.T) {
	tree := newPrepareFixture(t)
	stdout, _, err := tree.run(t, tree.prefix, "1.4.2")
	mustRefusal(t, err,
		"prepare-lock: committed inputs pin 1.4.1, not 1.4.2; re-pin deliberately with upgrade-plugins.sh")
	if stdout != "" {
		t.Fatalf("a changed selection wrote %q to stdout", stdout)
	}
	// Only the checker: no manager, no pin writer.
	tree.expectInvocations(t, "node|")
}

// TestPrepareRefusesAnotherManager keeps the reviewed lock a reviewed artefact: a
// manager that reports another version, or fails, is refused and never installs.
func TestPrepareRefusesAnotherManager(t *testing.T) {
	for name, testCase := range map[string]struct{ key, value string }{
		"wrong version":   {"MANAGER_VERSION", "12.9.1"},
		"nothing printed": {"SILENT_MANAGER", "1"},
	} {
		t.Run(name, func(t *testing.T) {
			tree := newPrepareFixture(t)
			t.Setenv(testCase.key, testCase.value)
			if testCase.key == "SILENT_MANAGER" {
				// A manager that answers nothing, which is also what a bound that fires
				// leaves behind.
				tree.program(t, "pnpm", "#!/bin/sh\nexit 1\n")
			}
			_, _, err := tree.run(t, tree.prefix, "1.4.2", "--refresh-lock")
			mustRefusal(t, err, "prepare-lock: pnpm 11.11.0 is required; refusing another manager version")
			for _, invocation := range tree.invocations(t) {
				if strings.HasPrefix(invocation, "run-pnpm|") {
					t.Fatalf("%s: the manager ran anyway: %q", testCase.key, invocation)
				}
			}
		})
	}
}

// TestPrepareAcceptsAManagerThatStumbled keeps the reviewed comparison: it was the
// manager's own report and nothing else, so a failed `--version` that still printed
// the pin continues, and the resolution behind it is where a real failure surfaces.
func TestPrepareAcceptsAManagerThatStumbled(t *testing.T) {
	tree := newPrepareFixture(t)
	tree.program(t, "pnpm", `#!/bin/sh
if [ "$1" = --version ]; then
    printf '11.11.0\n'
    exit 1
fi
printf 'fixture lock\n' >pnpm-lock.yaml
`)
	if _, stderr, err := tree.run(t, tree.prefix, "1.4.2", "--refresh-lock"); err != nil {
		t.Fatalf("a manager that printed the pin was refused: %v (%s)", err, stderr)
	}
}

// TestPrepareIdentityBound keeps the manager identity check bounded: a manager that
// answers nothing must not hold a build open, and the session must not survive.
func TestPrepareIdentityBound(t *testing.T) {
	tree := newPrepareFixture(t)
	tree.program(t, "pnpm", "#!/bin/sh\ntrap '' TERM\nexec sleep 300\n")
	shrinkBounds(t, 100*time.Millisecond)
	started := time.Now()
	_, _, err := tree.run(t, tree.prefix, "1.4.2", "--refresh-lock")
	mustRefusal(t, err, "prepare-lock: pnpm 11.11.0 is required; refusing another manager version")
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("the identity check ran for %s", elapsed)
	}
}

// TestPrepareRefreshPublishesReviewedInputs is the developer path end to end.
func TestPrepareRefreshPublishesReviewedInputs(t *testing.T) {
	tree := newPrepareFixture(t)
	stdout, stderr, err := tree.run(t, tree.prefix, "1.4.2", "--refresh-lock")
	if err != nil {
		t.Fatalf("preparation = %v (%s)", err, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want the predecessor's silence", stdout)
	}
	tree.expectInvocations(t, "node|", "pnpm|--version", "run-pnpm|", "pnpm|install", "node|", "node|")
	invocations := tree.invocations(t)
	// The identity check is not exempt from the manager policy: a caller pnpmfile or
	// an ambient registry must not influence even `--version`.
	if want := "pnpm|--version --pm-on-fail=error --config.update-notifier=false"; invocations[1] != want {
		t.Fatalf("identity argv = %q, want %q", invocations[1], want)
	}
	if !strings.Contains(invocations[2], "install --lockfile-only --no-frozen-lockfile") {
		t.Fatalf("resolution argv = %q", invocations[2])
	}
	for _, flag := range []string{
		"--config.update-notifier=false",
		"--ignore-scripts", "--ignore-pnpmfile", "--pm-on-fail=error",
		"--registry=https://registry.npmjs.org/",
		"--store-dir",
	} {
		if !strings.Contains(invocations[2], flag) {
			t.Fatalf("resolution argv %q lost %q", invocations[2], flag)
		}
	}
	if !strings.Contains(invocations[5], "opencode-prepare-") {
		t.Fatalf("the staged project was not re-checked: %q", invocations[5])
	}
	// The manager never runs in the payload and never sees the caller's HOME.
	pwd, err := os.ReadFile(os.Getenv("PWD_FILE"))
	if err != nil {
		t.Fatalf("read manager pwd: %v", err)
	}
	if !strings.Contains(string(pwd), "opencode-prepare-") {
		t.Fatalf("manager working directory = %q, want the invocation scratch", pwd)
	}
	home, err := os.ReadFile(os.Getenv("HOME_FILE"))
	if err != nil {
		t.Fatalf("read manager home: %v", err)
	}
	if !strings.Contains(string(home), "opencode-prepare-") {
		t.Fatalf("manager HOME = %q, want the invocation's private state", home)
	}
	// The three reviewed inputs are republished from the staged project, including
	// the pins the developer-only writer just wrote.
	published := tree.committedInputs(t)
	if published["pnpm-inputs.sha256"] != "deadbeef  package.json\n" {
		t.Fatalf("published pins = %q, want the writer's bytes", published["pnpm-inputs.sha256"])
	}
	if published["pnpm-lock.yaml"] != "fixture lock\n" {
		t.Fatalf("published lock = %q, want the resolved lock", published["pnpm-lock.yaml"])
	}
	if !strings.Contains(published["package.json"], `"@openchamber/opencode-claude": "1.4.2"`) {
		t.Fatalf("published manifest = %q", published["package.json"])
	}
	// The workspace is never rewritten by a preparation: it is a reviewed input.
	if published["pnpm-workspace.yaml"] != committedWorkspace {
		t.Fatalf("workspace = %q, want the committed bytes", published["pnpm-workspace.yaml"])
	}
}

// TestPrepareInstallFailureLeavesCommittedInputs keeps a failed resolution from
// touching the bytes a build verifies.
func TestPrepareInstallFailureLeavesCommittedInputs(t *testing.T) {
	tree := newPrepareFixture(t)
	before := tree.committedInputs(t)
	t.Setenv("INSTALL_FAIL", "1")
	stdout, stderr, err := tree.run(t, tree.prefix, "1.4.2", "--refresh-lock")
	mustSilent(t, err, 1)
	if !strings.Contains(stderr, "pnpm: fixture install failure") {
		t.Fatalf("stderr = %q, want the manager's own diagnostic", stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q", stdout)
	}
	after := tree.committedInputs(t)
	for name, body := range before {
		if after[name] != body {
			t.Fatalf("%s changed after a failed resolution", name)
		}
	}
	for _, invocation := range tree.invocations(t) {
		if strings.Contains(invocation, "pin-pnpm-inputs") {
			t.Fatalf("the pin writer ran after a failed resolution: %q", invocation)
		}
	}
}

// TestPrepareStagedBytesAreRevalidated keeps the pins from being written over a
// project the checker would refuse.
func TestPrepareStagedBytesAreRevalidated(t *testing.T) {
	tree := newPrepareFixture(t)
	before := tree.committedInputs(t)
	t.Setenv("STAGED_CHECK_FAIL", "1")
	_, stderr, err := tree.run(t, tree.prefix, "1.4.2", "--refresh-lock")
	mustSilent(t, err, 1)
	if !strings.Contains(stderr, "bytes differ from the reviewed pin") {
		t.Fatalf("stderr = %q, want the checker's own diagnostic", stderr)
	}
	after := tree.committedInputs(t)
	for name, body := range before {
		if after[name] != body {
			t.Fatalf("%s changed after a refused staged graph", name)
		}
	}
}

// TestPrepareCheckerFailure keeps the checker's own status and bytes.
func TestPrepareCheckerFailure(t *testing.T) {
	tree := newPrepareFixture(t)
	t.Setenv("CHECK_FAIL", "package.json: unreviewed input")
	_, stderr, err := tree.run(t, tree.prefix, "1.4.1")
	mustSilent(t, err, 1)
	if stderr != "check-lock: package.json: unreviewed input\n" {
		t.Fatalf("stderr = %q, want the checker's own line", stderr)
	}
}

// TestPrepareScratchFailure reports a scratch that cannot be created instead of
// running the manager in an unrelated directory.
func TestPrepareScratchFailure(t *testing.T) {
	tree := newPrepareFixture(t)
	t.Setenv("TMPDIR", filepath.Join(tree.root, "absent-tmp"))
	_, _, err := tree.run(t, tree.prefix, "1.4.2", "--refresh-lock")
	if err == nil {
		t.Fatal("a scratch that cannot be created was reported as success")
	}
	if status, ok := silentStatus(err); ok && status == 0 {
		t.Fatalf("status = %d", status)
	}
}

// TestStageManifest pins the staged bytes: the document with one selection
// changed, every key in place.
func TestStageManifest(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "package.json")
	if err := os.WriteFile(source, []byte(committedManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "staged.json")
	if err := stageManifest(source, destination, "1.4.2"); err != nil {
		t.Fatalf("stage: %v", err)
	}
	staged, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != stagedManifest {
		t.Fatalf("staged manifest =\n%s\nwant\n%s", staged, stagedManifest)
	}
	// A manifest the checker would have refused (it asserts the dependency map)
	// cannot be staged either: the refusal exists so a fixture that replaced the
	// checker cannot publish a manifest with no dependencies.
	if err := os.WriteFile(source, []byte("{\"name\":\"x\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRefusal(t, stageManifest(source, destination, "1.4.2"), "prepare-lock: cannot stage package.json")
}

// TestAuthorizePrefix pins the physical-path rule as a unit, including the case a
// whole-run test cannot build: the production prefix itself.
func TestAuthorizePrefix(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The rule is about the physical path, so the expectation is physical too:
	// /tmp is a symlink to /private/tmp on a Darwin host.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "a", "b")
	outside := t.TempDir()
	escaping := filepath.Join(root, "escape")
	if err := os.Symlink(outside, escaping); err != nil {
		t.Fatal(err)
	}
	if physical, err := authorizePrefix(inside, root); err != nil || physical != inside {
		t.Fatalf("inside = %q, %v, want %q", physical, err, inside)
	}
	// The authorized root itself is not a prefix: the reviewed case matches
	// `"$root"/*` only.
	mustSilent(t, errOf(authorizePrefix(root, root)), 1)
	mustSilent(t, errOf(authorizePrefix(escaping, root)), 1)
	mustUsage(t, errOf(authorizePrefix(inside, "")), "non-production prefix requires fixture root")
	mustUsage(t, errOf(authorizePrefix(inside, filepath.Join(root, "absent"))), "cd: can't cd to "+filepath.Join(root, "absent"))
}

// TestAuthorizePrefixProductionDispatch drives the exemption itself, not only the
// decision: a prefix that a build may prepare with no fixture root must be accepted,
// and a sibling of it must not. The production path is the one directory a host test
// cannot create, so this test points the variable at one it can.
func TestAuthorizePrefixProductionDispatch(t *testing.T) {
	root := t.TempDir()
	production := filepath.Join(root, "plugins")
	if err := os.MkdirAll(production, 0o755); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(production)
	if err != nil {
		t.Fatal(err)
	}
	saved := productionPrefix
	productionPrefix = physical
	t.Cleanup(func() { productionPrefix = saved })

	// No fixture root: the exemption is what authorizes this.
	got, err := authorizePrefix(production, "")
	if err != nil || got != physical {
		t.Fatalf("authorizePrefix(production, no fixture root) = %q, %v, want %q and no error", got, err, physical)
	}
	// A sibling that merely starts with the production path is not exempt.
	sibling := filepath.Join(root, "plugins-x")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	siblingPhysical, err := filepath.EvalSymlinks(sibling)
	if err != nil {
		t.Fatal(err)
	}
	mustUsage(t, errOf(authorizePrefix(sibling, "")), "non-production prefix requires fixture root")
	if siblingPhysical == physical {
		t.Fatal("the fixture is broken: the sibling resolved to the production path")
	}
}

// TestIsProductionPrefix is the exemption's whole decision. The production path is
// the one input a host test cannot create, so the decision is a pure function and
// this is its table: a sibling that merely starts with the production name is not
// the image's directory, and neither is a child of it.
func TestIsProductionPrefix(t *testing.T) {
	for _, testCase := range []struct {
		path string
		want bool
	}{
		{"/opt/agent-vm/opencode-plugins", true},
		{"/opt/agent-vm/opencode-plugins-x", false},
		{"/opt/agent-vm/opencode-plugins/nested", false},
		{"/opt/agent-vm", false},
		{"/opt/agent-vm/opencode-plugins/", false},
		{"", false},
	} {
		if got := isProductionPrefix(testCase.path); got != testCase.want {
			t.Errorf("isProductionPrefix(%q) = %v, want %v", testCase.path, got, testCase.want)
		}
	}
}
