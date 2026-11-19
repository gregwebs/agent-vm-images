package pnpm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gregwebs/agent-vm-images/internal/fetch"
)

// Options is one bootstrap: authenticate the pinned archive, publish the
// distribution into the image root, and leave the launcher and its aliases in the
// command directory.
type Options struct {
	// SourceDir is the read-only mount carrying pin.json, launcher.sh and
	// node-version-check.js.
	SourceDir string
	// FixtureArchive replaces the download with bytes the caller already holds.
	// It is the hermetic stand-in for internal/fetch, so a suite can drive this
	// program without a network and without shimming a downloader onto PATH.
	FixtureArchive string
	// Fetch is the HTTP client. Nil means the reviewed policy in internal/fetch.
	Fetch *fetch.Client
}

// Bootstrap installs the distribution the pin selects.
//
// Order is load-bearing. The pin is validated before anything is fetched, the
// archive is authenticated before a byte is decoded, the whole member inventory
// is validated before extraction, and the extracted tree is checked against the
// reviewed selection before the destination is created. A run that fails for any
// of those reasons leaves no destination behind.
func Bootstrap(ctx context.Context, options Options) error {
	source := options.SourceDir
	if source == "" {
		source = SourceDir()
	}
	pin, err := LoadPin(filepath.Join(source, "pin.json"))
	if err != nil {
		return err
	}
	fixture := os.Getenv(FixtureDestinationVariable)
	prefix, commands, production, err := publicationTargets(fixture)
	if err != nil {
		return err
	}
	published := append([]string{prefix}, commandPaths(commands)...)
	for _, path := range published {
		if _, err := os.Lstat(path); err == nil {
			return refuseText("refusing conflicting destination: " + path)
		}
	}
	scratch, err := os.MkdirTemp("", "pnpm-bootstrap-")
	if err != nil {
		return refuseCause(err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	archive := filepath.Join(scratch, "pnpm.tgz")
	payload, err := archivePayload(ctx, options, pin)
	if err != nil {
		return err
	}
	if err := os.WriteFile(archive, payload, 0o600); err != nil {
		return refuseCause(err)
	}
	if actual := Sum512(payload); actual != pin.SHA512 {
		return refuseText("archive SHA-512 mismatch")
	}
	packageDir, err := extract(archive, scratch, pin)
	if err != nil {
		return err
	}
	// The interpreter is checked before publication in a real image: a Node that
	// cannot run the entrypoint would leave a launcher that fails at use time.
	if production {
		if err := run(ProductionNode, filepath.Join(source, "node-version-check.js")); err != nil {
			return err
		}
	}
	if err := publish(source, packageDir, prefix, commands); err != nil {
		return err
	}
	if production {
		// The launcher is exercised once through its published name, so an image
		// that cannot answer --version fails the build instead of the first user.
		if err := run(filepath.Join(commands, "pnpm"), "--version"); err != nil {
			return err
		}
	}
	return nil
}

// publicationTargets resolves where this run publishes. A fixture root is
// authorized by FixtureDestination; a production install requires root because
// the published bytes must be image-owned.
func publicationTargets(fixture string) (prefix, commands string, production bool, err error) {
	if fixture != "" {
		prefix, commands, err = FixtureDestination(fixture)
		return prefix, commands, false, err
	}
	if os.Getuid() != 0 {
		return "", "", false, refuseText("production bootstrap requires root")
	}
	return ProductionPrefix, ProductionCommands, true, nil
}

// archivePayload is the bytes the pin authenticates: the named fixture archive
// when the caller supplied one, otherwise the pinned URL through internal/fetch.
func archivePayload(ctx context.Context, options Options, pin Pin) ([]byte, error) {
	if options.FixtureArchive == "" {
		client := options.Fetch
		if client == nil {
			client = fetch.New()
		}
		return client.Get(ctx, pin.URL, MaxArchiveBytes)
	}
	if !filepath.IsAbs(options.FixtureArchive) {
		return nil, refuseText("fixture archive must be an absolute path: " + options.FixtureArchive)
	}
	payload, err := os.ReadFile(options.FixtureArchive)
	if err != nil {
		// A missing or unreadable fixture archive is the hermetic stand-in for a
		// failed download, and it is a refusal like any other.
		return nil, refuseCause(err)
	}
	return payload, nil
}

// publish copies the authenticated tree into the image root and normalizes what
// the upstream archive may not decide: the exact launcher, its relative aliases
// and the modes a caller needs to read and traverse the distribution.
func publish(source, packageDir, prefix, commands string) error {
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		return refuseCause(err)
	}
	if err := os.MkdirAll(commands, 0o755); err != nil {
		return refuseCause(err)
	}
	if err := copyTree(packageDir, prefix); err != nil {
		return err
	}
	if err := normalizeModes(prefix); err != nil {
		return err
	}
	for _, name := range []string{"pnpm.mjs", "pnpx.mjs"} {
		if err := os.Chmod(filepath.Join(prefix, "bin", name), 0o755); err != nil {
			return refuseCause(err)
		}
	}
	// The pin ships beside the distribution so the verifier can compare the
	// installed selection with the reviewed one without trusting a label.
	if err := copyFile(filepath.Join(source, "pin.json"), filepath.Join(prefix, "pin.json"), 0o644); err != nil {
		return err
	}
	for _, directory := range []string{filepath.Dir(prefix), filepath.Dir(filepath.Dir(prefix)), commands} {
		if err := os.Chmod(directory, 0o755); err != nil {
			return refuseCause(err)
		}
	}
	launcher := filepath.Join(commands, "pnpm")
	if err := copyFile(filepath.Join(source, "launcher.sh"), launcher, 0o755); err != nil {
		return err
	}
	// The aliases are relative links to the one launcher, so the launcher cannot
	// be replaced while an alias keeps pointing at a different program.
	for _, name := range []string{"pnx", "pnpx"} {
		if err := os.Symlink("pnpm", filepath.Join(commands, name)); err != nil {
			return refuseCause(err)
		}
	}
	return nil
}

// normalizeModes makes the published tree readable and traversable by every uid
// while preserving which files were executable upstream: the image runs as an
// arbitrary uid, and an archive mode is not a policy this build adopts.
func normalizeModes(prefix string) error {
	return filepath.Walk(prefix, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return refuseCause(err)
		}
		mode := os.FileMode(0o644)
		if info.IsDir() || info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := os.Chmod(path, mode); err != nil {
			return refuseCause(err)
		}
		return nil
	})
}

