package cli

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/toolaccess"
)

// processChildEnv marks the re-executed test binary as the real command, so the
// stdio-failure and non-UTF8 vectors run against actual file descriptors instead
// of writer doubles.
const processChildEnv = "RECIPE_CONTRACT_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(processChildEnv) == "1" {
		os.Exit(runChild())
	}
	os.Exit(m.Run())
}

func runChild() int {
	return Run(os.Args[1:], ProcessEnvironment(), Streams{Out: os.Stdout, Err: DiagnosticStream(os.Stderr)})
}

type result struct {
	stdout []byte
	stderr []byte
	status int
}

func testEnv(workingDirectory toolaccess.PhysicalDirectory, path string) toolaccess.Environment {
	return toolaccess.Environment{
		WorkingDirectory: func() (toolaccess.PhysicalDirectory, error) { return workingDirectory, nil },
		Path:             toolaccess.PathValue(path),
	}
}

func runInProcess(t *testing.T, env toolaccess.Environment, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := Run(args, env, Streams{Out: &stdout, Err: &stderr})
	return result{stdout: stdout.Bytes(), stderr: stderr.Bytes(), status: status}
}

type streamTarget string

const (
	streamCapture streamTarget = ""
	streamFull    streamTarget = "full"
	streamClosed  streamTarget = "closed"
)

type childSpec struct {
	args   []string
	cwd    string
	env    []string
	stdout streamTarget
	stderr streamTarget
}

func runChildProcess(t *testing.T, spec childSpec) result {
	t.Helper()
	command := exec.Command(os.Args[0], spec.args...)
	command.Env = append([]string{processChildEnv + "=1"}, spec.env...)
	command.Dir = spec.cwd
	var stdoutFile, stderrFile *os.File
	var pipeWrite *os.File
	switch spec.stdout {
	case streamFull:
		file, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("cannot open /dev/full: %v", err)
		}
		stdoutFile = file
		command.Stdout = file
	case streamClosed:
		readEnd, writeEnd, err := os.Pipe()
		if err != nil {
			t.Fatalf("cannot create pipe: %v", err)
		}
		_ = readEnd.Close()
		pipeWrite = writeEnd
		command.Stdout = writeEnd
	}
	if spec.stderr == "full" {
		file, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("cannot open /dev/full: %v", err)
		}
		stderrFile = file
		command.Stderr = file
	}
	var stdoutBuffer, stderrBuffer bytes.Buffer
	if command.Stdout == nil {
		command.Stdout = &stdoutBuffer
	}
	if command.Stderr == nil {
		command.Stderr = &stderrBuffer
	}
	err := command.Run()
	for _, handle := range []*os.File{stdoutFile, stderrFile, pipeWrite} {
		if handle != nil {
			_ = handle.Close()
		}
	}
	status := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("cannot run the child: %v", err)
		}
		status = exitError.ExitCode()
	}
	return result{stdout: stdoutBuffer.Bytes(), stderr: stderrBuffer.Bytes(), status: status}
}

type golden struct {
	name   string
	status int
	stdout []byte
	stderr []byte
}

func loadGoldens(t *testing.T) map[string]golden {
	t.Helper()
	handle, err := os.Open(filepath.Join("testdata", "goldens.txt"))
	if err != nil {
		t.Fatalf("cannot read the goldens: %v", err)
	}
	defer func() { _ = handle.Close() }()
	goldens := make(map[string]golden)
	var current golden
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	flush := func() {
		if current.name != "" {
			goldens[current.name] = current
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "case: "):
			flush()
			current = golden{name: strings.TrimPrefix(line, "case: ")}
		case strings.HasPrefix(line, "status: "):
			status, err := strconv.Atoi(strings.TrimPrefix(line, "status: "))
			if err != nil {
				t.Fatalf("bad status line %q: %v", line, err)
			}
			current.status = status
		case strings.HasPrefix(line, "stdout.hex: "):
			decoded, err := hex.DecodeString(strings.TrimSpace(strings.TrimPrefix(line, "stdout.hex: ")))
			if err != nil {
				t.Fatalf("bad stdout hex: %v", err)
			}
			current.stdout = decoded
		case strings.HasPrefix(line, "stderr.hex: "):
			decoded, err := hex.DecodeString(strings.TrimSpace(strings.TrimPrefix(line, "stderr.hex: ")))
			if err != nil {
				t.Fatalf("bad stderr hex: %v", err)
			}
			current.stderr = decoded
		default:
			t.Fatalf("unexpected golden line %q", line)
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		t.Fatalf("cannot scan the goldens: %v", err)
	}
	return goldens
}

