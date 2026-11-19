// Command avm-test is the fixture builder the test suites and the suites above
// them share (ADR 0017).
//
// A pin suite needs synthetic release bytes: fake archives, manifests and release
// records, in both channels a writer corroborates, so that a refusal can be pinned
// on exactly one disagreement. The bumper suite needs the same bytes to serve an
// OLD and a NEW release to the writers it drives, and it needs them for every tool.
// That is why this exists as a command rather than as a flag on a test: the code
// that builds a fixture for another suite is the code the cases check against, so
// the closure handed to the bumper cannot be a hand-written lookalike.
//
// Two kinds of fixture live here, and they are not the same kind of thing:
// `write-fixture --tool agy` publishes through the real writer, so the agy suite and
// the bumper hold the bytes production produces, while `write-fixture --tool claude`
// and the `keygen`/`elf`/`closure`/`two-primaries` verbs are the Claude suite's own
// fixture primitives -- a fixture closure is signed by a key the review has never
// seen, which no writer run can produce. Both are exposed so the shell suites that
// need one call this implementation instead of keeping a copy of it.
//
// `write-fixture --tool codex` and `write-installer-fixture --tool codex` are the
// Codex suite's own builders: a synthetic closure (no writer run can produce a
// fixture's evidence without a real cosign), and the four-part installer fixture its
// two shell suites mount. Both are tool-scoped through the same table as every other
// mode, so a tool that has no such fixture says so instead of writing a lookalike.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/agy"
	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/claude"
	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/codex"
	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

type arguments struct {
	WriteFixture          *writeFixtureCommand          `arg:"subcommand:write-fixture" help:"write one fixture closure for a pin family (see --tool)"`
	WriteInstallerFixture *writeInstallerFixtureCommand `arg:"subcommand:write-installer-fixture" help:"write one installer suite fixture: closure, assets, archive and served tree"`
	Keygen                *keygenCommand                `arg:"subcommand:keygen" help:"generate one throwaway OpenPGP signer and print its fingerprint"`
	Elf                   *elfCommand                   `arg:"subcommand:elf" help:"write one ELF-shaped blob for a fixture payload"`
	Closure               *closureCommand               `arg:"subcommand:closure" help:"write one consistent Claude closure"`
	TwoPrimaries          *twoPrimariesCommand          `arg:"subcommand:two-primaries" help:"concatenate two armored public keys into one file"`
}

func (arguments) Description() string {
	return "Fixture builders for the pin suites. Writes test fixtures; publishes nothing."
}

type writeFixtureCommand struct {
	Tool    string `arg:"--tool,required" placeholder:"NAME" help:"the pin family this tool builds (see the error for the list)"`
	Dir     string `arg:"--dir,required" placeholder:"DIR" help:"the absolute directory to write"`
	Version string `arg:"--version" placeholder:"X.Y.Z" help:"the release to publish (default: the suite's fixture release)"`
	Signer  string `arg:"--signer" placeholder:"DIR" help:"claude only: the signer directory (default: DIR/signer, generated when it holds no key)"`
}

func (writeFixtureCommand) Description() string {
	return "Write one fixture closure. A family that publishes through its real writer does; a family whose closure carries a signer the review has never seen synthesizes it."
}

type writeInstallerFixtureCommand struct {
	Tool           string `arg:"--tool,required" placeholder:"NAME" help:"the pin family this tool builds (see the error for the list)"`
	Dir            string `arg:"--dir,required" placeholder:"DIR" help:"the absolute directory to write"`
	Release        string `arg:"--release,required" placeholder:"TAG" help:"the exact release the fixture pins"`
	Target         string `arg:"--target,required" placeholder:"TRIPLE" help:"the platform whose archive goes to DIR/archive.tgz"`
	NativeScript   string `arg:"--native-script,required" placeholder:"FILE" help:"the absolute script body the fixture entrypoint carries"`
	PackageVersion string `arg:"--package-version" placeholder:"X.Y.Z" help:"the package metadata version (default: the release's)"`
	PackageTarget  string `arg:"--package-target" placeholder:"TRIPLE" help:"the package metadata target (default: the platform's)"`
}

func (writeInstallerFixtureCommand) Description() string {
	return "Write one installer suite fixture: the closure, the upstream documents, the selected archive and the served tree."
}

type keygenCommand struct {
	Out    string `arg:"positional,required" placeholder:"OUT" help:"the directory for key.asc, secret.asc and fingerprint"`
	Subkey bool   `arg:"--subkey" help:"generate a cert-only primary and a signing subkey"`
}

