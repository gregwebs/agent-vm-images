package opencode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// InstallOptions is one owning install hook invocation.
type InstallOptions struct {
	// Vendor is the read-only mount carrying the patched upstream installer.
	Vendor string
	// Prefix is the installer's HOME and Link the command this layer publishes.
	Prefix string
	Link   string
	// Slot is the layer's requested selection: `v` and canonical exact semver.
	Slot string
	// Receipt is the classified-download receipt the vendored installer writes.
	// Empty means the build named none, and nothing may be softened.
	Receipt string
	// SoftFail is AGENT_INSTALL_SOFT_FAIL. Only a non-empty value may soften, and
	// only a transport failure the receipt classifies.
	SoftFail string
	// StatusDir is where a softened absence is recorded.
	StatusDir string
	Out, Err  io.Writer
}

// The retired hook's diagnostics, byte for byte: a build log and the black-box
// harness both quote them.
const (
	installHook = "install-opencode.sh"
	// installSlotRefusal is the entry point's `${AGENT_VM_VERSION_OPENCODE:?…}`.
	installSlotRefusal = installHook + " needs AGENT_VM_VERSION_OPENCODE"
	// absentRecordPrefix is what the gate above this hook reads: a record this hook
	// wrote is the only degraded state the gate accepts.
	absentRecordPrefix = "absent-transport "
)

// Install runs the owning install hook.
//
// The vendored installer is the only download path: it keeps OpenCode's own
// transport, with the exact-selection, classified-download and
// already-installed-skip patches of vendor/install.patch. It therefore runs as a
// child, not through run-install's interpreter, because only a child's status lets
// this hook see the classified 75 and decide.
//
// Nothing here is a second owner of a rule the child already owns: the child
// selects, downloads and installs. This program owns the slot grammar, the child's
// environment, the publication the layer promises, and the one softening the
// contract allows.
func Install(ctx context.Context, options InstallOptions) error {
	numeric, err := reviewedSelection(options.Slot)
	if err != nil {
		return err
	}
	installer := filepath.Join(options.Vendor, "install.sh")
	// `[ -r ]` before the child: a missing vendor tree is this hook's diagnostic, not
	// bash's, and the selection above has already run.
	if !readable(installer) {
		return refusal(installHook + ": vendored installer missing at " + installer)
	}
	// --no-modify-path is the installer's documented headless option: this layer
	// already puts the binary on PATH through ENV and the published link, so writing
	// shell profiles during a build would only add build-local noise.
	_, status := runChild(ctx, childSpec{
		argv:        []string{"bash", installer, "--no-modify-path"},
		stdin:       os.Stdin,
		stdout:      options.Out,
		stderr:      options.Err,
		environment: installEnvironment(options.Prefix, numeric),
	})
	if status == 0 {
		return publishInstalled(options)
	}
	code, softened := softAbsence(status, options)
	if !softened {
		return refusalf("%s: opencode installer failed (exit %d)", installHook, status)
	}
	// Recipe-owned partial artifacts only: the install directory and the link this
	// hook would have published. The absence record is what makes the degraded image
	// certifiable, so it is written last.
	if err := os.RemoveAll(filepath.Join(options.Prefix, ".opencode")); err != nil {
		return refusalf("%s: cannot remove the partial install: %s", installHook, err)
	}
	if err := os.Remove(options.Link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return refusalf("%s: cannot remove the partial command link: %s", installHook, err)
	}
	if err := os.MkdirAll(options.StatusDir, 0o755); err != nil {
		return refusalf("%s: cannot write the absence record: %s", installHook, err)
	}
	if err := writeFile(filepath.Join(options.StatusDir, recordName), absentRecordPrefix+code+"\n"); err != nil {
		return refusalf("%s: cannot write the absence record: %s", installHook, err)
	}
	say(options.Out, "==> opencode: ABSENT (transport download %s; soft-fail mode)\n", code)
	return nil
}

