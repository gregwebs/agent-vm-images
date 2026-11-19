package pnpm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVerify(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *install, Target)
		want   string
	}{
		{name: "the published distribution is the reviewed one"},
		{
			name: "the installed pin is the reviewed pin",
			change: func(t *testing.T, staged *install, _ Target) {
				alternative := staged.pin
				alternative.Version = "11.10.0"
				alternative.URL = "https://registry.npmjs.org/pnpm/-/pnpm-11.10.0.tgz"
				alternative.UpstreamTag = "v11.10.0"
				writePinText(t, filepath.Join(staged.prefix, "pin.json"), alternative)
			},
			want: "installed pin disagrees with reviewed source",
		},
		{
			name: "the bundle is pristine",
			change: func(t *testing.T, staged *install, _ Target) {
				appendBytes(t, filepath.Join(staged.prefix, "dist/pnpm.mjs"), "raw shadow")
			},
			want: "pristine distribution changed",
		},
		{
			name: "the launcher is a regular file",
			change: func(t *testing.T, staged *install, target Target) {
				launcher := filepath.Join(target.Commands, "pnpm")
				remove(t, launcher)
				if err := os.Symlink("pnpm-real", launcher); err != nil {
					t.Fatalf("cannot replace the launcher with a link: %v", err)
				}
			},
			want: "pnpm launcher must be a regular file",
		},
		{
			name: "the launcher is the reviewed script",
			change: func(t *testing.T, staged *install, target Target) {
				launcher := filepath.Join(target.Commands, "pnpm")
				if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec pnpm \"$@\"\n"), 0o755); err != nil {
					t.Fatalf("cannot rewrite the launcher: %v", err)
				}
			},
			want: "pnpm launcher changed",
		},
		{
			name: "the first PATH candidate is the installed command",
			change: func(t *testing.T, staged *install, _ Target) {
				shadow := filepath.Join(staged.root, "shadow")
				if err := os.Mkdir(shadow, 0o755); err != nil {
					t.Fatalf("cannot create the shadow directory: %v", err)
				}
				if err := os.WriteFile(filepath.Join(shadow, "pnpm"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatalf("cannot write the shadow command: %v", err)
				}
				t.Setenv("PATH", shadow+string(os.PathListSeparator)+os.Getenv("PATH"))
			},
			want: "unexpected first PATH candidate for pnpm",
		},
		{
			name: "an alias links relatively to pnpm",
			change: func(t *testing.T, _ *install, target Target) {
				alias := filepath.Join(target.Commands, "pnx")
				remove(t, alias)
				if err := os.Symlink("other", alias); err != nil {
					t.Fatalf("cannot retarget the alias: %v", err)
				}
			},
			want: "alias must link relatively to pnpm",
		},
		{
			name: "the distribution carries no links",
			change: func(t *testing.T, staged *install, _ Target) {
				if err := os.Symlink("pnpm.mjs", filepath.Join(staged.prefix, "bin/pnpm-link.mjs")); err != nil {
					t.Fatalf("cannot add the link: %v", err)
				}
			},
			want: "unexpected symlinked distribution entry: ",
		},
		{
			name: "no component is group or other writable",
			change: func(t *testing.T, staged *install, _ Target) {
				if err := os.Chmod(filepath.Join(staged.prefix, "dist/pnpm.mjs"), 0o666); err != nil {
					t.Fatalf("cannot open the bundle mode: %v", err)
				}
			},
			want: "group/other-writable distribution path: ",
		},
		{
			name: "every component is readable by all",
			change: func(t *testing.T, staged *install, _ Target) {
				if err := os.Chmod(filepath.Join(staged.prefix, "bin/pnpm.mjs"), 0o700); err != nil {
					t.Fatalf("cannot close the entry mode: %v", err)
				}
			},
			want: "unreadable distribution path: ",
		},
		{
			name: "a writable waypoint on the interpreter chain is audited",
			change: func(t *testing.T, staged *install, target Target) {
				hop := filepath.Join(staged.root, "writable-hop")
				if err := os.Mkdir(hop, 0o755); err != nil {
					t.Fatalf("cannot create the hop directory: %v", err)
				}
				real := filepath.Join(hop, "node")
				copyBytes(t, target.Node, real, 0o755)
				remove(t, target.Node)
				if err := os.Symlink(real, target.Node); err != nil {
					t.Fatalf("cannot point the interpreter at the hop: %v", err)
				}
				if err := os.Chmod(hop, 0o777); err != nil {
					t.Fatalf("cannot open the hop mode: %v", err)
				}
			},
			want: "group/other-writable distribution path: ",
		},
		{
			name: "the launcher mode is 0755",
			change: func(t *testing.T, _ *install, target Target) {
				if err := os.Chmod(filepath.Join(target.Commands, "pnpm"), 0o555); err != nil {
					t.Fatalf("cannot change the launcher mode: %v", err)
				}
			},
			want: "launcher mode must be 0755",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			staged := stageInstall(t)
			target := staged.installed(t)
			if testCase.change != nil {
				testCase.change(t, staged, target)
			}
			err := Verify(VerifyOptions{Target: target, Source: staged.source, PathIdentity: true})
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("Verify refused the reviewed installation: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Verify accepted an installation it must refuse: want %q", testCase.want)
			}
			// Prefix-shaped refusals name the mutated path, so the case states only
			// the frozen prefix of the message.
			if got := err.Error(); !hasPrefix(got, testCase.want) {
				t.Fatalf("refusal is %q, want it to start with %q", got, testCase.want)
			}
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("refusal %v is not a *Refusal", err)
			}
		})
	}
}