// copyTree publishes a directory tree. The destination is new: the conflict
// check in Bootstrap refused an existing prefix, so this cannot merge with
// leftovers of an earlier run.
func copyTree(source, destination string) error {
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return refuseCause(err)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return refuseCause(err)
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return refuseCause(err)
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			// The member rules refuse links and devices, so this is unreachable;
			// it stays as the explicit refusal rather than a silent skip.
			return refuseText("unsafe distribution member: " + relative)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(source, destination string, mode os.FileMode) error {
	payload, err := os.ReadFile(source)
	if err != nil {
		return refuseCause(err)
	}
	if err := os.WriteFile(destination, payload, mode); err != nil {
		return refuseCause(err)
	}
	if err := os.Chmod(destination, mode); err != nil {
		return refuseCause(err)
	}
	return nil
}

// commandPaths is each published command path, used by the conflict check.
func commandPaths(commands string) []string {
	paths := make([]string, 0, len(commandNames))
	for _, name := range commandNames {
		paths = append(paths, filepath.Join(commands, name))
	}
	return paths
}

// run executes a reviewed program with the inherited environment and turns a
// failure into a refusal that names the program and its status.
func run(name string, arguments ...string) error {
	command := exec.Command(name, arguments...) //nolint:gosec // the argv is this file's own fixed call.
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return refuse("%s failed: %s", name, err)
	}
	return nil
}
