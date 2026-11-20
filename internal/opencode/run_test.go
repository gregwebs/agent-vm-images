package opencode

import (
	"context"
	"errors"
	"os"
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
	if outcome.status == 0 || outcome.start != nil {
		t.Fatalf("report = %+v, want a killed child", outcome)
	}
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
	go func() {
		done <- runReport(ctx, reportSpec{
			argv: []string{"sh", "-c", `trap '' TERM
sh -c 'trap "" TERM; while :; do printf x >> "$BEAT_FILE"; sleep 0.05; done' &
wait`},
			stdoutFile: filepath.Join(work, "out"),
			stderrFile: filepath.Join(work, "err"),
		})
	}()
	waitForProgress(t, beat)
	cancel()
	outcome := <-done
	if outcome.status != 128+9 {
		t.Fatalf("status = %d, want the KILL (137): TERM alone would have been 143", outcome.status)
	}
	before := fileSize(t, beat)
	time.Sleep(300 * time.Millisecond)
	if after := fileSize(t, beat); after != before {
		t.Fatalf("the session kept writing after cancellation (%d → %d bytes)", before, after)
	}
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