func TestVerifyExemptsAFixtureSamefile(t *testing.T) {
	staged := stageInstall(t)
	target := staged.installed(t)
	shadow := filepath.Join(staged.root, "shadow")
	if err := os.Mkdir(shadow, 0o755); err != nil {
		t.Fatalf("cannot create the shadow directory: %v", err)
	}
	// A fixture root is not the image's /usr layout: `pnpm` can carry a second name
	// that resolves to the same file, which the alias rule tolerates for the
	// launcher only. The aliases are still the installed links themselves.
	if err := os.Symlink(filepath.Join(target.Commands, "pnpm"), filepath.Join(shadow, "pnpm")); err != nil {
		t.Fatalf("cannot link the fixture launcher: %v", err)
	}
	t.Setenv("PATH", shadow+string(os.PathListSeparator)+target.Commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := Verify(VerifyOptions{Target: target, Source: staged.source, PathIdentity: true}); err != nil {
		t.Fatalf("Verify refused a samefile fixture candidate: %v", err)
	}
}

func TestDereferencedPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("cannot resolve the fixture root: %v", err)
	}
	// A symlinked waypoint between the root and the final file: collapsing the
	// chain would hide it, so every hop has to appear.
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("cannot create the real directory: %v", err)
	}
	file := filepath.Join(real, "pnpm")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("cannot write the file: %v", err)
	}
	hop := filepath.Join(root, "hop")
	if err := os.Symlink(real, hop); err != nil {
		t.Fatalf("cannot create the hop: %v", err)
	}
	paths, err := dereferencedPaths(filepath.Join(hop, "pnpm"))
	if err != nil {
		t.Fatalf("dereferencedPaths failed: %v", err)
	}
	// The chain is spelled physically, exactly as the kernel resolves it, so the
	// link and both ends of the hop have to appear under their real paths.
	if !containsString(paths, hop) {
		t.Fatalf("the resolved chain %v does not carry the link %s", paths, hop)
	}
	for _, want := range []string{real, file} {
		if !containsString(paths, want) {
			t.Fatalf("the resolved chain %v does not carry %s", paths, want)
		}
	}
}

