package publication

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The six windows of #213 item 3 (P1–P6) are a contract with the maintainer, and
// every one of them ends a process. Only a real process can be asked what status it
// produced, so each case re-executes this test binary and the child delivers its
// signal through the transaction's own window hook.
const (
	caseVariable   = "PUBLICATION_WINDOW_CASE"
	rootVariable   = "PUBLICATION_WINDOW_ROOT"
	workVariable   = "PUBLICATION_WINDOW_WORK"
	ownerVariable  = "PUBLICATION_WINDOW_OWNER"
	windowVar      = "PUBLICATION_WINDOW_NAME"
	indexVariable  = "PUBLICATION_WINDOW_INDEX"
	signalVar      = "PUBLICATION_WINDOW_SIGNAL"
	failVariable   = "PUBLICATION_WINDOW_FAIL_COPY"
	refuseVariable = "PUBLICATION_WINDOW_REFUSE_RESTORE"
	cleanupVar     = "PUBLICATION_WINDOW_FAIL_CLEANUP"
)

// reportLine is the success line the child asks the transaction to print. A decided
// 0 must print it and every undecided window must not: they are two different
// owners of the same sentence otherwise.
const reportLine = "published every destination"

func TestMain(m *testing.M) {
	if os.Getenv(caseVariable) != "" {
		os.Exit(childStatus())
	}
	os.Exit(m.Run())
}

// childStatus runs one publication with the window the parent named, and returns the
// status the transaction ended the process with.
func childStatus() int {
	root := os.Getenv(rootVariable)
	transaction := &Transaction{
		Prefix: "pin-artifact: ",
		Root:   root,
		Work:   os.Getenv(workVariable),
		Stderr: os.Stderr,
		Report: Report{To: os.Stdout, Lines: []string{reportLine}},
		Destinations: []string{
			filepath.Join(root, "images/artifacts/node.json"),
			filepath.Join(root, "images/Dockerfile"),
			filepath.Join(root, "images/artifacts/node-release-key.asc"),
		},
		Staged: [][]byte{
			[]byte("staged node pin\n"),
			[]byte("staged dockerfile\n"),
			[]byte("staged key\n"),
		},
	}
	if name := os.Getenv(failVariable); name != "" {
		once := false
		failOn(transaction, name, &once)
	}
	if refused := os.Getenv(refuseVariable); refused != "" {
		if refused == "all" {
			transaction.copyFile = refusingRestore(transaction.copyFile)
		} else {
			transaction.copyFile = refusingRestoreOf(transaction.copyFile, refused)
		}
	}
	if at := window(os.Getenv(windowVar)); at != "" {
		// indexOf is the destination index the window must be reached at: the copy
		// the signal lands after, or the count of copies for the two windows that sit
		// after them all.
		indexOf, err := strconv.Atoi(os.Getenv(indexVariable))
		if err != nil {
			fmt.Fprintf(os.Stderr, "the parent named no window index: %v\n", err)
			return 97
		}
		transaction.hook = func(reached window, reachedIndex int) {
			if reached != at || reachedIndex != indexOf {
				return
			}
			fmt.Fprintf(os.Stderr, "%swindow %s %d\n", transaction.Prefix, reached, reachedIndex)
			if err := syscall.Kill(os.Getpid(), namedSignal()); err != nil {
				fmt.Fprintf(os.Stderr, "cannot deliver the signal: %v\n", err)
				return
			}
			// The handler records the signal before it takes the lock, so waiting on
			// that record is what makes the publication's next window check see the
			// signal. A sleep would be a guess about a scheduler.
			<-transaction.interrupted
		}
	}
	if os.Getenv(cleanupVar) != "" {
		owner := os.Getenv(ownerVariable)
		transaction.removeAll = func(path string) error {
			if path == owner {
				// #138's observed shape: the caller's scratch holds something that will
				// not go away, which is a leftover and not recovery material.
				return os.ErrPermission
			}
			return os.RemoveAll(path)
		}
	}
	return transaction.Run(os.Getenv(ownerVariable))
}

func namedSignal() syscall.Signal {
	if os.Getenv(signalVar) == "TERM" {
		return syscall.SIGTERM
	}
	return syscall.SIGINT
}

// windowResult is one re-executed publication: the status a process really produced,
// and the two streams it produced it with.
type windowResult struct {
	status int
	stdout string
	stderr string
}