// newFixture creates an audit directory on a physical, world-traversable path.
func newFixture(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "recipe-contract-cli-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return physical
}

func expandFixture(expected []byte, fixture string) []byte {
	return bytes.ReplaceAll(expected, []byte("${FIXTURE}"), []byte(fixture))
}

func expectResult(t *testing.T, name string, want golden, got result) {
	t.Helper()
	if got.status != want.status {
		t.Fatalf("%s: status = %d, want %d (stderr %q)", name, got.status, want.status, got.stderr)
	}
	if !bytes.Equal(got.stdout, want.stdout) {
		t.Fatalf("%s: stdout = %q, want %q", name, got.stdout, want.stdout)
	}
	if !bytes.Equal(got.stderr, want.stderr) {
		t.Fatalf("%s: stderr = %q, want %q", name, got.stderr, want.stderr)
	}
}

func TestGoldenMessages(t *testing.T) {
	t.Parallel()
	goldens := loadGoldens(t)
	fixture := newFixture(t)
	cases := []struct {
		name string
		spec childSpec
	}{
		{name: "help", spec: childSpec{args: []string{"tool-access", "--help"}, cwd: fixture}},
		{name: "usage-install-status", spec: childSpec{args: []string{"install-status", "path-state"}, cwd: fixture}},
		{name: "usage-tool-access", spec: childSpec{args: []string{"tool-access"}, cwd: fixture}},
		{name: "usage-dispatcher", spec: childSpec{args: []string{"bogus"}, cwd: fixture}},
		{name: "unknown-option-nonutf8", spec: childSpec{args: []string{"tool-access", "--bad-\xff"}, cwd: fixture}},
		{name: "unknown-subcommand-nonutf8", spec: childSpec{args: []string{"install-status", "bad-\xff", "arg"}, cwd: fixture}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			test.spec.env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
			want := goldens[test.name]
			want.stdout = expandFixture(want.stdout, fixture)
			want.stderr = expandFixture(want.stderr, fixture)
			expectResult(t, test.name, want, runChildProcess(t, test.spec))
		})
	}
}