func TestDereferencedPathsRefusesALongChain(t *testing.T) {
	root := t.TempDir()
	previous := filepath.Join(root, "start")
	if err := os.WriteFile(previous, []byte("x"), 0o644); err != nil {
		t.Fatalf("cannot write the chain start: %v", err)
	}
	// 41 relative hops is one past the kernel's ceiling: it is a loop or an
	// unusable chain, and refusing here names the path.
	for index := 0; index < symlinkHopCeiling+1; index++ {
		link := filepath.Join(root, "hop"+string(rune('a'+index)))
		if err := os.Symlink(filepath.Base(previous), link); err != nil {
			t.Fatalf("cannot create hop %d: %v", index, err)
		}
		previous = link
	}
	_, err := dereferencedPaths(previous)
	if err == nil || err.Error() != "symlink chain too deep: "+previous {
		t.Fatalf("refusal is %v", err)
	}
}

func appendBytes(t *testing.T, path, text string) {
	t.Helper()
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("cannot open %s: %v", path, err)
	}
	if _, err := handle.WriteString(text); err != nil {
		t.Fatalf("cannot append to %s: %v", path, err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("cannot close %s: %v", path, err)
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("cannot remove %s: %v", path, err)
	}
}

func copyBytes(t *testing.T, source, destination string, mode os.FileMode) {
	t.Helper()
	payload, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("cannot read %s: %v", source, err)
	}
	if err := os.WriteFile(destination, payload, mode); err != nil {
		t.Fatalf("cannot write %s: %v", destination, err)
	}
}

func hasPrefix(text, prefix string) bool {
	return len(text) >= len(prefix) && text[:len(prefix)] == prefix
}

// TestFirstCandidateEmptyComponents is the identity rule a shell actually applies:
// an empty PATH entry is the current directory, so a same-version `pnpm` sitting
// there is the program the image would run.
func TestFirstCandidateEmptyComponents(t *testing.T) {
	staged := stageInstall(t)
	target := staged.installed(t)
	// The working directory is the fixture root, and it carries a shadow that
	// reports the same version as the reviewed launcher.
	shadow := filepath.Join(staged.root, "pnpm")
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\necho 11.11.0\n"), 0o755); err != nil {
		t.Fatalf("cannot write the shadow command: %v", err)
	}
	t.Chdir(staged.root)
	installed := target.Commands
	separator := string(os.PathListSeparator)
	absent := filepath.Join(staged.root, "absent")
	cases := []struct {
		name   string
		path   string
		refuse bool
	}{
		// Each empty entry sits before the installed directory, so the current
		// directory is the first place the shell would find the command.
		{name: "leading empty component", path: separator + installed + separator + os.Getenv("PATH"), refuse: true},
		{name: "interior empty component", path: absent + separator + separator + installed, refuse: true},
		{name: "trailing empty component", path: absent + separator, refuse: true},
		{name: "only the shadow", path: separator, refuse: true},
		// An empty entry after the installed directory is harmless: the first
		// candidate is still the installed file.
		{name: "the installed command first", path: installed + separator + absent + separator, refuse: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("PATH", testCase.path)
			err := Verify(VerifyOptions{Target: target, Source: staged.source, PathIdentity: true})
			if testCase.refuse {
				if err == nil || err.Error() != "unexpected first PATH candidate for pnpm" {
					t.Fatalf("refusal is %v, want the first-candidate rule", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify refused an installation whose PATH starts with it: %v", err)
			}
		})
	}
}

// TestFirstCandidateIsShellResolved pins the spelling too: the candidate for an
// empty entry is the cwd-relative name, not the absolute one.
func TestFirstCandidateIsShellResolved(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(filepath.Join(root, "pnpm"), []byte("x"), 0o755); err != nil {
		t.Fatalf("cannot write the shadow: %v", err)
	}
	candidate, found := firstCandidate(string(os.PathListSeparator)+"/usr/bin", "pnpm")
	if !found {
		t.Fatal("the empty PATH entry was skipped")
	}
	if candidate != "."+string(filepath.Separator)+"pnpm" {
		t.Fatalf("candidate is %q, want the cwd-relative ./pnpm", candidate)
	}
}
