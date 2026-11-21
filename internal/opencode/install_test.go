package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The install hook's fixture children: the vendored installer, the composed
// directory's two programs, preparation and the manager. Each records what it was
// handed, so an assertion is about what the process did rather than about a mock.

// fakeVendoredInstaller is the vendored `install.sh`: it keeps what the hook exported
// and reproduces the two things the hook reasons about, a classified download
// receipt and a payload.
const fakeVendoredInstaller = `#!/bin/sh
printf 'install|%s\n' "$*" >>"$CALLS"
printf '%s|%s\n' "$HOME" "$VERSION" >>"$CALLS"
if [ -n "${FAKE_RECEIPT:-}" ] && [ -n "${FAKE_RECEIPT_PATH:-}" ]; then
    printf '%s\n' "$FAKE_RECEIPT" >"$FAKE_RECEIPT_PATH"
fi
if [ "${FAKE_EXIT:-0}" -eq 0 ]; then
    mkdir -p "$HOME/.opencode/bin" "$HOME/.opencode/share"
    chmod 0700 "$HOME/.opencode" "$HOME/.opencode/share"
    printf '#!/bin/sh\nexit 0\n' >"$HOME/.opencode/bin/opencode"
    chmod 0755 "$HOME/.opencode/bin/opencode"
    printf 'notice\n' >"$HOME/.opencode/share/README"
    chmod 0600 "$HOME/.opencode/share/README"
fi
exit "${FAKE_EXIT:-0}"
`

// installFixture is one install-hook case.
type installFixture struct {
	*fixture
	vendor    string
	receipt   string
	link      string
	statusDir string
}

func newInstallFixture(t *testing.T) *installFixture {
	t.Helper()
	tree := newFixture(t)
	state := &installFixture{
		fixture:   tree,
		vendor:    filepath.Join(tree.root, "vendor"),
		receipt:   filepath.Join(tree.root, "receipt"),
		link:      filepath.Join(tree.root, "link", "opencode"),
		statusDir: filepath.Join(tree.root, "status"),
	}
	if err := os.MkdirAll(state.vendor, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state.vendor, "install.sh"), []byte(fakeVendoredInstaller), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RECEIPT_PATH", state.receipt)
	return state
}

func (f *installFixture) options(slot string) InstallOptions {
	return InstallOptions{
		Vendor:    f.vendor,
		Prefix:    f.prefix,
		Link:      f.link,
		Slot:      slot,
		Receipt:   f.receipt,
		StatusDir: f.statusDir,
	}
}

// run executes one install with captured streams.
func (f *installFixture) run(t *testing.T, options InstallOptions) (string, string, error) {
	t.Helper()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	options.Out, options.Err = out, errOut
	err := Install(context.Background(), options)
	return out.String(), errOut.String(), err
}

func (f *installFixture) statusRecord(t *testing.T) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.statusDir, recordName))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read the absence record: %v", err)
	}
	return string(data), true
}

// TestInstallPublishesOnSuccess is the success contract: the vendored installer runs
// as a child with the prefix as its HOME and the stripped version, the payload becomes
// readable and executable as the predecessor's `chmod -R a+rX` made it, and the link
// the image documents is published. No record is written: this hook never claims
// `installed`.
func TestInstallPublishesOnSuccess(t *testing.T) {
	tree := newInstallFixture(t)
	stdout, stderr, err := tree.run(t, tree.options("v2.0.29"))
	if err != nil {
		t.Fatalf("install = %v (stderr %q)", err, stderr)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("a clean install wrote %q / %q", stdout, stderr)
	}
	tree.expectInvocations(t, "install|--no-modify-path", tree.prefix+"|2.0.29")
	if _, recorded := tree.statusRecord(t); recorded {
		t.Fatal("the install hook wrote a record; only the gate records")
	}
	target, err := os.Readlink(tree.link)
	if err != nil {
		t.Fatalf("read the published link: %v", err)
	}
	if want := filepath.Join(tree.prefix, ".opencode", "bin", "opencode"); target != want {
		t.Fatalf("link target = %q, want %q", target, want)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(tree.prefix, ".opencode"):                    0o755,
		filepath.Join(tree.prefix, ".opencode", "share"):           0o755,
		filepath.Join(tree.prefix, ".opencode", "share", "README"): 0o644,
		filepath.Join(tree.prefix, ".opencode", "bin", "opencode"): 0o755,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %04o, want %04o", path, info.Mode().Perm(), want)
		}
	}
}

