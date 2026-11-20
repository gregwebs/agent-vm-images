package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// shrinkBounds shortens the reviewed bounds for one test and restores them. The
// bounds are package state because no caller may lengthen them; a test that has to
// observe a deadline has to move it.
func shrinkBounds(t *testing.T, bound time.Duration) {
	t.Helper()
	savedGrace, savedIdentity := killGrace, identityBound
	savedVersion, savedRuntime, savedProbe := versionBound, runtimeBound, probeBound
	killGrace, identityBound = bound, bound
	versionBound, runtimeBound, probeBound = bound, bound, bound
	t.Cleanup(func() {
		killGrace, identityBound = savedGrace, savedIdentity
		versionBound, runtimeBound, probeBound = savedVersion, savedRuntime, savedProbe
	})
}

// TestChildStatus pins the status mapping against real children rather than a
// hand-built error table: the shell's 127 for a program it cannot find, 126 for one
// it cannot execute, the child's own code, and 128+N for a signal.
func TestChildStatus(t *testing.T) {
	directory := t.TempDir()
	notExecutable := filepath.Join(directory, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name string
		argv []string
		want int
	}{
		{"own code", []string{"sh", "-c", "exit 3"}, 3},
		{"signal", []string{"sh", "-c", "kill -9 $$"}, 128 + 9},
		{"not found", []string{"opencode-fixture-absent-program"}, notFoundStatus},
		{"not executable", []string{notExecutable}, cannotExecuteStatus},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, status := runChild(context.Background(), childSpec{argv: testCase.argv})
			if status != testCase.want {
				t.Fatalf("status = %d, want %d", status, testCase.want)
			}
		})
	}
}

// TestInterruptStatuses drives the watcher with the real signals the predecessors
// trapped. A hand-built table would prove only that the table exists.
func TestInterruptStatuses(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		signal syscall.Signal
		status int
	}{
		{"interrupt", syscall.SIGINT, 130},
		{"terminate", syscall.SIGTERM, 143},
		{"hangup", syscall.SIGHUP, 129},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			watcher, stop := watchInterrupts()
			defer stop()
			if err := syscall.Kill(os.Getpid(), testCase.signal); err != nil {
				t.Fatalf("signal: %v", err)
			}
			select {
			case <-watcher.done:
			case <-time.After(10 * time.Second):
				t.Fatal("the watcher never saw the signal")
			}
			status, interrupted := watcher.exited()
			if !interrupted || status != testCase.status {
				t.Fatalf("exited = %d, %v, want %d, true", status, interrupted, testCase.status)
			}
			// A signal outranks the failure it caused.
			var interruptedFailure *Interrupted
			if err := interruptedOr(watcher, errors.New("boom")); !errors.As(err, &interruptedFailure) ||
				interruptedFailure.Status != testCase.status {
				t.Fatalf("interruptedOr = %v, want the signal status", err)
			}
		})
	}
}