func (keygenCommand) Description() string {
	return "Generate one throwaway OpenPGP signer and print its primary fingerprint."
}

type elfCommand struct {
	Out     string `arg:"positional,required" placeholder:"OUT" help:"the blob to write"`
	Machine int    `arg:"--machine,required" placeholder:"62|183" help:"the e_machine to declare"`
	Class   int    `arg:"--class" placeholder:"N" help:"the e_ident class (default 2, ELFCLASS64)"`
	Endian  int    `arg:"--endian" placeholder:"N" help:"the e_ident data word (default 1, little-endian)"`
	Size    int    `arg:"--size" placeholder:"N" help:"the blob's size in bytes (default 4096)"`
}

func (elfCommand) Description() string {
	return "Write one ELF-shaped blob, for a fixture payload that is not an executable."
}

type closureCommand struct {
	Out         string `arg:"positional,required" placeholder:"OUT" help:"the closure directory to write"`
	Signer      string `arg:"--signer,required" placeholder:"DIR" help:"the signer directory (key.asc, secret.asc, fingerprint)"`
	Version     string `arg:"--version,required" placeholder:"X.Y.Z" help:"the release the closure pins"`
	AMD64       string `arg:"--amd64,required" placeholder:"FILE" help:"the linux-x64 executable"`
	ARM64       string `arg:"--arm64,required" placeholder:"FILE" help:"the linux-arm64 executable"`
	Key         string `arg:"--key" placeholder:"FILE" help:"the committed key file (default: the signer's public key)"`
	Fingerprint string `arg:"--fingerprint" placeholder:"FPR" help:"the primary the pin names (default: the signer's)"`
	Signature   string `arg:"--signature" placeholder:"FILE" help:"a detached signature over other bytes (default: sign the manifest)"`
	Subkey      bool   `arg:"--subkey" help:"read the signing subkey fingerprint from the signer directory"`
}

func (closureCommand) Description() string {
	return "Write one consistent Claude closure: pin, manifest, signature and key."
}

type twoPrimariesCommand struct {
	Out    string `arg:"positional,required" placeholder:"OUT" help:"the combined key file"`
	First  string `arg:"positional,required" placeholder:"FIRST" help:"the first armored public key"`
	Second string `arg:"positional,required" placeholder:"SECOND" help:"the second armored public key"`
}

func (twoPrimariesCommand) Description() string {
	return "Concatenate two armored public keys into one key file."
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	var parsed arguments
	parser, err := arg.NewParser(arg.Config{Program: "avm-test"}, &parsed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "avm-test: %v\n", err)
		return 2
	}
	if err := parser.Parse(argv); err != nil {
		if errors.Is(err, arg.ErrHelp) {
			parser.WriteHelp(os.Stdout)
			return 0
		}
		fmt.Fprintf(os.Stderr, "avm-test: %v\n", err)
		parser.WriteHelp(os.Stderr)
		return 2
	}
	switch {
	case parsed.WriteFixture != nil:
		return runWriteFixture(*parsed.WriteFixture)
	case parsed.WriteInstallerFixture != nil:
		return runWriteInstallerFixture(*parsed.WriteInstallerFixture)
	case parsed.Keygen != nil:
		return runKeygen(*parsed.Keygen)
	case parsed.Elf != nil:
		return runElf(*parsed.Elf)
	case parsed.Closure != nil:
		return runClosure(*parsed.Closure)
	case parsed.TwoPrimaries != nil:
		return runTwoPrimaries(*parsed.TwoPrimaries)
	}
	parser.WriteHelp(os.Stderr)
	return 2
}

// absolute refuses a path this tool would reinterpret. Every path it is handed is one:
// `script/dev/avm-test.sh` chdirs to the checkout before exec, so a relative path names
// a different tree here than it did for the caller -- which is a malformed invocation
// (2), not a fixture this tool could not build (1).
func absolute(name, path string) error {
	if filepath.IsAbs(path) {
		return nil
	}
	return fmt.Errorf("%s must be absolute: %q, because this tool runs from the checkout",
		name, path)
}