// TestInstallSlotGrammar is the reviewed selection: `v` and canonical exact semver,
// major 2 only, and every refusal happens before the child starts.
func TestInstallSlotGrammar(t *testing.T) {
	cases := []struct{ slot, want string }{
		// The `v` spelling is the contract's, not a convenience.
		{"2.0.29", "install-opencode.sh: expected v<version>, got '2.0.29'"},
		{"latest", "install-opencode.sh: expected v<version>, got 'latest'"},
		{"^2.0.29", "install-opencode.sh: expected v<version>, got '^2.0.29'"},
		// The canonical grammar, after the `v`.
		{"v1.18", "install-opencode.sh: not an exact semver version: 'v1.18'"},
		{"v2.0.29 ", "install-opencode.sh: not an exact semver version: 'v2.0.29 '"},
		{"v02.0.29", "install-opencode.sh: not an exact semver version: 'v02.0.29'"},
		{"v2.0.29+build!", "install-opencode.sh: not an exact semver version: 'v2.0.29+build!'"},
		// An embedded newline is the case the line-oriented match would have passed.
		{"v2.0.29\n2.0.30", "install-opencode.sh: not an exact semver version: 'v2.0.29\n2.0.30'"},
		{"v2.0.29\n", "install-opencode.sh: not an exact semver version: 'v2.0.29\n'"},
		// Only major 2 is reviewed.
		{"v1.18.34", "install-opencode.sh: only major 2 is reviewed: 'v1.18.34'"},
		{"v3.0.0", "install-opencode.sh: only major 2 is reviewed: 'v3.0.0'"},
		{"v20.0.1", "install-opencode.sh: only major 2 is reviewed: 'v20.0.1'"},
	}
	for _, testCase := range cases {
		t.Run(strings.ReplaceAll(testCase.slot, "\n", "\\n"), func(t *testing.T) {
			tree := newInstallFixture(t)
			_, _, err := tree.run(t, tree.options(testCase.slot))
			mustRefusal(t, err, testCase.want)
			tree.expectNoChild(t)
		})
	}
	// An empty slot is the entry point's `${:?}`: usage, with its text.
	tree := newInstallFixture(t)
	_, _, err := tree.run(t, tree.options(""))
	mustUsage(t, err, installSlotRefusal)
	tree.expectNoChild(t)
}

// TestInstallAcceptsExactMajorTwo keeps the grammar from being stricter than the
// reviewed one: a prerelease and build metadata are exact versions.
func TestInstallAcceptsExactMajorTwo(t *testing.T) {
	for _, slot := range []string{"v2.0.29", "v2.0.29-rc.1", "v2.0.29+build.7", "v2.0.0-0.3.0"} {
		t.Run(slot, func(t *testing.T) {
			tree := newInstallFixture(t)
			if _, _, err := tree.run(t, tree.options(slot)); err != nil {
				t.Fatalf("%q: %v, want the slot accepted", slot, err)
			}
			tree.expectInvocations(t, "install|--no-modify-path", tree.prefix+"|"+strings.TrimPrefix(slot, "v"))
		})
	}
}

// TestInstallRequiresVendoredInstaller: an absent vendor tree is this hook's own
// diagnostic, and the selection above it has already run.
func TestInstallRequiresVendoredInstaller(t *testing.T) {
	tree := newInstallFixture(t)
	if err := os.Remove(filepath.Join(tree.vendor, "install.sh")); err != nil {
		t.Fatal(err)
	}
	_, _, err := tree.run(t, tree.options("v2.0.29"))
	mustRefusal(t, err, "install-opencode.sh: vendored installer missing at "+filepath.Join(tree.vendor, "install.sh"))
	tree.expectNoChild(t)
}

