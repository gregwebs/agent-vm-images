package toolchain

// Installed-state verification: what the installer must have left behind.
//
// The tool child environment is built, never inherited: an ambient
// RUSTUP_TOOLCHAIN, RUSTC_WRAPPER or LD_PRELOAD must not be able to change which
// toolchain answers these questions, so only PATH, LANG, LC_ALL and TZ pass
// through and the three toolchain variables are set here.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/strictjson"
)

// Homes are the two rustup homes the installer wrote.
type Homes struct {
	Rustup string
	Cargo  string
}

// Result is one finished tool process.
type Result struct {
	Stdout string
	Stderr string
	Status int
}

// Runner runs one tool child with exactly env and cwd (empty means the parent's).
type Runner interface {
	Run(ctx context.Context, argv []string, env []string, cwd string) (Result, error)
}

// ExecRunner is the real one: the program at argv[0], not resolved further,
// because the source reader execs `CARGO_HOME/bin/NAME` as given.
type ExecRunner struct{}

// Run executes argv with exactly env.
func (ExecRunner) Run(ctx context.Context, argv []string, env []string, cwd string) (Result, error) {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = env
	command.Dir = cwd
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.Status = exit.ExitCode()
			return result, nil
		}
		return result, err
	}
	return result, nil
}

// CheckInstalled applies every installed-state rule in the source reader's
// order. Each rule builds its own message, so both readers report the same first
// refusal.
func CheckInstalled(ctx context.Context, pin Pin, arch Architecture, homes Homes,
	runner Runner) error {
	closure := pin.Closure(arch)
	env := toolEnvironment(homes)
	rustup := filepath.Join(homes.Cargo, "bin", "rustup")
	settings := filepath.Join(homes.Rustup, "settings.toml")
	info, err := os.Stat(settings)
	if err != nil || !info.Mode().IsRegular() {
		return refuse("installed: missing rustup settings at %s", settings)
	}
	raw, err := os.ReadFile(settings)
	if err != nil {
		return refuse("installed: missing rustup settings at %s", settings)
	}
	values, err := decodeTOML(raw, "rustup settings.toml")
	if err != nil {
		return err
	}
	for _, pair := range settingsKeys {
		key, expected := pair[0], pair[1]
		value, present := values[key]
		if !present {
			value = nil
		}
		if text, ok := value.(string); !ok || text != expected {
			return refuse("installed: rustup setting %s is %s, expected %s", key,
				pyRepr(value), strictPythonRepr(expected))
		}
	}
	expectedToolchain := string(pin.Channel) + "-" + string(closure.Host)
	if value, present := values["default_toolchain"]; !present || value != expectedToolchain {
		return refuse("installed: rustup default toolchain is not the pinned exact channel")
	}
	if _, present := values["RUSTUP_TOOLCHAIN"]; present {
		return refuse("installed: rustup settings override RUSTUP_TOOLCHAIN")
	}

	version, err := requireSuccess(ctx, runner, []string{rustup, "--version"}, env, "",
		"rustup --version")
	if err != nil {
		return err
	}
	if !strings.Contains(version.Stdout, string(pin.RustupVersion)) {
		return refuse("installed: rustup is not %s: %s", pin.RustupVersion,
			strings.TrimSpace(version.Stdout))
	}
	listing, err := requireSuccess(ctx, runner, []string{rustup, "toolchain", "list"}, env, "",
		"rustup toolchain list")
	if err != nil {
		return err
	}
	toolchains := splitLinesStripped(listing.Stdout)
	if len(toolchains) != 1 || toolchains[0] != expectedToolchain+" (active, default)" {
		return refuse("installed: unexpected toolchains %s", pyListRepr(toolchains))
	}
	installed, err := requireSuccess(ctx, runner,
		[]string{rustup, "component", "list", "--installed"}, env, "", "rustup component list")
	if err != nil {
		return err
	}
	components := sortedFields(installed.Stdout)
	expectedComponents := make([]string, 0, len(SelectedPairs(arch)))
	for _, pair := range SelectedPairs(arch) {
		expectedComponents = append(expectedComponents,
			FilenameStems[pair.Package]+"-"+string(pair.Target))
	}
	sort.Strings(expectedComponents)
	if !sameStrings(components, expectedComponents) {
		return refuse("installed: components %s != %s", pyListRepr(components),
			pyListRepr(expectedComponents))
	}
	targetsOut, err := requireSuccess(ctx, runner,
		[]string{rustup, "target", "list", "--installed"}, env, "", "rustup target list")
	if err != nil {
		return err
	}
	targets := sortedFields(targetsOut.Stdout)
	expectedTargets := []string{string(closure.Host), string(closure.Musl)}
	sort.Strings(expectedTargets)
	if !sameStrings(targets, expectedTargets) {
		return refuse("installed: targets %s", pyListRepr(targets))
	}

	toolchainDir := filepath.Join(homes.Rustup, "toolchains", expectedToolchain)
	for _, target := range []TargetTriple{closure.Host, closure.Musl} {
		standard := filepath.Join(toolchainDir, "lib", "rustlib", string(target), "lib")
		if !hasRlib(standard) {
			return refuse("installed: missing %s standard library in %s", target, standard)
		}
	}

	verbose, err := requireSuccess(ctx, runner, []string{filepath.Join(homes.Cargo, "bin", "rustc"),
		"-vV"}, env, "", "rustc -vV")
	if err != nil {
		return err
	}
	fields := fieldMap(verbose.Stdout)
	if fields["release"] != string(pin.Channel) {
		return refuse("installed: rustc release %s != %s", pyReprString(fields, "release"),
			pin.Channel)
	}
	if fields["host"] != string(closure.Host) {
		return refuse("installed: rustc host %s != %s", pyReprString(fields, "host"), closure.Host)
	}
	tools := [][]string{
		{filepath.Join(homes.Cargo, "bin", "cargo"), "--version"},
		{filepath.Join(homes.Cargo, "bin", "cargo"), "clippy", "--version"},
		{filepath.Join(homes.Cargo, "bin", "rustfmt"), "--version"},
	}
	for _, tool := range tools {
		if _, err := requireSuccess(ctx, runner, tool, env, "",
			strings.Join(tool[1:], " ")); err != nil {
			return err
		}
	}
	return nil
}