// pins refuses a release this family's reader would not pin. An absent value means the
// caller took the default, which the builder owns.
func pins(entry tool, name, release string) error {
	if release == "" || entry.release == nil {
		return nil
	}
	if err := entry.release(release); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// usage reports a malformed invocation.
func usage(err error) int {
	fmt.Fprintf(os.Stderr, "avm-test: %v\n", err)
	return 2
}

// runWriteFixture validates what this tool cannot honour before it builds anything: a
// tool it does not know, a flag that tool does not take, a release that tool cannot
// pin, and the two paths a fixture run writes or reads through, because a caller that
// branches on the status must read a typo as a typo.
func runWriteFixture(command writeFixtureCommand) int {
	entry, known := tools[command.Tool]
	if !known {
		return usage(fmt.Errorf("%q is not a fixture this tool builds: %s",
			command.Tool, toolNames()))
	}
	if command.Signer != "" && !entry.signer {
		return usage(fmt.Errorf("--signer is not a flag --tool %s honours", command.Tool))
	}
	if err := pins(entry, "--version", command.Version); err != nil {
		return usage(err)
	}
	// --dir is required, and an explicitly present empty one is a malformed invocation,
	// not an empty destination the builder should judge; --signer is optional, so an
	// empty one simply is not there.
	if command.Dir == "" {
		return usage(fmt.Errorf("--dir must not be empty"))
	}
	for _, path := range []struct{ name, value string }{
		{"--dir", command.Dir},
		{"--signer", command.Signer},
	} {
		if path.value == "" {
			continue
		}
		if err := absolute(path.name, path.value); err != nil {
			return usage(err)
		}
	}
	return emit(func(r harness.Reporter) string { return entry.build(r, command) })
}

// runWriteInstallerFixture validates what this mode cannot honour before it builds
// anything: a tool it does not know, a tool whose family has no installer fixture, the
// two paths a fixture run writes or reads through, and the two triples -- an unknown
// triple is a caller's typo, so it is usage (2), never a failed build (1).
func runWriteInstallerFixture(command writeInstallerFixtureCommand) int {
	entry, known := tools[command.Tool]
	if !known {
		return usage(fmt.Errorf("%q is not a fixture this tool builds: %s",
			command.Tool, toolNames()))
	}
	if entry.installer == nil {
		return usage(fmt.Errorf("--tool %s has no installer fixture", command.Tool))
	}
	if command.Release == "" {
		return usage(fmt.Errorf("--release must not be empty"))
	}
	if err := pins(entry, "--release", command.Release); err != nil {
		return usage(err)
	}
	for _, path := range []struct{ name, value string }{
		{"--dir", command.Dir},
		{"--native-script", command.NativeScript},
	} {
		if err := absolute(path.name, path.value); err != nil {
			return usage(err)
		}
	}
	for _, triple := range []struct{ name, value string }{
		{"--target", command.Target},
		{"--package-target", command.PackageTarget},
	} {
		// --target is required, and an explicitly empty one is a malformed invocation
		// rather than a package for whatever platform the builder picks.
		if triple.value == "" && triple.name == "--target" {
			return usage(fmt.Errorf("%s must not be empty", triple.name))
		}
		if triple.value == "" {
			continue
		}
		if !codex.KnownTarget(triple.value) {
			return usage(fmt.Errorf("%s must be one of %s, found %q", triple.name,
				strings.Join(codex.Targets, ", "), triple.value))
		}
	}
	return emit(func(r harness.Reporter) string { return entry.installer(r, command) })
}

func runKeygen(command keygenCommand) int {
	if err := absolute("OUT", command.Out); err != nil {
		return usage(err)
	}
	var fingerprint claude.Fingerprint
	if status := build(func(r harness.Reporter) {
		fingerprint = claude.Keygen(r, command.Out, command.Subkey)
	}); status != 0 {
		return status
	}
	fmt.Printf("%s\n", fingerprint)
	return 0
}

func runElf(command elfCommand) int {
	if err := absolute("OUT", command.Out); err != nil {
		return usage(err)
	}
	return build(func(r harness.Reporter) {
		claude.Elf(r, claude.ElfArguments{
			Out:     command.Out,
			Machine: command.Machine,
			Class:   command.Class,
			Endian:  command.Endian,
			Size:    command.Size,
		})
	})
}

func runClosure(command closureCommand) int {
	for _, path := range []struct{ name, value string }{
		{"OUT", command.Out},
		{"--signer", command.Signer},
		{"--amd64", command.AMD64},
		{"--arm64", command.ARM64},
		{"--key", command.Key},
		{"--signature", command.Signature},
	} {
		if path.value == "" {
			continue
		}
		if err := absolute(path.name, path.value); err != nil {
			return usage(err)
		}
	}
	return build(func(r harness.Reporter) {
		claude.Closure(r, claude.ClosureArguments{
			Out:         command.Out,
			Signer:      command.Signer,
			Version:     command.Version,
			AMD64:       command.AMD64,
			ARM64:       command.ARM64,
			Key:         command.Key,
			Fingerprint: command.Fingerprint,
			Signature:   command.Signature,
			Subkey:      command.Subkey,
		})
	})
}

func runTwoPrimaries(command twoPrimariesCommand) int {
	// Every one of the three is a required positional, so none can be empty and none
	// is skipped: an empty one is refused like a relative one.
	for _, path := range []struct{ name, value string }{
		{"OUT", command.Out},
		{"FIRST", command.First},
		{"SECOND", command.Second},
	} {
		if err := absolute(path.name, path.value); err != nil {
			return usage(err)
		}
	}
	return build(func(r harness.Reporter) {
		claude.TwoPrimaries(r, command.Out, command.First, command.Second)
	})
}

// build runs one fixture builder and returns the status its failure carries: a
// fixture this tool cannot build is a failed run (1), not a malformed invocation.
func build(work func(harness.Reporter)) int {
	if err := harness.Command(work); err != nil {
		fmt.Fprintf(os.Stderr, "avm-test: %v\n", err)
		return 1
	}
	return 0
}

// builder publishes one fixture from the parsed command and returns the exact text to
// print, so a tool whose output a caller parses says what that caller reads.
type builder func(harness.Reporter, writeFixtureCommand) string

// installerBuilder is the installer fixture mode's builder. A family that has none
// says so instead of answering with a lookalike.
type installerBuilder func(harness.Reporter, writeInstallerFixtureCommand) string

// tool is one fixture family: the builders it has, and which of this command's flags
// apply to it at all.
type tool struct {
	build builder
	// installer is the `write-installer-fixture` builder, nil when the family has none.
	installer installerBuilder
	// release refuses a tag this family cannot pin. A family whose reader has an
	// exact-release grammar answers here, so a mistyped tag is a malformed invocation
	// (2) rather than a fixture that could not be built (1). A family without one
	// builds whatever tag it is handed and leaves the verdict to its writer or its
	// builder, which is the older agy and claude surface.
	release func(string) error
	// signer reports whether the tool honours --signer.
	signer bool
}

// tools is the fixture families this command builds: the table the flag value is
// looked up in, and the whole list of what exists.
var tools = map[string]tool{
	"agy": {
		build: func(r harness.Reporter, command writeFixtureCommand) string {
			version := agy.WriteFixture(r, agy.Fixture{Dir: command.Dir, Version: command.Version})
			return fmt.Sprintf("avm-test: agy %s written to %s\n", version, command.Dir)
		},
	},
	"claude": {
		signer: true,
		build: func(r harness.Reporter, command writeFixtureCommand) string {
			version := claude.WriteFixture(r, claude.Fixture{
				Dir:     command.Dir,
				Version: command.Version,
				Signer:  command.Signer,
			})
			return fmt.Sprintf("avm-test: claude %s written to %s\n", version, command.Dir)
		},
	},
	"codex": {
		release: codex.ReleaseSupported,
		build: func(r harness.Reporter, command writeFixtureCommand) string {
			release := codex.WriteFixture(r, codex.Fixture{Dir: command.Dir, Release: command.Version})
			return fmt.Sprintf("avm-test: codex %s written to %s\n", release, command.Dir)
		},
		installer: func(r harness.Reporter, command writeInstallerFixtureCommand) string {
			release := codex.WriteInstallerFixture(r, codex.InstallerFixture{
				Dir:            command.Dir,
				Release:        command.Release,
				Target:         command.Target,
				NativeScript:   command.NativeScript,
				PackageVersion: command.PackageVersion,
				PackageTarget:  command.PackageTarget,
			})
			return fmt.Sprintf("avm-test: codex installer fixture for %s %s written to %s\n",
				release, command.Target, command.Dir)
		},
	},
}

func toolNames() string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// emit runs one fixture mode's builder and prints its one line. Both modes reach their
// builder through here, so neither has a switch of its own and a mode that reported its
// summary differently would be visible in one place.
func emit(build func(harness.Reporter) string) int {
	summary := ""
	if err := harness.Command(func(r harness.Reporter) { summary = build(r) }); err != nil {
		fmt.Fprintf(os.Stderr, "avm-test: %v\n", err)
		return 1
	}
	fmt.Print(summary)
	return 0
}