func TestGoldenNonUTF8Paths(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("invalid UTF-8 filenames are a Linux vector; the Linux run owns these goldens")
	}
	goldens := loadGoldens(t)
	fixture := newFixture(t)
	path := filepath.Join(fixture, "bad-\xff")
	if err := os.WriteFile(path, []byte("plain\n"), 0o644); err != nil {
		t.Fatalf("cannot create an invalid UTF-8 filename: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	environment := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}

	violation := goldens["violation-nonutf8"]
	violation.stdout = expandFixture(violation.stdout, fixture)
	violation.stderr = expandFixture(violation.stderr, fixture)
	expectResult(t, "violation-nonutf8", violation, runChildProcess(t, childSpec{
		args: []string{"tool-access", path}, cwd: fixture, env: environment,
	}))

	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	success := goldens["success-nonutf8"]
	success.stdout = expandFixture(success.stdout, fixture)
	success.stderr = expandFixture(success.stderr, fixture)
	expectResult(t, "success-nonutf8", success, runChildProcess(t, childSpec{
		args: []string{"tool-access", path}, cwd: fixture, env: environment,
	}))
}

func TestGoldenStreamFailures(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("/dev/full is a Linux vector; the Linux run owns these goldens")
	}
	goldens := loadGoldens(t)
	fixture := newFixture(t)
	plain := filepath.Join(fixture, "plain")
	if err := os.WriteFile(plain, []byte("plain\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	environment := []string{"PATH=" + fixture + ":/usr/bin:/bin", "LANG=C.UTF-8"}

	expectResult(t, "deferred-flush-full", goldens["deferred-flush-full"], runChildProcess(t, childSpec{
		args: []string{"tool-access", "--help"}, cwd: fixture, env: environment, stdout: streamFull,
	}))

	// Quiet success writes nothing, so a failing stdout must not invent a flush
	// failure: the original status is preserved.
	quiet := runChildProcess(t, childSpec{
		args: []string{"tool-access", "--quiet", "./plain"}, cwd: fixture, env: environment,
		stdout: streamFull,
	})
	if quiet.status != 0 || len(quiet.stderr) != 0 {
		t.Fatalf("quiet success with a failing stdout = %+v", quiet)
	}

	// A buffered success line, then a violation, then a failing flush: the
	// violation stays on stderr and the flush failure decides the status.
	mixed := runChildProcess(t, childSpec{
		args: []string{"tool-access", "./plain", "no-such-target"}, cwd: fixture, env: environment,
		stdout: streamFull,
	})
	wantStderr := []byte("tool-access: no-such-target: not found on PATH\n" +
		"Exception ignored on flushing sys.stdout:\nOSError: [Errno 28] No space left on device\n")
	if mixed.status != 120 || !bytes.Equal(mixed.stderr, wantStderr) {
		t.Fatalf("success-then-violation = %+v, want status 120 and %q", mixed, wantStderr)
	}

	// Enough output to cross the buffer: the write fails while running, so the
	// status is the legacy 1 with the exception line and no source frames.
	arguments := make([]string, 0, 1002)
	arguments = append(arguments, "tool-access")
	for index := 0; index < 1000; index++ {
		arguments = append(arguments, "./plain")
	}
	synchronous := runChildProcess(t, childSpec{
		args: arguments, cwd: fixture, env: environment, stdout: streamFull,
	})
	expectResult(t, "synchronous-flush-full", goldens["synchronous-flush-full"], synchronous)

	// 4096..8191 bytes: CPython 3.13.5 swallows the error and exits 0; Go keeps
	// the documented diagnostic (ADR 0004 records the difference).
	lineLength := 17 + len(fixture) + 1 + len("plain")
	midrangeCount := 6000 / lineLength
	midrangeArguments := make([]string, 0, midrangeCount+2)
	midrangeArguments = append(midrangeArguments, "tool-access")
	for index := 0; index < midrangeCount; index++ {
		midrangeArguments = append(midrangeArguments, "./plain")
	}
	midrange := runChildProcess(t, childSpec{
		args: midrangeArguments, cwd: fixture, env: environment, stdout: streamFull,
	})
	expectResult(t, "midrange-flush-full", goldens["midrange-flush-full"], midrange)

	// A failing stderr must not move diagnostics to stdout and must report 120.
	failingStderr := runChildProcess(t, childSpec{
		args: []string{"tool-access", "--bogus"}, cwd: fixture, env: environment, stderr: streamFull,
	})
	if failingStderr.status != 120 || len(failingStderr.stdout) != 0 {
		t.Fatalf("failing stderr = %+v, want status 120 and empty stdout", failingStderr)
	}
}

func TestArgvPrecedence(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	plain := filepath.Join(fixture, "plain")
	if err := os.WriteFile(plain, []byte("plain\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	env := testEnv(toolaccess.PhysicalDirectoryOf(fixture), fixture+":/usr/bin:/bin")
	cases := []struct {
		name       string
		args       []string
		wantStatus int
		wantOut    string
		wantErr    string
	}{
		{name: "no-args", args: nil, wantStatus: 2, wantErr: dispatcherUsage},
		{name: "help-first", args: []string{"tool-access", "--help"}, wantStatus: 0, wantOut: toolAccessHelp},
		{name: "help-after-target", args: []string{"tool-access", "--quiet", "./plain", "--help"}, wantStatus: 0, wantOut: toolAccessHelp},
		{name: "help-after-mode-conflict", args: []string{"tool-access", "--content", "--tree", "-h"}, wantStatus: 0, wantOut: toolAccessHelp},
		{name: "unknown-before-help", args: []string{"tool-access", "--bogus", "--help"}, wantStatus: 2, wantErr: "check-tool-access.py: unknown option --bogus\n"},
		{name: "unknown-double-dash", args: []string{"tool-access", "--"}, wantStatus: 2, wantErr: "check-tool-access.py: unknown option --\n"},
		{name: "unknown-lone-dash", args: []string{"tool-access", "-"}, wantStatus: 2, wantErr: "check-tool-access.py: unknown option -\n"},
		{name: "unknown-quiet-equals", args: []string{"tool-access", "--quiet=x"}, wantStatus: 2, wantErr: "check-tool-access.py: unknown option --quiet=x\n"},
		{name: "no-target", args: []string{"tool-access", "--quiet"}, wantStatus: 2, wantErr: toolAccessUsage},
		{name: "content-and-tree", args: []string{"tool-access", "--content", "--tree", "./plain"}, wantStatus: 2, wantErr: toolAccessUsage},
		{name: "empty-target", args: []string{"tool-access", ""}, wantStatus: 1, wantErr: "tool-access: " + fixture + "/: resolves to no final component\n"},
		{name: "success", args: []string{"tool-access", "./plain"}, wantStatus: 0, wantOut: "tool-access: ok " + plain + "\n"},
		{name: "quiet-success", args: []string{"tool-access", "--quiet", "./plain"}, wantStatus: 0},
		{name: "first-ok-second-fails-third-not-run", args: []string{"tool-access", "./plain", "absent", "./plain"},
			wantStatus: 1, wantOut: "tool-access: ok " + plain + "\n",
			wantErr: "tool-access: absent: not found on PATH\n"},
		{name: "install-usage-arity", args: []string{"install-status", "path-state"}, wantStatus: 2, wantErr: installStatusUsage},
		{name: "install-arity-before-subcommand", args: []string{"install-status", "bogus"}, wantStatus: 2, wantErr: installStatusUsage},
		{name: "install-unknown-subcommand", args: []string{"install-status", "bogus", "arg"}, wantStatus: 2, wantErr: "install-status.py: unknown subcommand bogus\n"},
		{name: "install-path-state-absent", args: []string{"install-status", "path-state", filepath.Join(fixture, "absent")}, wantStatus: 0},
		{name: "install-path-state-present", args: []string{"install-status", "path-state", plain}, wantStatus: 1},
		{name: "install-path-state-dotdot", args: []string{"install-status", "path-state", ".."}, wantStatus: 2},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := runInProcess(t, env, test.args...)
			if got.status != test.wantStatus {
				t.Fatalf("status = %d, want %d (stdout %q stderr %q)", got.status, test.wantStatus, got.stdout, got.stderr)
			}
			if string(got.stdout) != test.wantOut {
				t.Fatalf("stdout = %q, want %q", got.stdout, test.wantOut)
			}
			if string(got.stderr) != test.wantErr {
				t.Fatalf("stderr = %q, want %q", got.stderr, test.wantErr)
			}
		})
	}
}

func TestRecordsAndCwdIndependence(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	write := func(name, content string) string {
		path := filepath.Join(fixture, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	installed := write("record-installed", "installed\n")
	transport := write("record-transport", "absent-transport ECONNRESET\n")
	broken := write("record-broken", "absent-transport 99\n")
	failingCwd := toolaccess.Environment{
		WorkingDirectory: func() (toolaccess.PhysicalDirectory, error) {
			return toolaccess.PhysicalDirectory{}, errors.New("getcwd failed")
		},
		Path: toolaccess.PathValue("/usr/bin:/bin"),
	}
	if got := runInProcess(t, failingCwd, "install-status", "record", installed); got.status != 0 || string(got.stdout) != "installed\n" {
		t.Fatalf("record with a failing cwd = %+v", got)
	}
	if got := runInProcess(t, failingCwd, "install-status", "code", transport); got.status != 0 || string(got.stdout) != "ECONNRESET\n" {
		t.Fatalf("code with a failing cwd = %+v", got)
	}
	if got := runInProcess(t, failingCwd, "install-status", "code", installed); got.status != 1 || len(got.stdout) != 0 {
		t.Fatalf("code of a non-transport record = %+v", got)
	}
	if got := runInProcess(t, failingCwd, "install-status", "code", broken); got.status != 1 {
		t.Fatalf("code of an unknown transport code = %+v", got)
	}
	if got := runInProcess(t, failingCwd, "install-status", "record", broken); got.status != 1 {
		t.Fatalf("record of an unknown record = %+v", got)
	}
	if got := runInProcess(t, failingCwd, "tool-access", "--help"); got.status != 0 {
		t.Fatalf("help with a failing cwd = %+v", got)
	}
	if got := runInProcess(t, failingCwd, "tool-access", "/usr/bin/env"); got.status != 0 {
		t.Fatalf("absolute audit with a failing cwd = %+v", got)
	}
	if got := runInProcess(t, failingCwd, "tool-access", "env"); got.status != 0 {
		t.Fatalf("absolute PATH candidate with a failing cwd = %+v", got)
	}
}

func TestTreeRootEstablishment(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	tree := filepath.Join(fixture, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "file"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := testEnv(toolaccess.PhysicalDirectoryOf(fixture), "/usr/bin:/bin")
	if got := runInProcess(t, env, "tool-access", "--tree", "./tree"); got.status != 0 {
		t.Fatalf("tree audit = %+v", got)
	}
	// The tree root is bound to cwd, never to PATH: a PATH-only name is not a
	// directory and must fail rather than certify a command.
	got := runInProcess(t, env, "tool-access", "--tree", "file")
	if got.status != 1 || !strings.Contains(string(got.stderr), "does not exist") {
		t.Fatalf("PATH-resolved tree target = %+v", got)
	}
}

func TestOutputStreamsStaySeparate(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	path := filepath.Join(fixture, "nonexec")
	if err := os.WriteFile(path, []byte("plain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := testEnv(toolaccess.PhysicalDirectoryOf(fixture), "/usr/bin:/bin")
	var stdout, stderr bytes.Buffer
	status := Run([]string{"tool-access", "./nonexec"}, env, Streams{Out: &stdout, Err: &stderr})
	if status != 1 {
		t.Fatalf("status = %d", status)
	}
	if stdout.Len() != 0 {
		t.Fatalf("violation wrote to stdout: %q", stdout.String())
	}
	want := "tool-access: " + path + ": not executable by all (mode 644)\n"
	if stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}
}

// runShellChild runs the child through `sh`, the only way a test can hand it a
// closed file descriptor or a cwd that no longer exists. The script sees the test
// binary as $0 and the child argv as "$@".
func runShellChild(t *testing.T, spec childSpec, script string) result {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot locate the test binary: %v", err)
	}
	command := exec.Command("sh", append([]string{"-c", script, binary}, spec.args...)...)
	command.Env = append([]string{processChildEnv + "=1"}, spec.env...)
	command.Dir = spec.cwd
	var stdoutBuffer, stderrBuffer bytes.Buffer
	command.Stdout = &stdoutBuffer
	command.Stderr = &stderrBuffer
	err = command.Run()
	status := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("cannot run the child: %v", err)
		}
		status = exitError.ExitCode()
	}
	return result{stdout: stdoutBuffer.Bytes(), stderr: stderrBuffer.Bytes(), status: status}
}

// TestBothStreamFailureStatus pins the simultaneous-failure status: a synchronous
// stdout failure whose stderr report also fails left CPython's exit flush failing,
// so the interpreter returned 120. A closed stderr is different: the interpreter
// had no sys.stderr at all, dropped the report and kept status 1.
func TestBothStreamFailureStatus(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("/dev/full is a Linux vector; the Linux run owns these goldens")
	}
	goldens := loadGoldens(t)
	fixture := newFixture(t)
	plain := filepath.Join(fixture, "plain")
	if err := os.WriteFile(plain, []byte("plain\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	environment := []string{"PATH=" + fixture + ":/usr/bin:/bin", "LANG=C.UTF-8"}
	arguments := make([]string, 0, 1002)
	arguments = append(arguments, "tool-access")
	for index := 0; index < 1000; index++ {
		arguments = append(arguments, "./plain")
	}
	expectResult(t, "both-streams-full", goldens["both-streams-full"], runChildProcess(t, childSpec{
		args: arguments, cwd: fixture, env: environment, stdout: streamFull, stderr: streamFull,
	}))

	// stderr closed rather than failing: the legacy interpreter had no sys.stderr
	// and dropped the report, so the synchronous failure keeps status 1.
	closed := runShellChild(t, childSpec{args: arguments, cwd: fixture, env: environment},
		`exec "$0" "$@" >/dev/full 2>&-`)
	if closed.status != synchronousFailureStatus || len(closed.stdout) != 0 || len(closed.stderr) != 0 {
		t.Fatalf("closed stderr = %+v, want status %d and no bytes", closed, synchronousFailureStatus)
	}
}

// TestMergedStreamDiagnosticOffset pins where a diagnostic lands when stdout and
// stderr share one descriptor (`2>&1`). CPython's TextIOWrapper flushes the
// *pending* text before appending a write that would not fit, so the diagnostic
// follows the last whole line that fit in one buffer. Appending first and
// flushing afterwards leaves it one line later. Measured against CPython 3.13.5
// on the maintained image: with 34-byte success lines the diagnostic starts at
// byte 8160 (241 lines), never at 8194.
func TestMergedStreamDiagnosticOffset(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	// The success line names the resolved path, so the fixture length decides the
	// line length. Pick a name whose line does not divide the buffer exactly: at an
	// exact multiple the two orders agree and the case would not distinguish them.
	prefix := "tool-access: ok " + fixture + "/"
	name := "plain"
	for pythonBufferSize%(len(prefix)+len(name)+1) == 0 {
		name += "x"
	}
	plain := filepath.Join(fixture, name)
	if err := os.WriteFile(plain, []byte("plain\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	lineLength := len(prefix) + len(name) + 1
	// 200 lines overflow one buffer and stay below two, so exactly one flush
	// precedes the diagnostic and the expectation is a single product.
	const targets = 200
	if targets*lineLength >= 2*pythonBufferSize {
		t.Fatalf("fixture lines of %d bytes overflow two buffers; the case needs one", lineLength)
	}
	arguments := []string{"tool-access"}
	for index := 0; index < targets; index++ {
		arguments = append(arguments, "./"+name)
	}
	arguments = append(arguments, "./missing")
	got := runShellChild(t, childSpec{args: arguments, cwd: fixture,
		env: []string{"PATH=" + fixture + ":/usr/bin:/bin", "LANG=C.UTF-8"}},
		`exec "$0" "$@" 2>&1`)
	if got.status != 1 {
		t.Fatalf("status = %d, want 1 (stderr %q)", got.status, got.stderr)
	}
	diagnostic := []byte("tool-access: " + filepath.Join(fixture, "missing") + ": does not exist\n")
	offset := bytes.Index(got.stdout, diagnostic)
	if offset < 0 {
		t.Fatalf("missing the violation diagnostic in %d bytes of output", len(got.stdout))
	}
	if want := (pythonBufferSize / lineLength) * lineLength; offset != want {
		t.Fatalf("diagnostic at byte %d, want %d (line %d bytes): the pending text must flush before an oversized append",
			offset, want, lineLength)
	}
}

// TestInvalidByteRunsUsePythonReplaceDecoding pins the ordinary audit diagnostic
// for runs of invalid bytes: Python decoded the offending env option with
// `decode("utf-8", "replace")`, which emits one U+FFFD per maximal ill-formed
// subpart. Inputs and expectations were measured on CPython 3.13.5.
func TestInvalidByteRunsUsePythonReplaceDecoding(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	env := testEnv(toolaccess.PhysicalDirectoryOf(fixture), "/usr/bin:/bin")
	cases := []struct {
		name     string
		shebang  string
		expected string
	}{
		{name: "one invalid byte", shebang: "#!/usr/bin/env -S -x\xff sh",
			expected: "tool-access: /usr/bin/env: unsupported env option '-x\uFFFD'\n"},
		{name: "two invalid bytes", shebang: "#!/usr/bin/env -S -x\xff\xff sh",
			expected: "tool-access: /usr/bin/env: unsupported env option '-x\uFFFD\uFFFD'\n"},
		{name: "truncated three-byte and invalid", shebang: "#!/usr/bin/env -S -x\xe2\x80\xff sh",
			expected: "tool-access: /usr/bin/env: unsupported env option '-x\uFFFD\uFFFD'\n"},
		{name: "truncated four-byte and invalid", shebang: "#!/usr/bin/env -S -x\xf0\x9f\x98\xff sh",
			expected: "tool-access: /usr/bin/env: unsupported env option '-x\uFFFD\uFFFD'\n"},
		{name: "encoded surrogate", shebang: "#!/usr/bin/env -S -x\xed\xa0\x80 sh",
			expected: "tool-access: /usr/bin/env: unsupported env option '-x\uFFFD\uFFFD\uFFFD'\n"},
		{name: "truncated prefix only", shebang: "#!/usr/bin/env -S -x\xe2\x82 sh",
			expected: "tool-access: /usr/bin/env: unsupported env option '-x\uFFFD'\n"},
	}
	for index, test := range cases {
		path := filepath.Join(fixture, "env-option-"+strconv.Itoa(index))
		if err := os.WriteFile(path, []byte(test.shebang+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
		got := runInProcess(t, env, "tool-access", path)
		if got.status != 1 || len(got.stdout) != 0 || string(got.stderr) != test.expected {
			t.Fatalf("%s: %+v, want status 1 and %q", test.name, got, test.expected)
		}
	}
}

// TestUncaughtExceptionClass pins the fallback report for internal failures: a
// removed cwd is a FileNotFoundError in the legacy helper, and a NUL byte in a
// derived path is the ValueError its lstat raised before the syscall.
func TestUncaughtExceptionClass(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	environment := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	nullScript := filepath.Join(fixture, "nul-interpreter")
	if err := os.WriteFile(nullScript, []byte("#!/bin/sh\x00x\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nullScript, 0o755); err != nil {
		t.Fatal(err)
	}
	nullPath := filepath.Join(fixture, "nul-path")
	if err := os.WriteFile(nullPath, []byte("#!/usr/bin/env -S PATH=/bi\x00n sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nullPath, 0o755); err != nil {
		t.Fatal(err)
	}
	valueError := "ValueError: lstat: embedded null character in path\n"
	for name, target := range map[string]string{"nul interpreter": nullScript, "nul env PATH": nullPath} {
		got := runChildProcess(t, childSpec{args: []string{"tool-access", target}, cwd: fixture, env: environment})
		if got.status != synchronousFailureStatus || string(got.stderr) != valueError {
			t.Fatalf("%s: %+v, want status 1 and %q", name, got, valueError)
		}
	}

	// A removed cwd makes getcwd(3) fail before any path is resolved. Each shape
	// gets its own directory: the removal is the fixture, so a shared one would
	// make the second case's setup fail instead of its assertion. Linux only:
	// xnu's getcwd still names a deleted vnode, so macOS has no such vector.
	if runtime.GOOS != "linux" {
		t.Skip("removed cwd is a Linux vector; the Linux run owns these goldens")
	}
	want := "FileNotFoundError: [Errno 2] No such file or directory\n"
	for index, arguments := range [][]string{
		{"tool-access", "./plain"},
		{"tool-access", "--tree", "plain"},
	} {
		doomed := filepath.Join(fixture, "doomed-"+strconv.Itoa(index))
		if err := os.Mkdir(doomed, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(doomed, 0o755); err != nil {
			t.Fatal(err)
		}
		removed := runShellChild(t, childSpec{args: arguments, cwd: doomed, env: environment},
			`rmdir "$PWD" && exec "$0" "$@"`)
		if removed.status != synchronousFailureStatus || string(removed.stderr) != want {
			t.Fatalf("%v: %+v, want status 1 and %q", arguments, removed, want)
		}
	}
}
