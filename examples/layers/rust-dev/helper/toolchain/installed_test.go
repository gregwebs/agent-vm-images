package toolchain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner answers as the installed tools would, and records what it was asked.
type fakeRunner struct {
	outputs map[string]Result
	errors  map[string]error
	calls   [][]string
	envs    [][]string
}

func (f *fakeRunner) Run(_ context.Context, argv []string, env []string) (Result, error) {
	key := filepath.Base(argv[0]) + " " + strings.Join(argv[1:], " ")
	f.calls = append(f.calls, argv)
	f.envs = append(f.envs, env)
	if err, present := f.errors[key]; present {
		return Result{}, err
	}
	if result, present := f.outputs[key]; present {
		return result, nil
	}
	return Result{}, errors.New("unexpected command: " + strings.Join(argv, " "))
}

func newInstalledTree(t *testing.T, arch Architecture) (Homes, string) {
	t.Helper()
	root := t.TempDir()
	rustupHome := filepath.Join(root, "rustup")
	cargoHome := filepath.Join(root, "cargo")
	channel := "1.98.1"
	toolchain := channel + "-" + string(arch.Host())
	if err := os.MkdirAll(filepath.Join(rustupHome, "toolchains", toolchain, "lib", "rustlib",
		string(arch.Host()), "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rustupHome, "toolchains", toolchain, "lib", "rustlib",
		string(arch.Musl()), "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(rustupHome, "toolchains", toolchain, "lib", "rustlib",
		string(arch.Host()), "lib", "libstd.rlib"), "")
	write(t, filepath.Join(rustupHome, "toolchains", toolchain, "lib", "rustlib",
		string(arch.Musl()), "lib", "libstd.rlib"), "")
	write(t, filepath.Join(rustupHome, "settings.toml"), strings.Join([]string{
		`profile = "minimal"`,
		`auto_self_update = "disable"`,
		`auto_install = "disable"`,
		`default_toolchain = "` + toolchain + `"`,
		"",
	}, "\n"))
	if err := os.MkdirAll(filepath.Join(cargoHome, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rustup", "rustc", "cargo", "rustfmt"} {
		write(t, filepath.Join(cargoHome, "bin", name), "")
	}
	return Homes{Rustup: rustupHome, Cargo: cargoHome}, toolchain
}

// cleanOutputs is what a correct install answers.
func cleanOutputs(arch Architecture) map[string]Result {
	components := make([]string, 0, 6)
	for _, pair := range SelectedPairs(arch) {
		components = append(components, FilenameStems[pair.Package]+"-"+string(pair.Target))
	}
	lines := ""
	for index := len(components) - 1; index >= 0; index-- {
		lines += components[index] + "\n"
	}
	return map[string]Result{
		"rustup --version":                  {Stdout: "rustup 1.28.2 (e4f3ad6f8 2025-04-28)\n"},
		"rustup toolchain list":             {Stdout: "1.98.1-" + string(arch.Host()) + " (active, default)\n"},
		"rustup component list --installed": {Stdout: lines},
		"rustup target list --installed":    {Stdout: string(arch.Musl()) + "\n" + string(arch.Host()) + "\n"},
		"rustc -vV": {Stdout: "rustc 1.98.1 (48a229cea 2026-09-01)\nbinary: rustc\nhost: " +
			string(arch.Host()) + "\nrelease: 1.98.1\n"},
		"cargo --version":        {Stdout: "cargo 1.98.1\n"},
		"cargo clippy --version": {Stdout: "clippy 0.1.98\n"},
		"rustfmt --version":      {Stdout: "rustfmt 1.8.0\n"},
	}
}

// TestCheckInstalled is invariant 25: one sub-test per installed-state rule, each
// naming the fragment the source reader reports, plus the order.
func TestCheckInstalled(t *testing.T) {
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	arch := AMD64
	cases := []struct {
		label  string
		change func(t *testing.T, homes Homes, outputs map[string]Result,
			runner *fakeRunner)
		frag string
	}{
		{"clean", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {},
			""},
		{"no settings", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			if err := os.Remove(filepath.Join(homes.Rustup, "settings.toml")); err != nil {
				t.Fatal(err)
			}
		}, "installed: missing rustup settings at "},
		{"profile wrong", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			rewriteSettings(t, homes, `profile = "minimal"`, `profile = "default"`)
		}, "installed: rustup setting profile is 'default', expected 'minimal'"},
		{"auto_self_update wrong", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			rewriteSettings(t, homes, `auto_self_update = "disable"`,
				`auto_self_update = "enable"`)
		}, "installed: rustup setting auto_self_update is 'enable', expected 'disable'"},
		{"auto_install wrong", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			rewriteSettings(t, homes, `auto_install = "disable"`, `auto_install = "enable"`)
		}, "installed: rustup setting auto_install is 'enable', expected 'disable'"},
		{"profile not a string", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			rewriteSettings(t, homes, `profile = "minimal"`, `profile = 2026-01-01`)
		}, "installed: rustup setting profile is"},
		{"default toolchain wrong", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			rewriteSettings(t, homes, `default_toolchain = "1.98.1-`,
				`default_toolchain = "1.98.0-`)
		}, "installed: rustup default toolchain is not the pinned exact channel"},
		{"RUSTUP_TOOLCHAIN key", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			appendSettings(t, homes, `RUSTUP_TOOLCHAIN = "1.98.1-x86_64-unknown-linux-gnu"`)
		}, "installed: rustup settings override RUSTUP_TOOLCHAIN"},
		{"rustup 1.28.1", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			outputs["rustup --version"] = Result{Stdout: "rustup 1.28.1 (e4f3ad6f8 2025-04-28)\n"}
		}, "installed: rustup is not 1.28.2"},
		{"extra toolchain", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			outputs["rustup toolchain list"] = Result{Stdout: "1.98.1-x86_64-unknown-linux-gnu (active, default)\n" +
				"stable-x86_64-unknown-linux-gnu\n"}
		}, "installed: unexpected toolchains"},
		{"component missing", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			outputs["rustup component list --installed"] = Result{Stdout: "cargo-x86_64-unknown-linux-gnu\n"}
		}, "installed: components"},
		{"musl target missing", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			outputs["rustup target list --installed"] = Result{Stdout: "x86_64-unknown-linux-gnu\n"}
		}, "installed: targets"},
		{"host std missing", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			removeStd(t, homes, "x86_64-unknown-linux-gnu")
		}, "installed: missing x86_64-unknown-linux-gnu standard library"},
		{"musl std missing", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			removeStd(t, homes, "x86_64-unknown-linux-musl")
		}, "installed: missing x86_64-unknown-linux-musl standard library"},
		{"std has no rlib", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			if err := os.Remove(filepath.Join(homes.Rustup, "toolchains",
				"1.98.1-x86_64-unknown-linux-gnu", "lib", "rustlib",
				"x86_64-unknown-linux-gnu", "lib", "libstd.rlib")); err != nil {
				t.Fatal(err)
			}
		}, "installed: missing x86_64-unknown-linux-gnu standard library"},
		{"rustc release wrong", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			outputs["rustc -vV"] = Result{Stdout: "release: 1.98.0\nhost: x86_64-unknown-linux-gnu\n"}
		}, "installed: rustc release '1.98.0' != 1.98.1"},
		{"rustc host wrong", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			outputs["rustc -vV"] = Result{Stdout: "release: 1.98.1\nhost: aarch64-unknown-linux-gnu\n"}
		}, "installed: rustc host 'aarch64-unknown-linux-gnu' != x86_64-unknown-linux-gnu"},
		{"clippy fails", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			outputs["cargo clippy --version"] = Result{Status: 1, Stderr: "error: no such command\n"}
		}, "clippy --version failed with status 1: error: no such command"},
		{"rustfmt fails", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			outputs["rustfmt --version"] = Result{Status: 1}
			// The source reader's `what` is `' '.join(argv[1:])`, so the last two
			// tool runs are named by their flags alone.
		}, "--version failed with status 1: no output"},
		{"rustup version fails", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			outputs["rustup --version"] = Result{Status: 1, Stderr: "\nboom\n"}
		}, "rustup --version failed with status 1: boom"},
		{"rustup absent", func(t *testing.T, homes Homes, outputs map[string]Result, runner *fakeRunner) {
			runner.errors["rustup --version"] = errors.New("exec: no such file")
		}, "rustup --version failed: exec: no such file"},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			homes, _ := newInstalledTree(t, arch)
			outputs := cleanOutputs(arch)
			runner := &fakeRunner{outputs: outputs, errors: map[string]error{}}
			test.change(t, homes, outputs, runner)
			err := CheckInstalled(context.Background(), pin, arch, homes, runner)
			if test.frag == "" {
				if err != nil {
					t.Fatalf("refused a clean install: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.frag) {
				t.Fatalf("got %v, want a refusal naming %q", err, test.frag)
			}
		})
	}
}

