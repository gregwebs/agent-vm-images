// Package agypin reads what the agy layer's gates need, offline.
//
// agy publishes no signature, so the pin is the whole selection and the install
// hook's only job at that point is to refuse an exact-but-unpinned version *before*
// a fetch. The build gate's job is different and sharper: agy keeps running when an
// install is subtly wrong, so the installed command's mode, owner and the release's
// own self-update switch are re-read from the published bytes rather than assumed
// from the pin.
package agypin

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
)

// Refusal is one refused input.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

func refuse(format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...)}
}

// Marker is the environment variable agy's release reads to disable self-update.
//
// The image sets it, so the gate requiring it is why a release that renamed the
// variable fails the bump: a release that no longer reads it would ship a binary
// that silently updates itself in a guest.
const Marker = "AGY_CLI_DISABLE_AUTO_UPDATE"

// Reviewed mode of the published command. Every guest UID runs it, and an image
// whose /usr/local/bin/agy is group- or world-writable lets one guest replace what
// another executes. The shared installer already refuses an archive member that is
// not 0755, so this asserts the published result rather than a second rule.
const ReviewedMode os.FileMode = 0o755

// Architectures the pin has an archive for, which is the only set an install may
// name.
var Architectures = []string{"amd64", "arm64"}

// CheckSelection reads the committed pin and requires it to select exactly the
// requested version for a supported architecture.
//
// The pin is read through the same validator the installer uses, so a pin file the
// installer would reject cannot select a version this gate then claims is pinned.
func CheckSelection(pinDir, version, arch string) (artifactpin.Version, error) {
	if !ExactVersion(version) {
		return artifactpin.Version{}, refuse("not an exact agy version: '%s'", version)
	}
	supported := false
	for _, candidate := range Architectures {
		if arch == candidate {
			supported = true
		}
	}
	if !supported {
		return artifactpin.Version{}, refuse("unsupported architecture '%s'", arch)
	}
	path := filepath.Join(pinDir, "pin.json")
	pin, err := artifactpin.Load(path)
	if err != nil {
		return artifactpin.Version{}, refuse("the committed agy pin at %s is not readable: %s",
			path, err)
	}
	if pin.Name != artifactpin.Agy {
		return artifactpin.Version{}, refuse("%s pins %s, not agy", path, pin.Name)
	}
	requested, err := artifactpin.ParseVersion(version, "--version")
	if err != nil {
		return artifactpin.Version{}, refuse("not an exact agy version: '%s'", version)
	}
	if pin.Version != requested {
		return artifactpin.Version{}, refuse("the committed agy pin selects %s, not %s",
			pin.Version, version)
	}
	if _, err := pin.Archive(artifactpin.LinuxAMD64); err != nil {
		return artifactpin.Version{}, err
	}
	if _, err := pin.Archive(artifactpin.LinuxARM64); err != nil {
		return artifactpin.Version{}, err
	}
	return pin.Version, nil
}

// ExactVersion reports whether text is an exact X.Y.Z version with no surrounding
// bytes: the hook refuses a multi-line value whose first line looks exact, and a
// trailing space, before anything acts on it.
func ExactVersion(text string) bool {
	if text == "" || text != trimSpace(text) {
		return false
	}
	_, err := artifactpin.ParseVersion(text, "version")
	return err == nil
}

func trimSpace(text string) string {
	start, end := 0, len(text)
	for start < end && isSpace(text[start]) {
		start++
	}
	for end > start && isSpace(text[end-1]) {
		end--
	}
	return text[start:end]
}

func isSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r'
}

// CheckInstalled is the build gate's view of the published command: a regular file
// this invocation may execute, at exactly the reviewed mode and the expected owner,
// and a release that still names the self-update switch.
func CheckInstalled(bin string, uid, gid int) error {
	info, err := os.Lstat(bin)
	if err != nil {
		return refuse("cannot read the mode and owner of %s: %s", bin, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return refuse("%s is not a regular file", bin)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return refuse("cannot read the filesystem identity of %s", bin)
	}
	mode := os.FileMode(stat.Mode & 0o7777)
	found := fmt.Sprintf("%03o %d:%d", mode, stat.Uid, stat.Gid)
	want := fmt.Sprintf("%03o %d:%d", ReviewedMode, uid, gid)
	if found != want {
		return refuse("%s is '%s', want '%s'", bin, found, want)
	}
	named, err := namesMarker(bin, Marker)
	if err != nil {
		return refuse("cannot read %s: %s", bin, err)
	}
	if !named {
		return refuse("%s no longer names %s; the pinned release would "+
			"self-update in a guest -- review the new release", bin, Marker)
	}
	return nil
}

// chunkBytes is how much of the command is read at a time.
const chunkBytes = 1 << 20

// namesMarker streams the file looking for the exact byte string. The command is a
// 234 MB binary, so this reads it in chunks with an overlap instead of holding it,
// and it searches bytes rather than lines because the embedded string carries no
// newline of its own.
func namesMarker(path, marker string) (bool, error) {
	handle, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = handle.Close() }()
	return containsMarker(handle, []byte(marker), chunkBytes)
}

// containsMarker is namesMarker's streaming search with the chunk size named, so a
// test can put the marker across a chunk boundary instead of writing a megabyte.
func containsMarker(reader io.Reader, needle []byte, chunk int) (bool, error) {
	if len(needle) == 0 {
		return true, nil
	}
	overlap := len(needle) - 1
	tail := make([]byte, 0, overlap)
	buffer := make([]byte, chunk)
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			window := append(append(make([]byte, 0, len(tail)+read), tail...), buffer[:read]...)
			if bytes.Contains(window, needle) {
				return true, nil
			}
			if len(window) > overlap {
				tail = append(tail[:0], window[len(window)-overlap:]...)
			} else {
				tail = append(tail[:0], window...)
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}