// reviewedSelection is the slot grammar: `v` and canonical exact semver, major 2
// only. The stripped numeric version is what the installer receives.
func reviewedSelection(slot string) (string, error) {
	if slot == "" {
		// The entry point's `${:?}` refuses an unset or empty slot before this program
		// runs; a direct caller gets the same status and the same text, because a
		// missing required variable is a usage error under the shell that ran it.
		return "", &Usage{Text: installSlotRefusal}
	}
	numeric, prefixed := strings.CutPrefix(slot, "v")
	if !prefixed {
		return "", refusalf("%s: expected v<version>, got '%s'", installHook, slot)
	}
	// exactVersion is the reviewed grammar, and it refuses an embedded newline before
	// the grammar. The anchored pattern would refuse one on its own -- Go's `$` is the
	// end of the text, where the predecessor's `grep` matched line by line -- so the
	// separate test is the predecessor's order kept, not a second defence.
	if !exactVersion(numeric) {
		return "", refusalf("%s: not an exact semver version: '%s'", installHook, slot)
	}
	// The predecessor's `case "$numeric" in 2.*)`: after the grammar that prefix is
	// the major component, so this is the major gate and not a range test.
	if !strings.HasPrefix(numeric, "2.") {
		return "", refusalf("%s: only major 2 is reviewed: '%s'", installHook, slot)
	}
	return numeric, nil
}

// installEnvironment is this process's environment with the two values the vendor
// installer reads applied: HOME is the install root and VERSION the selection.
func installEnvironment(prefix, numeric string) []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if key == "HOME" || key == "VERSION" {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "HOME="+prefix, "VERSION="+numeric)
}

// softAbsence is the whole softening decision: a classified transport status, a
// receipt that classifies it, and a non-empty AGENT_INSTALL_SOFT_FAIL. Anything else
// stays hard, whatever the receipt says.
func softAbsence(status int, options InstallOptions) (string, bool) {
	if status != classifiedTransportStatus || options.Receipt == "" || options.SoftFail == "" {
		return "", false
	}
	return transportCode(options.Receipt)
}

// receiptPattern is the classified-download receipt `download.sh` writes. It is the
// vocabulary, not this hook's invention: a tool that happens to exit 75 without one
// of these lines is an ordinary failure.
var receiptPattern = regexp.MustCompile(`^transport download [0-9]+$`)

// transportCode reports the code a receipt records, and whether the receipt
// classifies anything at all.
//
// The predecessor scanned the file with `grep -Eq`, so one matching line anywhere was
// enough, and took the code from that line's third field. `download.sh` truncates the
// receipt and writes exactly one line, so a receipt naming two classified failures is
// not something a build produces; the first matching line is the one this reads.
func transportCode(path string) (string, bool) {
	source, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(source), "\n"), "\n") {
		if !receiptPattern.MatchString(line) {
			continue
		}
		// The pattern's three space-separated fields are what makes this index safe.
		return strings.Fields(line)[2], true
	}
	return "", false
}

// publishInstalled is the layer's promise after a successful install: the payload is
// readable by the read-only runtime UIDs and the command the image documents exists.
func publishInstalled(options InstallOptions) error {
	// `set -e` made a failed publish the hook's own failure, and the mode is what the
	// runtime UIDs need to load the command at all.
	if err := publishPermissions(filepath.Join(options.Prefix, ".opencode")); err != nil {
		return refusalf("%s: cannot publish the installed command: %s", installHook, err)
	}
	if err := os.MkdirAll(filepath.Dir(options.Link), 0o755); err != nil {
		return refusalf("%s: cannot publish the installed command: %s", installHook, err)
	}
	if err := replaceLink(filepath.Join(options.Prefix, ".opencode", "bin", "opencode"), options.Link); err != nil {
		return refusalf("%s: cannot publish the installed command: %s", installHook, err)
	}
	return nil
}

// replaceLink is `ln -sf TARGET LINK`: an existing destination is replaced, and a
// destination that is a directory receives the link beneath it, which is what `ln`
// does with one operand.
func replaceLink(target, link string) error {
	if info, err := os.Lstat(link); err == nil && info.IsDir() {
		link = filepath.Join(link, filepath.Base(target))
	}
	if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Symlink(target, link)
}

// publishPermissions is `chmod -R a+rX`: readable by the read-only runtime UIDs, and
// executable exactly where it already was.
//
// internal/dsh owns the same rule for its payload. Two owning hooks sharing one
// predicate is not yet a shared package; the copy is deliberate and named here so a
// third consumer extracts it instead of writing a fourth.
func publishPermissions(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		mode |= 0o444
		if info.IsDir() || mode&0o111 != 0 {
			mode |= 0o111
		}
		if mode == info.Mode().Perm() {
			return nil
		}
		return os.Chmod(path, mode)
	})
}

// readable is `[ -r ]`: this process may open the path. The predecessor tested it
// before running anything, so an absent or unreadable vendor tree is one diagnostic
// from this program.
func readable(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	return file.Close() == nil
}