// TestCheckInstalledForeignArch is the last installed row: amd64 expectations
// against an arm64 home.
func TestCheckInstalledForeignArch(t *testing.T) {
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	homes, _ := newInstalledTree(t, ARM64)
	outputs := cleanOutputs(ARM64)
	runner := &fakeRunner{outputs: outputs, errors: map[string]error{}}
	err = CheckInstalled(context.Background(), pin, AMD64, homes, runner)
	if err == nil || !strings.Contains(err.Error(), "installed:") {
		t.Fatalf("got %v", err)
	}
}

// TestToolEnvironmentIsBuilt is invariant 26: only PATH, LANG, LC_ALL and TZ pass
// through, the three toolchain variables are set here, and an ambient
// RUSTUP_TOOLCHAIN, RUSTC_WRAPPER or LD_PRELOAD cannot reach a tool.
func TestToolEnvironmentIsBuilt(t *testing.T) {
	t.Setenv("PATH", "/pinned/bin:/usr/bin:/bin")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("LC_ALL", "C")
	t.Setenv("TZ", "UTC")
	t.Setenv("RUSTUP_TOOLCHAIN", "stable")
	t.Setenv("RUSTC_WRAPPER", "sccache")
	t.Setenv("LD_PRELOAD", "/tmp/inject.so")
	t.Setenv("RUSTFLAGS", "-Ctarget-cpu=native")
	for _, key := range []string{"HOME", "USER", "SHELL"} {
		t.Setenv(key, "/should/not/reach/a/tool")
	}
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	homes, _ := newInstalledTree(t, AMD64)
	runner := &fakeRunner{outputs: cleanOutputs(AMD64), errors: map[string]error{}}
	if err := CheckInstalled(context.Background(), pin, AMD64, homes, runner); err != nil {
		t.Fatalf("refused a clean install: %v", err)
	}
	if len(runner.envs) == 0 {
		t.Fatal("no tool ran")
	}
	for index, env := range runner.envs {
		present := map[string]string{}
		for _, entry := range env {
			key, value, _ := strings.Cut(entry, "=")
			present[key] = value
		}
		want := map[string]string{
			"PATH": "/pinned/bin:/usr/bin:/bin", "LANG": "C.UTF-8", "LC_ALL": "C",
			"TZ": "UTC", "RUSTUP_HOME": homes.Rustup, "CARGO_HOME": homes.Cargo,
			"RUSTUP_AUTO_INSTALL": "0",
		}
		if len(present) != len(want) {
			t.Errorf("call %d has %d variables (%v), want %d", index, len(present),
				env, len(want))
		}
		for key, value := range want {
			if present[key] != value {
				t.Errorf("call %d: %s is %q, want %q", index, key, present[key], value)
			}
		}
		for _, banned := range append(ambientToolchainVariables, "HOME", "USER", "SHELL") {
			if banned == "RUSTUP_HOME" || banned == "CARGO_HOME" {
				continue
			}
			if _, leaked := present[banned]; leaked {
				t.Errorf("call %d: %s reached the tool", index, banned)
			}
		}
	}
}