// TestReportCapturesBothStreams is the envelope's contract: both files always
// exist, both streams land in them, and the child gets a HOME of its own that is
// gone afterwards.
func TestReportCapturesBothStreams(t *testing.T) {
	work := t.TempDir()
	stdoutFile := filepath.Join(work, "out")
	stderrFile := filepath.Join(work, "err")
	callerHome := os.Getenv("HOME")
	outcome := runReport(context.Background(), reportSpec{
		argv:       []string{"sh", "-c", `printf '%s\n' "$HOME"; printf 'diagnostic\n' >&2`},
		stdoutFile: stdoutFile,
		stderrFile: stderrFile,
	})
	if outcome.start != nil || outcome.status != 0 {
		t.Fatalf("report = %+v", outcome)
	}
	report, err := os.ReadFile(stdoutFile)
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, err := os.ReadFile(stderrFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(diagnostics) != "diagnostic\n" {
		t.Fatalf("captured stderr = %q", diagnostics)
	}
	home := strings.TrimSpace(string(report))
	if home == "" || home == callerHome {
		t.Fatalf("captured HOME = %q, want the child's own", home)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the private HOME %q survived the run", home)
	}
	// The files exist even when the program never starts, so a caller that reads
	// them always sees them.
	outcome = runReport(context.Background(), reportSpec{
		argv:       []string{"opencode-fixture-absent-program"},
		stdoutFile: stdoutFile,
		stderrFile: stderrFile,
	})
	if outcome.start == nil {
		t.Fatalf("an absent program was reported as started: %+v", outcome)
	}
	for _, path := range []string{stdoutFile, stderrFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

// TestReportBound keeps the deadline: a child that never answers is killed and its
// status is the signal's.
func TestReportBound(t *testing.T) {
	shrinkBounds(t, 100*time.Millisecond)
	work := t.TempDir()
	started := time.Now()
	outcome := runReport(context.Background(), reportSpec{
		argv:       []string{"sh", "-c", "exec sleep 300"},
		bound:      runtimeBound,
		stdoutFile: filepath.Join(work, "out"),
		stderrFile: filepath.Join(work, "err"),
	})
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("the bound did not fire: %s", elapsed)
	}
	// The child dies from the TERM this program sends, so the status is the signal's
	// and not `timeout`'s 124.
	if outcome.status != 128+int(syscall.SIGTERM) || outcome.start != nil {
		t.Fatalf("report = %+v, want status 143", outcome)
	}
}

// TestReportRefusesAnUnusableFirstCandidate keeps the envelope's resolution rule:
// the first PATH candidate that exists is the one run, so a present-but-unusable
// `node` is refused rather than silently skipped for a later, working one. Go's
// `exec.LookPath` skips it, which would let a gate certify a command the runtime
// cannot start.
func TestReportRefusesAnUnusableFirstCandidate(t *testing.T) {
	work := t.TempDir()
	bad := filepath.Join(work, "bad")
	good := filepath.Join(work, "good")
	for directory, mode := range map[string]os.FileMode{bad: 0o644, good: 0o755} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "node"), []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	// A dangling symlink is a candidate too: lstat says it is there.
	dangling := filepath.Join(work, "dangling")
	if err := os.MkdirAll(dangling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "nowhere"), filepath.Join(dangling, "node")); err != nil {
		t.Fatal(err)
	}
	for name, first := range map[string]string{"not executable": bad, "dangling symlink": dangling} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", first+string(os.PathListSeparator)+good)
			outcome := runReport(context.Background(), reportSpec{
				argv:       []string{"node", "--version"},
				stdoutFile: filepath.Join(work, "out"),
				stderrFile: filepath.Join(work, "err"),
			})
			if outcome.start == nil {
				t.Fatalf("the unusable first candidate was skipped: %+v", outcome)
			}
			if outcome.status != 0 {
				t.Fatalf("status = %d, want the start failure to be reported on its own", outcome.status)
			}
		})
	}
	// A name that no PATH entry carries is the same refusal, not a 127: the
	// envelope reported it and exited 1.
	t.Run("absent from PATH", func(t *testing.T) {
		t.Setenv("PATH", work)
		outcome := runReport(context.Background(), reportSpec{
			argv:       []string{"opencode-fixture-absent-program"},
			stdoutFile: filepath.Join(work, "out"),
			stderrFile: filepath.Join(work, "err"),
		})
		if outcome.start == nil {
			t.Fatalf("an absent program was reported as started: %+v", outcome)
		}
	})
}

// TestReportMapsClassifiedTransportStatus keeps the envelope's one remapping: a
// tool that happens to exit 75 must not look like a classified transport failure.
func TestReportMapsClassifiedTransportStatus(t *testing.T) {
	work := t.TempDir()
	outcome := runReport(context.Background(), reportSpec{
		argv:       []string{"sh", "-c", "exit 75"},
		stdoutFile: filepath.Join(work, "out"),
		stderrFile: filepath.Join(work, "err"),
	})
	if outcome.status != genericFailure {
		t.Fatalf("status = %d, want 1", outcome.status)
	}
}

