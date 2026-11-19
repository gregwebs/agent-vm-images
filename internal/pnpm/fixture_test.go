package pnpm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// reviewedLauncher is the shell the base publishes as /usr/local/bin/pnpm. The
// verifier compares the installed file with this one byte for byte, so the fixture
// must publish exactly these bytes.
const reviewedLauncher = "#!/bin/sh\nexec /usr/bin/node /opt/agent-vm/pnpm/bin/pnpm.mjs \"$@\"\n"

// install is one staged fixture image: the reviewed source mount, the archive the
// pin authenticates, and the two publication directories inside an
// invocation-owned root.
type install struct {
	root        string
	destination string
	source      string
	prefix      string
	commands    string
	archive     string
	pin         Pin
}

// stageInstall builds a fixture image root whose source mount carries a valid pin
// for a reviewed-shape archive. Nothing is published yet.
func stageInstall(t *testing.T) *install {
	t.Helper()
	// Every fixture path is physical: the audit walks the resolution chain, so a
	// lexical temporary directory would audit the symlinked /tmp above it, which
	// no image path ever carries.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("cannot resolve the fixture root: %v", err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("cannot open the fixture root: %v", err)
	}
	destination := filepath.Join(root, "image")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatalf("cannot create the fixture destination: %v", err)
	}
	members := reviewedMembers()
	archive := writeArchive(t, members)
	pin := archivePin(t, archive, members)
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("cannot create the source mount: %v", err)
	}
	writeSource(t, source, pin, reviewedLauncher)
	return &install{
		root:        root,
		destination: destination,
		source:      source,
		prefix:      filepath.Join(destination, "opt/agent-vm/pnpm"),
		commands:    filepath.Join(destination, "usr/local/bin"),
		archive:     archive,
		pin:         pin,
	}
}

// writeSource writes the reviewed pin and launcher into a source mount.
func writeSource(t *testing.T, source string, pin Pin, launcher string) {
	t.Helper()
	writePinText(t, filepath.Join(source, "pin.json"), pin)
	if err := os.WriteFile(filepath.Join(source, "launcher.sh"), []byte(launcher), 0o644); err != nil {
		t.Fatalf("cannot write the launcher: %v", err)
	}
}

// writePinText writes a self-consistent pin document.
func writePinText(t *testing.T, path string, pin Pin) {
	t.Helper()
	document := map[string]any{
		"version":        pin.Version,
		"url":            pin.URL,
		"integrity":      pin.Integrity,
		"sha512":         pin.SHA512,
		"bundleSha256":   pin.BundleSHA256,
		"upstreamTag":    pin.UpstreamTag,
		"upstreamCommit": pin.UpstreamCommit,
	}
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("cannot marshal the pin: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("cannot write the pin: %v", err)
	}
}

// publish runs the bootstrap against the staged fixture.
func (i *install) publish(t *testing.T, archive string) error {
	t.Helper()
	t.Setenv(FixtureRootVariable, i.root)
	t.Setenv(FixtureDestinationVariable, i.destination)
	return Bootstrap(context.Background(), Options{SourceDir: i.source, FixtureArchive: archive})
}

// installed stages the distribution and returns the audited target with a PATH
// that resolves the published commands, which is the fixture layout the image
// certification runs against.
func (i *install) installed(t *testing.T) Target {
	t.Helper()
	if err := i.publish(t, i.archive); err != nil {
		t.Fatalf("bootstrap refused the reviewed fixture: %v", err)
	}
	node := filepath.Join(i.destination, "usr/bin/node")
	if err := os.MkdirAll(filepath.Dir(node), 0o755); err != nil {
		t.Fatalf("cannot create the interpreter directory: %v", err)
	}
	if err := os.WriteFile(node, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("cannot write the interpreter: %v", err)
	}
	t.Setenv("PATH", i.commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	root, err := filepath.EvalSymlinks(i.destination)
	if err != nil {
		t.Fatalf("cannot resolve the fixture destination: %v", err)
	}
	return Target{
		Prefix:      i.prefix,
		Commands:    i.commands,
		Node:        node,
		FixtureRoot: root,
	}
}
