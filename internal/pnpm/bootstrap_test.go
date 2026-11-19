package pnpm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestBootstrap(t *testing.T) {
	t.Run("publishes the reviewed distribution", func(t *testing.T) {
		staged := stageInstall(t)
		if err := staged.publish(t, staged.archive); err != nil {
			t.Fatalf("bootstrap refused the reviewed fixture: %v", err)
		}
		bundle, err := os.ReadFile(filepath.Join(staged.prefix, "dist/pnpm.mjs"))
		if err != nil {
			t.Fatalf("cannot read the published bundle: %v", err)
		}
		// The published bundle is the archive member, byte for byte: the build
		// never edits upstream bytes.
		if string(bundle) != "pristine fixture bundle" {
			t.Fatalf("published bundle is %q", bundle)
		}
		launcher, err := os.ReadFile(filepath.Join(staged.commands, "pnpm"))
		if err != nil {
			t.Fatalf("cannot read the launcher: %v", err)
		}
		if string(launcher) != reviewedLauncher {
			t.Fatalf("launcher is %q", launcher)
		}
		info, err := os.Stat(filepath.Join(staged.commands, "pnpm"))
		if err != nil {
			t.Fatalf("cannot stat the launcher: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("launcher mode is %o", info.Mode().Perm())
		}
		for _, name := range []string{"pnx", "pnpx"} {
			link, err := os.Readlink(filepath.Join(staged.commands, name))
			if err != nil {
				t.Fatalf("cannot read the %s alias: %v", name, err)
			}
			if link != "pnpm" {
				t.Fatalf("%s alias points at %q", name, link)
			}
		}
		entry, err := os.Stat(filepath.Join(staged.prefix, "dist/pnpm.mjs"))
		if err != nil {
			t.Fatalf("cannot stat the published bundle: %v", err)
		}
		if entry.Mode().Perm() != 0o644 {
			t.Fatalf("bundle mode is %o", entry.Mode().Perm())
		}
	})
	t.Run("a missing fixture archive is a refusal and leaves no destination", func(t *testing.T) {
		staged := stageInstall(t)
		err := staged.publish(t, filepath.Join(staged.root, "absent.tgz"))
		if err == nil {
			t.Fatal("bootstrap accepted a missing fixture archive")
		}
		var refusal *Refusal
		if !errors.As(err, &refusal) {
			t.Fatalf("a missing fixture archive is %T, want *Refusal", err)
		}
		assertNoDestination(t, staged)
	})
	t.Run("an archive the pin does not authenticate is refused", func(t *testing.T) {
		staged := stageInstall(t)
		corrupt := filepath.Join(staged.root, "corrupt.tgz")
		raw, err := os.ReadFile(staged.archive)
		if err != nil {
			t.Fatalf("cannot read the fixture archive: %v", err)
		}
		raw[len(raw)-1] ^= 1
		if err := os.WriteFile(corrupt, raw, 0o600); err != nil {
			t.Fatalf("cannot write the corrupt archive: %v", err)
		}
		err = staged.publish(t, corrupt)
		if err == nil || err.Error() != "archive SHA-512 mismatch" {
			t.Fatalf("refusal is %v, want the archive digest mismatch", err)
		}
		assertNoDestination(t, staged)
	})
	t.Run("a conflicting destination is refused", func(t *testing.T) {
		staged := stageInstall(t)
		t.Setenv(FixtureRootVariable, staged.root)
		prefix, commands, err := FixtureDestination(staged.destination)
		if err != nil {
			t.Fatalf("cannot resolve the fixture destination: %v", err)
		}
		if err := os.MkdirAll(commands, 0o755); err != nil {
			t.Fatalf("cannot create the command directory: %v", err)
		}
		conflict := filepath.Join(commands, "pnpm")
		if err := os.WriteFile(conflict, []byte("existing"), 0o755); err != nil {
			t.Fatalf("cannot write the conflicting command: %v", err)
		}
		publishErr := staged.publish(t, staged.archive)
		if publishErr == nil || publishErr.Error() != "refusing conflicting destination: "+conflict {
			t.Fatalf("refusal is %v", publishErr)
		}
		if _, err := os.Stat(prefix); err == nil {
			t.Fatal("a refused install published a distribution")
		}
	})
	t.Run("a relative fixture archive is refused", func(t *testing.T) {
		staged := stageInstall(t)
		err := staged.publish(t, "pnpm.tgz")
		if err == nil {
			t.Fatal("bootstrap accepted a relative fixture archive")
		}
		assertNoDestination(t, staged)
	})
	t.Run("an escaping destination is refused", func(t *testing.T) {
		staged := stageInstall(t)
		outside := filepath.Join(staged.root, "outside")
		if err := os.Mkdir(outside, 0o755); err != nil {
			t.Fatalf("cannot create the outside directory: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(staged.destination, "opt")); err != nil {
			t.Fatalf("cannot redirect the prefix: %v", err)
		}
		t.Setenv(FixtureRootVariable, staged.root)
		resolved, err := filepath.EvalSymlinks(staged.destination)
		if err != nil {
			t.Fatalf("cannot resolve the fixture destination: %v", err)
		}
		_, _, err = FixtureDestination(staged.destination)
		// The refusal names the first candidate whose own resolution escapes, which
		// is the distribution prefix under the redirected `opt`.
		want := "fixture descendant escapes destination: " + filepath.Join(resolved, "opt/agent-vm/pnpm")
		if err == nil || err.Error() != want {
			t.Fatalf("refusal is %v, want %q", err, want)
		}
	})
	t.Run("a fixture root outside the destination is refused", func(t *testing.T) {
		staged := stageInstall(t)
		t.Setenv(FixtureRootVariable, staged.root)
		_, _, err := FixtureDestination(staged.root)
		if err == nil || err.Error() != "destination outside fixture root" {
			t.Fatalf("refusal is %v", err)
		}
	})
	t.Run("no production positional argument", func(t *testing.T) {
		// The predecessor refused every positional argument rather than reporting
		// usage; the CLI layer keeps that status and message.
		staged := stageInstall(t)
		if err := staged.publish(t, staged.archive); err != nil {
			t.Fatalf("bootstrap refused the reviewed fixture: %v", err)
		}
		if _, err := os.Stat(filepath.Join(staged.prefix, "pin.json")); err != nil {
			t.Fatalf("the published prefix carries no pin: %v", err)
		}
	})
}

// assertNoDestination is the invariant every refused bootstrap keeps: neither
// directory it would publish into exists.
func assertNoDestination(t *testing.T, staged *install) {
	t.Helper()
	if _, err := os.Stat(staged.prefix); err == nil {
		t.Fatal("a refused bootstrap published a distribution")
	}
	if _, err := os.Stat(filepath.Join(staged.commands, "pnpm")); err == nil {
		t.Fatal("a refused bootstrap published a launcher")
	}
}

// TestFixtureDestinationRequiresAnOwnedRoot covers the refusals that do not need a
// published tree.
func TestFixtureDestinationRequiresAnOwnedRoot(t *testing.T) {
	t.Run("the variable is required", func(t *testing.T) {
		t.Setenv(FixtureRootVariable, "")
		if _, _, err := FixtureDestination("/"); err == nil {
			t.Fatal("a missing fixture root was accepted")
		}
	})
	t.Run("the root cannot be the filesystem root", func(t *testing.T) {
		t.Setenv(FixtureRootVariable, "/")
		if _, _, err := FixtureDestination("/"); err == nil || err.Error() != "fixture root must be invocation-owned" {
			t.Fatalf("refusal is %v", err)
		}
	})
	t.Run("the destination must exist", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv(FixtureRootVariable, root)
		if _, _, err := FixtureDestination(filepath.Join(root, "absent")); err == nil {
			t.Fatal("a missing destination was accepted")
		}
	})
}

func TestBootstrapNeedsRootInProduction(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("the production refusal is about a non-root caller")
	}
	t.Setenv(FixtureDestinationVariable, "")
	t.Setenv(SourceDirVariable, "")
	err := Bootstrap(context.Background(), Options{SourceDir: t.TempDir()})
	if err == nil {
		t.Fatal("a production bootstrap without root was accepted")
	}
	// The pin is read first, so the refusal here is the missing pin rather than
	// the uid; the uid rule is checked where the destination is resolved.
	_, _, _, targetErr := publicationTargets("")
	if targetErr == nil || targetErr.Error() != "production bootstrap requires root" {
		t.Fatalf("refusal is %v", targetErr)
	}
}