// TestEndSessionTermsBeforeItKills keeps the order the comment in endSession calls
// load-bearing: the child's own TERM trap has to run before it is killed, which is
// what lets `run-pnpm.sh` forward the TERM to its manager. A KILL-only teardown
// would never reach the trap, so the file the trap writes is the evidence.
func TestEndSessionTermsBeforeItKills(t *testing.T) {
	shrinkBounds(t, 100*time.Millisecond)
	work := t.TempDir()
	trapped := filepath.Join(work, "trapped")
	ready := filepath.Join(work, "ready")
	t.Setenv("TRAPPED_FILE", trapped)
	t.Setenv("READY_FILE", ready)

	command := exec.Command("sh", "-c", `trap 'printf termed >"$TRAPPED_FILE"; exit 0' TERM
printf ready >"$READY_FILE"
while :; do sleep 0.02; done`)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL) })
	waitForProgress(t, ready)
	if err := endSession(command); err != nil {
		t.Fatalf("endSession: %v", err)
	}
	err := command.Wait()
	// The trap ran, which means TERM arrived while the child could still handle it.
	if _, statErr := os.Stat(trapped); statErr != nil {
		t.Fatalf("the child's TERM trap never ran: %v", statErr)
	}
	if err != nil {
		t.Fatalf("the TERMed child exited with %v, want its trap's 0", err)
	}
}

// TestCancelledSessionLeavesNothingAlive is the termination property the composed
// envelope did not have: `timeout` puts its own child in a new process group, so a
// signal to the envelope's group left that child running with nobody to forward
// anything. Here the leader *and* its descendant ignore TERM, so the 137 proves the
// KILL reached both, and the descendant's own heartbeat proves nothing kept
// executing.
//
// The heartbeat is the observable on purpose: a killed descendant is re-parented to
// init and can stay visible as a zombie, so "the pid is gone" is not the property
// and would fail on a host whose init does not reap. A process that is still running
// writes; a zombie cannot.
func TestCancelledSessionLeavesNothingAlive(t *testing.T) {
	shrinkBounds(t, 100*time.Millisecond)
	work := t.TempDir()
	beat := filepath.Join(work, "beat")
	t.Setenv("BEAT_FILE", beat)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan reportOutcome, 1)
	collected := false
	// The session is killed outright at the end, whatever happened above: a driver
	// that lost its own teardown -- which is what this test's mutant does -- must not
	// leave a loop running on the host.
	leaderFile := filepath.Join(work, "leader.pid")
	t.Setenv("LEADER_PID_FILE", leaderFile)
	t.Cleanup(func() {
		if pid := readPIDFile(leaderFile); pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
	t.Cleanup(func() {
		cancel()
		if collected {
			return
		}
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Errorf("the report run did not return after cancellation")
		}
	})
	go func() {
		done <- runReport(ctx, reportSpec{
			argv: []string{"sh", "-c", `trap '' TERM
printf '%s' $$ >"$LEADER_PID_FILE"
sh -c 'trap "" TERM; while :; do printf x >> "$BEAT_FILE"; sleep 0.05; done' &
wait`},
			stdoutFile: filepath.Join(work, "out"),
			stderrFile: filepath.Join(work, "err"),
		})
	}()
	waitForProgress(t, beat)
	cancel()
	outcome := <-done
	collected = true
	if outcome.status != 128+9 {
		t.Fatalf("status = %d, want the KILL (137): TERM alone would have been 143", outcome.status)
	}
	before := fileSize(t, beat)
	time.Sleep(300 * time.Millisecond)
	if after := fileSize(t, beat); after != before {
		t.Fatalf("the session kept writing after cancellation (%d → %d bytes)", before, after)
	}
}

// readPIDFile returns the pid a fixture wrote, or 0 when it has not written it.
func readPIDFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid := 0
	for _, digit := range strings.TrimSpace(string(data)) {
		if digit < '0' || digit > '9' {
			return 0
		}
		pid = pid*10 + int(digit-'0')
	}
	return pid
}

func waitForProgress(t *testing.T, path string) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		if fileSize(t, path) > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the fixture never started writing")
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