// require reports a line the case depends on. A case whose window hook never fired
// names this first: the hook is the evidence that the signal landed where the case
// claims.
func (r windowResult) require(t *testing.T, line string) {
	t.Helper()
	if !strings.Contains(r.stderr, line) {
		t.Fatalf("the run never said %q\nstatus %d\nstdout:\n%s\nstderr:\n%s",
			line, r.status, r.stdout, r.stderr)
	}
}

// requireNone reports a line the case forbids. "No line" is the whole assertion for a
// window that must not claim retention or success.
func (r windowResult) requireNone(t *testing.T, line string) {
	t.Helper()
	if strings.Contains(r.stderr, line) || strings.Contains(r.stdout, line) {
		t.Fatalf("the run said %q, which this window forbids\nstatus %d\nstdout:\n%s\nstderr:\n%s",
			line, r.status, r.stdout, r.stderr)
	}
}

func (r windowResult) requireStatus(t *testing.T, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status %d, want %d\nstdout:\n%s\nstderr:\n%s",
			r.status, want, r.stdout, r.stderr)
	}
}

func runWindow(t *testing.T, environment map[string]string) windowResult {
	t.Helper()
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(), caseVariable+"=1")
	for name, value := range environment {
		command.Env = append(command.Env, name+"="+value)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	status := 0
	if err := command.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("the window case did not exit through the transaction: %v (%s)",
				err, stderr.String())
		}
		status = exit.ExitCode()
	}
	return windowResult{status: status, stdout: stdout.String(), stderr: stderr.String()}
}

// requireBackups holds the retained backup set to the committed bytes: a retained
// directory whose numbered backups are not the originals is not a way back.
func requireBackups(t *testing.T, work string, created *fixture) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(work, "backup", ".complete")); err != nil {
		t.Fatalf("the retained backup is not marked complete: %v", err)
	}
	for index, original := range created.raw {
		backup := filepath.Join(work, "backup", strconv.Itoa(index))
		raw, err := os.ReadFile(backup)
		if err != nil {
			t.Fatalf("the retained backup %s is missing: %v", backup, err)
		}
		if !bytes.Equal(raw, original) {
			t.Fatalf("the retained backup %d is not the committed byte", index)
		}
	}
}

func requireRetained(t *testing.T, work, owner string) {
	t.Helper()
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		t.Fatalf("the work directory %s was not retained: %v", work, err)
	}
	if info, err := os.Stat(owner); err != nil || !info.IsDir() {
		t.Fatalf("the owner scratch %s was not retained: %v", owner, err)
	}
}

func requireDiscarded(t *testing.T, work, owner string) {
	t.Helper()
	for _, directory := range []string{work, owner} {
		if _, err := os.Stat(directory); !os.IsNotExist(err) {
			t.Fatalf("%s was retained where this window retains nothing: %v", directory, err)
		}
	}
}

// TestP1SignalMidCopyRestoresEveryDestinationAndKeepsTheBackups: a signal that lands
// after some copies and before the rest stops the copies, restores what landed, keeps
// the complete backup set and exits 128+N.
//
// The mutation this case holds down: retaining nothing on a restored signal (delete
// the retention in the mid-copy branch) leaves the sources restored and the way back
// gone, and this case names the missing directory.
func TestP1SignalMidCopyRestoresEveryDestinationAndKeepsTheBackups(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:  created.root,
		workVariable:  work,
		ownerVariable: owner,
		windowVar:     string(windowCopy),
		indexVariable: "1",
	})
	result.requireStatus(t, 130)
	result.require(t, "window copy 1")
	result.require(t, "publication interrupted; sources restored")
	result.require(t, "recovery material retained at "+work)
	result.require(t, "owner scratch retained at "+owner)
	result.require(t, "STOP: recover "+work+" together with "+owner)
	if changed := created.changed(); len(changed) > 0 {
		t.Fatalf("a mid-copy signal restored every destination, changed: %v", changed)
	}
	if remoded := created.remoded(); len(remoded) > 0 {
		t.Fatalf("a mid-copy signal restored every destination's mode, remoded: %v", remoded)
	}
	requireRetained(t, work, owner)
	requireBackups(t, work, created)
	if _, err := os.Stat(filepath.Join(work, "recovery.json")); !os.IsNotExist(err) {
		t.Fatalf("a complete restore writes no recovery record: %v", err)
	}
	result.requireNone(t, reportLine)
}