// toolEnvironment is exactly the source reader's child environment.
func toolEnvironment(homes Homes) []string {
	env := make([]string, 0, 7)
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TZ"} {
		if value, present := os.LookupEnv(key); present {
			env = append(env, key+"="+value)
		}
	}
	if _, present := os.LookupEnv("PATH"); !present {
		env = append(env, "PATH="+imagesPath)
	}
	// Only the four above, plus the three pinned toolchain variables, are built:
	// the environment is copied key by key, never inherited, so no ambient
	// RUSTUP_TOOLCHAIN, RUSTC_WRAPPER or LD_PRELOAD can reach a tool. There is no
	// filter after the copy because there is nothing to filter out -- a filter
	// would be a rule with no input that could ever exercise it.
	return append(env,
		"RUSTUP_HOME="+homes.Rustup,
		"CARGO_HOME="+homes.Cargo,
		"RUSTUP_AUTO_INSTALL=0")
}

// requireSuccess applies the source reader's `require_success` rule.
func requireSuccess(ctx context.Context, runner Runner, argv, env []string, cwd string,
	what string) (Result, error) {
	result, err := runner.Run(ctx, argv, env, cwd)
	if err != nil {
		return result, refuse("%s failed: %v", what, err)
	}
	if result.Status != 0 {
		return result, refuse("%s failed with status %d: %s", what, result.Status,
			outputTail(result))
	}
	return result, nil
}

// outputTail is the last line of stderr, else of stdout, else "no output".
func outputTail(result Result) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	lines := strings.Split(detail, "\n")
	if detail == "" {
		return "no output"
	}
	return lines[len(lines)-1]
}

// splitLinesStripped is Python's `text.strip().splitlines()`.
func splitLinesStripped(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// sortedFields is Python's `sorted(text.split())`.
func sortedFields(text string) []string {
	fields := strings.Fields(text)
	sort.Strings(fields)
	return fields
}

// hasRlib reports whether dir holds a `*.rlib` entry (glob does not match a
// dotfile, so neither does this).
func hasRlib(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".rlib") {
			return true
		}
	}
	return false
}

// fieldMap is Python's `dict(line.split(': ', 1) for line in lines if ': ' in
// line)`: the last duplicate wins.
func fieldMap(text string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		fields[key] = value
	}
	return fields
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// pyRepr renders a decoded TOML value the way Python's repr does for the values
// a settings diagnostic can name.
func pyRepr(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		return strictPythonRepr(typed)
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	default:
		// A TOML date, time or datetime: named, not spelled as Python's own repr,
		// because the diagnostic's fragment is the setting name.
		return pyTypeName(typed) + "(" + fmt.Sprint(typed) + ")"
	}
}

func pyTypeName(value any) string {
	switch value.(type) {
	case string:
		return "str"
	case int64:
		return "int"
	case float64:
		return "float"
	case bool:
		return "bool"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	default:
		return "object"
	}
}

// pyListRepr is Python's list repr of strings.
func pyListRepr(values []string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strictPythonRepr(value))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// pyReprString is `repr(fields.get(key))`: a string repr, or None when absent.
func pyReprString(fields map[string]string, key string) string {
	value, present := fields[key]
	if !present {
		return "None"
	}
	return strictPythonRepr(value)
}

// strictPythonRepr is Python's repr for the ASCII strings these diagnostics
// name.
func strictPythonRepr(text string) string { return strictjson.Repr(text) }

// imagesPath is the fallback PATH: the image's own directories.
const imagesPath = "/usr/bin:/bin"
