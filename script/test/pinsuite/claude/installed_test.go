package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// fileOwner is a file's owning uid and gid. The expected owner is the one this process
// can actually create: the suite cannot produce a root-owned file, which is why the
// hook has AGENT_VM_CLAUDE_EXPECTED_OWNER.
func fileOwner(r harness.Reporter, path string) (int, int, string) {
	r.Helper()
	info, err := os.Stat(path)
	if err != nil {
		r.Fatalf("cannot stat %s: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		r.Fatalf("cannot read the owner of %s on this platform", path)
	}
	return int(stat.Uid), int(stat.Gid), fmt.Sprintf("%d:%d", stat.Uid, stat.Gid)
}

// installedRefused is one `installed` invocation that must refuse.
func installedRefused(r harness.Reporter, fixture *fixture, prefix, owner string, needles ...string) string {
	r.Helper()
	return refuses(r, pinInvocation("installed", fixture.closure, "--version", Version,
		"--platform", string(linuxX64), "--prefix", prefix, "--owner", owner), needles)
}

// TestInstalledNativeLayoutAccepted: the native layout, its launcher and its payload are
// what the pin says.
func TestInstalledNativeLayoutAccepted(t *testing.T) {
	fixture := testFixture(t)
	payload := harness.ReadFile(t, fixture.x64)
	_, _, owner := fileOwner(t, fixture.x64)
	prefix := installLayout(t, scratch(t, "installed-good"), installShape{Payload: payload})
	printed := accepts(t, pinInvocation("installed", fixture.closure, "--version", Version,
		"--platform", string(linuxX64), "--prefix", prefix, "--owner", owner)).Stdout
	if !strings.Contains(printed, "match the committed pin") {
		t.Fatalf("the accepted layout printed %q", printed)
	}
}

// TestInstalledPayloadBytesDifferRefused: the placed payload is re-hashed, so a byte
// added after the install is a refusal.
func TestInstalledPayloadBytesDifferRefused(t *testing.T) {
	fixture := testFixture(t)
	payload := harness.ReadFile(t, fixture.x64)
	_, _, owner := fileOwner(t, fixture.x64)
	prefix := installLayout(t, scratch(t, "installed-bytes"),
		installShape{Payload: append(payload, '\n')})
	installedRefused(t, fixture, prefix, owner, "sha256")
}

// TestInstalledPayloadModeRefused: the payload's whole permission word is 0755, so a
// group- or world-writable one is refused by its mode.
func TestInstalledPayloadModeRefused(t *testing.T) {
	fixture := testFixture(t)
	payload := harness.ReadFile(t, fixture.x64)
	_, _, owner := fileOwner(t, fixture.x64)
	for _, mode := range []os.FileMode{0o775, 0o644} {
		t.Run(fmt.Sprintf("%04o", mode), func(t *testing.T) {
			prefix := installLayout(t, scratch(t, "installed-mode"),
				installShape{Payload: payload, PayloadMode: mode})
			installedRefused(t, fixture, prefix, owner, fmt.Sprintf("%04o", mode), "not 0755")
		})
	}
}

// TestInstalledDifferentOwnerRefused: the payload's owner is part of the layout.
func TestInstalledDifferentOwnerRefused(t *testing.T) {
	fixture := testFixture(t)
	payload := harness.ReadFile(t, fixture.x64)
	uid, gid, _ := fileOwner(t, fixture.x64)
	prefix := installLayout(t, scratch(t, "installed-owner"), installShape{Payload: payload})
	other := fmt.Sprintf("%d:%d", uid+1, gid+1)
	installedRefused(t, fixture, prefix, other, "is owned")
}

// TestInstalledLaunchersRefused: the launcher is a symlink to exactly the installed
// payload -- not a relative path, not another prefix's payload, not a file, not a
// dangling link.
func TestInstalledLaunchersRefused(t *testing.T) {
	fixture := testFixture(t)
	payload := harness.ReadFile(t, fixture.x64)
	_, _, owner := fileOwner(t, fixture.x64)
	t.Run("relative target", func(t *testing.T) {
		prefix := installLayout(t, scratch(t, "installed-relative"), installShape{
			Payload:  payload,
			Launcher: ".local/share/claude/versions/" + Version,
		})
		installedRefused(t, fixture, prefix, owner, "points at")
	})
	t.Run("another prefix's payload", func(t *testing.T) {
		prefix := installLayout(t, scratch(t, "installed-other"), installShape{
			Payload:  payload,
			Launcher: "/opt/agent/.local/share/claude/versions/" + Version,
		})
		installedRefused(t, fixture, prefix, owner, "points at")
	})
	t.Run("a regular file", func(t *testing.T) {
		prefix := installLayout(t, scratch(t, "installed-regular"), installShape{Payload: payload})
		launcher := filepath.Join(prefix, ".local/bin/claude")
		if err := os.Remove(launcher); err != nil {
			t.Fatalf("cannot remove the launcher: %v", err)
		}
		harness.WriteFile(t, launcher, []byte("#!/bin/sh\n"))
		installedRefused(t, fixture, prefix, owner, "not a symlink")
	})
	t.Run("a dangling target", func(t *testing.T) {
		prefix := installLayout(t, scratch(t, "installed-dangling"), installShape{
			Payload:  payload,
			Launcher: "/nonexistent/claude",
		})
		installedRefused(t, fixture, prefix, owner, "points at")
	})
}

// TestInstalledVersionsEntriesRefused: the versions directory holds exactly the pinned
// version, and the payload in it is a regular file.
func TestInstalledVersionsEntriesRefused(t *testing.T) {
	fixture := testFixture(t)
	payload := harness.ReadFile(t, fixture.x64)
	_, _, owner := fileOwner(t, fixture.x64)
	t.Run("an extra entry", func(t *testing.T) {
		prefix := installLayout(t, scratch(t, "installed-extra"), installShape{
			Payload:       payload,
			ExtraVersions: []string{Version + "-old"},
		})
		installedRefused(t, fixture, prefix, owner, "holds")
	})
	t.Run("a missing directory", func(t *testing.T) {
		prefix := installLayout(t, scratch(t, "installed-missing"), installShape{Payload: payload})
		remove(t, filepath.Join(prefix, ".local/share/claude/versions"))
		installedRefused(t, fixture, prefix, owner, "is missing")
	})
	t.Run("a symlinked payload", func(t *testing.T) {
		prefix := installLayout(t, scratch(t, "installed-symlink"), installShape{Payload: payload})
		entry := filepath.Join(prefix, ".local/share/claude/versions", Version)
		real := filepath.Join(prefix, "real-payload")
		if err := os.Rename(entry, real); err != nil {
			t.Fatalf("cannot move the payload: %v", err)
		}
		if err := os.Symlink(real, entry); err != nil {
			t.Fatalf("cannot write the payload symlink: %v", err)
		}
		installedRefused(t, fixture, prefix, owner, "not a regular file")
	})
}

// TestInstalledWritableDirectoriesRefusedByName: each directory in the layout is refused
// by name when the group or the world can write it.
func TestInstalledWritableDirectoriesRefusedByName(t *testing.T) {
	fixture := testFixture(t)
	payload := harness.ReadFile(t, fixture.x64)
	_, _, owner := fileOwner(t, fixture.x64)
	rows := []struct {
		relative string
		mode     os.FileMode
	}{
		{".local/bin", 0o775},
		{".local/share/claude", 0o757},
		{".local/share/claude/versions", 0o775},
	}
	for _, row := range rows {
		t.Run(row.relative, func(t *testing.T) {
			prefix := installLayout(t, scratch(t, "installed-writable"), installShape{Payload: payload})
			if err := os.Chmod(filepath.Join(prefix, row.relative), row.mode); err != nil {
				t.Fatalf("cannot widen the mode of %s: %v", row.relative, err)
			}
			installedRefused(t, fixture, prefix, owner, row.relative, "group- or world-writable")
		})
	}
}