// TestP2SignalDuringBackupLeavesTheSourcesUnchangedAndRetainsNothing: R1 = A, the
// owner's ruling on #213. A signal while the backups are being taken writes no
// destination, so it restores nothing, retains nothing and exits 128+N. The partial
// backup set it leaves is not recovery material: it is a directory to delete.
//
// The mutation this case holds down: route this window into the restore-and-retain
// branch (the shell's opposite branch), and this case names the retained directory
// it must not find.
func TestP2SignalDuringBackupLeavesTheSourcesUnchangedAndRetainsNothing(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:  created.root,
		workVariable:  work,
		ownerVariable: owner,
		windowVar:     string(windowBackup),
		indexVariable: "0",
		signalVar:     "TERM",
	})
	result.requireStatus(t, 143)
	result.require(t, "window backup 0")
	result.require(t, "interrupted before publication; sources unchanged")
	result.requireNone(t, "recovery material retained")
	result.requireNone(t, "retained at")
	result.requireNone(t, "STOP: recover")
	result.requireNone(t, reportLine)
	if changed := created.changed(); len(changed) > 0 {
		t.Fatalf("a backup-phase signal wrote no destination, changed: %v", changed)
	}
	if remoded := created.remoded(); len(remoded) > 0 {
		t.Fatalf("a backup-phase signal changed no destination's mode, remoded: %v", remoded)
	}
	requireDiscarded(t, work, owner)
}

// TestP3SignalAfterAFailedCopyWithARefusedRestoreExitsThree: the copies did not all
// land, the restore of the one that did fails, and the signal arrives before the
// outcome exists. The status is 3, the complete backup set and the owner scratch are
// retained and printed, and the destination whose restore failed still carries the
// published byte.
//
// The mutation this case holds down: report a restore failure as a completed one
// (delete the incomplete branch), and this case sees 128+N with a source left
// published.
func TestP3SignalAfterAFailedCopyWithARefusedRestoreExitsThree(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:   created.root,
		workVariable:   work,
		ownerVariable:  owner,
		windowVar:      string(windowCopied),
		indexVariable:  "3",
		failVariable:   "Dockerfile",
		refuseVariable: "Dockerfile",
	})
	result.requireStatus(t, 3)
	result.require(t, "window copied 3")
	result.require(t, "interrupted and restore incomplete")
	result.require(t, "recovery material retained at "+work)
	result.require(t, "owner scratch retained at "+owner)
	result.require(t, "STOP: recover "+work+" together with "+owner)
	result.requireNone(t, reportLine)
	requireRetained(t, work, owner)
	requireBackups(t, work, created)
	// The two destinations the restore reached are the committed bytes again; the one
	// whose restore was refused still carries what the copy wrote.
	for _, index := range []int{0, 2} {
		raw, err := os.ReadFile(created.paths[index])
		if err != nil || !bytes.Equal(raw, created.raw[index]) {
			t.Fatalf("destination %d was not restored: %v", index, err)
		}
	}
	refused := "half-written\n"
	if raw, err := os.ReadFile(created.paths[1]); err != nil || string(raw) != refused {
		t.Fatalf("the destination whose restore was refused is %q, want %q: %v",
			raw, refused, err)
	}
	var record struct {
		Phase        string `json:"phase"`
		Error        string `json:"error"`
		Destinations []struct {
			Destination string `json:"destination"`
			Existed     bool   `json:"existed"`
			Backup      *string
		} `json:"destinations"`
	}
	raw, err := os.ReadFile(filepath.Join(work, "recovery.json"))
	if err != nil {
		t.Fatalf("an incomplete recovery retains its record: %v", err)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("the retained record is not JSON: %v", err)
	}
	if record.Phase != "publication" {
		t.Fatalf("the record names phase %q, want publication", record.Phase)
	}
	if record.Error == "" {
		t.Fatal("the record names no cause")
	}
	if len(record.Destinations) != len(created.paths) {
		t.Fatalf("the record lists %d destinations, want %d", len(record.Destinations),
			len(created.paths))
	}
}