// TestCheckInstalledOrder is the order contract: the settings file is examined
// before any tool runs, so a wrong setting is the first refusal even when the
// tools are also wrong.
func TestCheckInstalledOrder(t *testing.T) {
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	homes, _ := newInstalledTree(t, AMD64)
	rewriteSettings(t, homes, `profile = "minimal"`, `profile = "default"`)
	runner := &fakeRunner{outputs: map[string]Result{
		"rustup --version": {Status: 1},
	}, errors: map[string]error{}}
	err = CheckInstalled(context.Background(), pin, AMD64, homes, runner)
	if err == nil || !strings.Contains(err.Error(), "rustup setting profile") {
		t.Fatalf("got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a tool ran before the settings were checked: %v", runner.calls)
	}
}

func rewriteSettings(t *testing.T, homes Homes, old, replacement string) {
	t.Helper()
	path := filepath.Join(homes.Rustup, "settings.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), old) {
		t.Fatalf("settings.toml does not hold %q", old)
	}
	write(t, path, strings.Replace(string(raw), old, replacement, 1))
}

func appendSettings(t *testing.T, homes Homes, line string) {
	t.Helper()
	path := filepath.Join(homes.Rustup, "settings.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(raw)+line+"\n")
}

func removeStd(t *testing.T, homes Homes, target string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(homes.Rustup, "toolchains",
		"1.98.1-x86_64-unknown-linux-gnu", "lib", "rustlib", target)); err != nil {
		t.Fatal(err)
	}
}