// TestInstallSoftenTakesAllThree is the softening branch and its three preconditions.
// A classified transport failure that a receipt names, with a non-empty
// AGENT_INSTALL_SOFT_FAIL, is the only combination that may degrade the layer.
func TestInstallSoftenTakesAllThree(t *testing.T) {
	t.Run("classifies and softens", func(t *testing.T) {
		tree := newInstallFixture(t)
		options := tree.options("v2.0.29")
		options.SoftFail = "1"
		t.Setenv("FAKE_EXIT", "75")
		t.Setenv("FAKE_RECEIPT", "transport download 6")
		// The recipe's own partial artifacts, both present before the run.
		if err := os.MkdirAll(filepath.Join(tree.prefix, ".opencode"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(tree.link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/nowhere", tree.link); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := tree.run(t, options)
		if err != nil {
			t.Fatalf("install = %v (stderr %q)", err, stderr)
		}
		if want := "==> opencode: ABSENT (transport download 6; soft-fail mode)\n"; stdout != want {
			t.Fatalf("stdout = %q, want %q", stdout, want)
		}
		record, recorded := tree.statusRecord(t)
		if !recorded || record != "absent-transport 6\n" {
			t.Fatalf("record = %q, %v, want %q", record, recorded, "absent-transport 6\n")
		}
		if _, err := os.Lstat(filepath.Join(tree.prefix, ".opencode")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the partial install survived: %v", err)
		}
		if _, err := os.Lstat(tree.link); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the partial link survived: %v", err)
		}
	})
	hard := []struct {
		name    string
		status  string
		receipt string
		soft    string
		path    bool
	}{
		// Soft-fail mode is the caller's decision, not the receipt's.
		{name: "no soft-fail mode", status: "75", receipt: "transport download 6", soft: ""},
		// A bare 75 is not a classified download failure.
		{name: "no receipt content", status: "75", receipt: "", soft: "1"},
		// A receipt that does not classify.
		{name: "unclassified receipt", status: "75", receipt: "download failed 6", soft: "1"},
		{name: "receipt without a code", status: "75", receipt: "transport download", soft: "1"},
		// The status has to be the classified one, whatever the receipt says.
		{name: "other status", status: "3", receipt: "transport download 6", soft: "1"},
		// A receipt the build never named cannot classify anything.
		{name: "no receipt path", status: "75", receipt: "transport download 6", soft: "1", path: true},
	}
	for _, testCase := range hard {
		t.Run(testCase.name, func(t *testing.T) {
			tree := newInstallFixture(t)
			options := tree.options("v2.0.29")
			options.SoftFail = testCase.soft
			if testCase.path {
				options.Receipt = ""
			}
			t.Setenv("FAKE_EXIT", testCase.status)
			t.Setenv("FAKE_RECEIPT", testCase.receipt)
			// A partial tree from an earlier attempt: only the softening branch is
			// allowed to remove it.
			marker := filepath.Join(tree.prefix, ".opencode", "partial")
			if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, err := tree.run(t, options)
			mustRefusal(t, err, "install-opencode.sh: opencode installer failed (exit "+testCase.status+")")
			if _, recorded := tree.statusRecord(t); recorded {
				t.Fatal("a hard failure wrote an absence record")
			}
			// A hard failure removes nothing: the artifacts are what an operator
			// inspects, and no record authorises anything.
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("a hard failure removed the install directory: %v", err)
			}
		})
	}
	t.Run("missing receipt file", func(t *testing.T) {
		tree := newInstallFixture(t)
		options := tree.options("v2.0.29")
		options.SoftFail = "1"
		options.Receipt = filepath.Join(tree.root, "absent-receipt")
		t.Setenv("FAKE_EXIT", "75")
		_, _, err := tree.run(t, options)
		mustRefusal(t, err, "install-opencode.sh: opencode installer failed (exit 75)")
	})
}

// TestPublishPermissionsDoesNotFollowLinks is the `chmod -R a+rX` boundary: a symlink
// inside the payload must not change the mode of what it points at. os.Chmod follows
// one where `chmod -R` never did, so the skip is the rule and not a lint preference.
func TestPublishPermissionsDoesNotFollowLinks(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "secret")
	if err := os.WriteFile(target, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "payload")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "readme")
	if err := os.WriteFile(inside, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := publishPermissions(root); err != nil {
		t.Fatalf("publishPermissions = %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("a symlink changed the mode of %s (outside the payload) to %04o", target, info.Mode().Perm())
	}
	// The payload itself is still published, so the skip is not a no-op.
	info, err = os.Stat(inside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("payload file mode = %04o, want 0644", info.Mode().Perm())
	}
}

// TestTransportCodeReadsTheClassifiedLine: the receipt's own grammar decides, and the
// code is the third field of that line.
func TestTransportCodeReadsTheClassifiedLine(t *testing.T) {
	for _, testCase := range []struct {
		name, receipt string
		code          string
		classified    bool
	}{
		{"one line", "transport download 28\n", "28", true},
		{"unterminated", "transport download 60", "60", true},
		{"prefix only", "transport download\n", "", false},
		{"other line first", "retrying\ntransport download 7\n", "7", true},
		{"two classified lines", "transport download 6\ntransport download 7\n", "6", true},
		{"unknown code", "transport download 999\n", "999", true},
		{"leading space", " transport download 6\n", "", false},
		{"empty", "", "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "receipt")
			if err := os.WriteFile(path, []byte(testCase.receipt), 0o644); err != nil {
				t.Fatal(err)
			}
			code, classified := transportCode(path)
			if classified != testCase.classified || code != testCase.code {
				t.Fatalf("transportCode = %q, %v; want %q, %v", code, classified, testCase.code, testCase.classified)
			}
		})
	}
}