// TestP4SignalAfterTheCopiesLeavesThePublicationAndExitsThree: every copy landed and
// the outcome was not taken, so the publication is left in place — undoing it would
// destroy bytes nothing is inconsistent with — and the caller is told 3 with the
// material retained.
//
// The mutation this case holds down: treat a signal after the copies as a mid-copy
// one (delete the `copied` distinction, which is what the old unlock gap between the
// last copy and `settled` made reachable), and this case finds its sources restored
// and its status 130.
func TestP4SignalAfterTheCopiesLeavesThePublicationAndExitsThree(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:  created.root,
		workVariable:  work,
		ownerVariable: owner,
		windowVar:     string(windowCopied),
		indexVariable: "3",
	})
	result.requireStatus(t, 3)
	result.require(t, "window copied 3")
	result.require(t, "publication result undecided when the signal arrived")
	result.require(t, "recovery material retained at "+work)
	result.require(t, "owner scratch retained at "+owner)
	result.requireNone(t, reportLine)
	if published := created.republished(); len(published) != len(created.paths) {
		t.Fatalf("the completed publication was left in place, republished: %v", published)
	}
	if remoded := created.remoded(); len(remoded) > 0 {
		t.Fatalf("the completed publication kept every mode, remoded: %v", remoded)
	}
	requireRetained(t, work, owner)
	requireBackups(t, work, created)
}

// TestP5SignalAfterTheOutcomeExitsThreeWithoutASuccessReport: the outcome exists and
// nothing has been reported. Neither the decided status nor the signal status may
// claim it, and the success line must not be printed: the caller gets 3 and the
// material.
//
// The mutation this case holds down: stop the signal watch before the report (or take
// the report before the pending-signal check), and this case finds the success line
// next to a status that says the outcome is unknown.
func TestP5SignalAfterTheOutcomeExitsThreeWithoutASuccessReport(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:  created.root,
		workVariable:  work,
		ownerVariable: owner,
		windowVar:     string(windowDecided),
		indexVariable: "0",
	})
	result.requireStatus(t, 3)
	result.require(t, "window decided 0")
	result.require(t, "publication result undecided when the signal arrived")
	result.require(t, "recovery material retained at "+work)
	result.require(t, "owner scratch retained at "+owner)
	if strings.Contains(result.stdout, reportLine) {
		t.Fatalf("an undecided publication printed the success line\n%s", result.stdout)
	}
	if published := created.republished(); len(published) != len(created.paths) {
		t.Fatalf("the decision left the publication in place, republished: %v", published)
	}
	if remoded := created.remoded(); len(remoded) > 0 {
		t.Fatalf("the decision left every mode alone, remoded: %v", remoded)
	}
	requireRetained(t, work, owner)
	requireBackups(t, work, created)
}

// TestP6ScratchRemovalFailureAfterSuccessStaysZero: #138. The publication is decided
// and reported; the caller's scratch will not go away. The status stays 0, the report
// is printed, the leftover is named once and it is never called recovery material,
// because nothing is left to recover.
//
// The mutation this case holds down: propagate the removal's error as the status, and
// this case sees a failed run that published.
func TestP6ScratchRemovalFailureAfterSuccessStaysZero(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:  created.root,
		workVariable:  work,
		ownerVariable: owner,
		cleanupVar:    "1",
	})
	result.requireStatus(t, 0)
	if !strings.Contains(result.stdout, "pin-artifact: "+reportLine) {
		t.Fatalf("a decided 0 printed its report\n%s", result.stdout)
	}
	if published := created.republished(); len(published) != len(created.paths) {
		t.Fatalf("the publication stands, republished: %v", published)
	}
	if remoded := created.remoded(); len(remoded) > 0 {
		t.Fatalf("a published destination keeps its mode, remoded: %v", remoded)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Fatalf("a decided 0 removes the work directory: %v", err)
	}
	if info, err := os.Stat(owner); err != nil || !info.IsDir() {
		t.Fatalf("the leftover is still there to be named: %v", err)
	}
	leftovers := []string{}
	for _, line := range strings.Split(result.stderr, "\n") {
		if strings.Contains(line, "cleanup failed") {
			leftovers = append(leftovers, line)
		}
	}
	if len(leftovers) != 1 {
		t.Fatalf("the leftover is named once, got %d lines:\n%s", len(leftovers), result.stderr)
	}
	if !strings.Contains(leftovers[0], owner) {
		t.Fatalf("the leftover line names %s: %s", owner, leftovers[0])
	}
	result.requireNone(t, "recovery")
	result.requireNone(t, "STOP: recover")
}
