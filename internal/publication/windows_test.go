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

// The windows of #213 item 3 (P1–P6) are a contract with the maintainer, and every one
// of them ends a process. Only a real process can be asked what status it produced, so
// each case re-executes this test binary and the child delivers its signal through the
// transaction's own window hook.
//
// The hook is an unexported field of the transaction, nil in every production run: it is
// not a flag, not an environment variable and not reachable from another package, so a
// shipped binary carries the nil check and nothing that can fire it. That is the whole
// seam, and TestNoExportedFieldReachesTheWindowSeam keeps it that way.
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
	// signalFromCopyVariable names a destination whose copy delivers the signal, and
	// signalFromRestoreVariable names one whose *restore* does, which is how a case
	// lands inside a copy and inside an ordinary rollback.
	signalFromCopyVariable    = "PUBLICATION_WINDOW_SIGNAL_FROM_COPY"
	signalFromRestoreVariable = "PUBLICATION_WINDOW_SIGNAL_FROM_RESTORE"
	// signalFromReportVariable makes the report writer deliver the signal, so the
	// window that exists while the success lines are being written has a case.
	signalFromReportVariable = "PUBLICATION_WINDOW_SIGNAL_FROM_REPORT"
	// Request mode builds a Request and calls Publish, which is where the resolved
	// root, the report's errors and a publication that never starts live.
	requestModeVariable = "PUBLICATION_WINDOW_REQUEST"
	filesVariable       = "PUBLICATION_WINDOW_FILES"
	tempRootVariable    = "PUBLICATION_WINDOW_TEMP_ROOT"
	failReportVariable  = "PUBLICATION_WINDOW_FAIL_REPORT"
)

// reportLine is the success line the child asks the transaction to print. A decided
// 0 must print it and every undecided window must not: they are two different
// owners of the same sentence otherwise.
const reportLine = "published every destination"

func TestMain(m *testing.M) {
	if os.Getenv(caseVariable) != "" {
		if os.Getenv(requestModeVariable) != "" {
			os.Exit(childRequest())
		}
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
	}
	// The same shape the parent's fixture publishes: one staged byte per destination,
	// named after it, so a case can read the staged bytes back from the tree.
	for _, relative := range []string{"images/artifacts/node.json", "images/Dockerfile",
		"images/artifacts/node-release-key.asc"} {
		transaction.Destinations = append(transaction.Destinations, filepath.Join(root, relative))
		transaction.Staged = append(transaction.Staged,
			[]byte("staged "+filepath.Base(relative)+"\n"))
	}
	if name := os.Getenv(failVariable); name != "" {
		once := false
		failOn(transaction, name, &once)
	}
	if name := os.Getenv(signalFromCopyVariable); name != "" {
		signalDuringCopy(transaction, name, "staged")
	}
	if name := os.Getenv(signalFromRestoreVariable); name != "" {
		signalDuringCopy(transaction, name, "backup")
	}
	if os.Getenv(signalFromReportVariable) != "" {
		// The channel is created by prepare, so the writer reads it from the
		// transaction when it is called rather than capturing it here.
		transaction.Report.To = signalWriter{transaction: transaction}
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

// childRequest runs one publication through Publish, the entry point the writers call,
// and returns the status the process ended with.
func childRequest() int {
	root := os.Getenv(rootVariable)
	request := Request{
		Prefix:     "pin-artifact: ",
		Root:       root,
		TempRoot:   os.Getenv(tempRootVariable),
		TempPrefix: "pin-artifact.",
		Stderr:     os.Stderr,
		Owner:      os.Getenv(ownerVariable),
		Report:     Report{To: os.Stdout, Lines: []string{reportLine}},
	}
	if os.Getenv(failReportVariable) != "" {
		request.Report.To = failingWriter{}
	}
	for _, name := range strings.Split(os.Getenv(filesVariable), ",") {
		request.Files = append(request.Files, Staged{Path: name,
			Raw: []byte("staged " + filepath.Base(name) + "\n")})
	}
	return Publish(request)
}

// signalDuringCopy delivers the signal from the transaction's own write, so the case
// lands inside a copy that is running (or failing), not between two of them.
func signalDuringCopy(transaction *Transaction, name, sourceKind string) {
	real := transaction.copyFile
	if real == nil {
		real = copyFile
	}
	once := false
	transaction.copyFile = func(source, destination string, mode os.FileMode) error {
		err := real(source, destination, mode)
		if !once && filepath.Base(destination) == name &&
			strings.Contains(source, sourceKind) {
			once = true
			deliver(transaction.Prefix, transaction.interrupted, "copy "+sourceKind)
		}
		return err
	}
}

// deliver sends the signal and waits for the handler to record it, which is what makes
// the publication's next window check see it without a sleep.
func deliver(prefix string, interrupted chan struct{}, where string) {
	fmt.Fprintf(os.Stderr, "%swindow %s\n", prefix, where)
	if err := syscall.Kill(os.Getpid(), namedSignal()); err != nil {
		fmt.Fprintf(os.Stderr, "cannot deliver the signal: %v\n", err)
		return
	}
	<-interrupted
}

// signalWriter is the report stream that writes the success line and then delivers the
// signal: the caller has been told before the signal arrives, which is exactly the
// window that must not exit 0.
type signalWriter struct{ transaction *Transaction }

func (w signalWriter) Write(raw []byte) (int, error) {
	written, err := os.Stdout.Write(raw)
	deliver(w.transaction.Prefix, w.transaction.interrupted, "report")
	return written, err
}

// failingWriter is a report stream that accepts nothing.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

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
		info, err := os.Stat(backup)
		if err != nil {
			t.Fatalf("the retained backup %s has no mode: %v", backup, err)
		}
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("the retained backup %d is %04o, want 0644", index, info.Mode().Perm())
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
	// Each entry is the mapping a maintainer reads by hand: what the relative
	// destination was, whether it existed, and which numbered backup puts it back.
	for index, entry := range record.Destinations {
		relative, err := filepath.Rel(created.root, created.paths[index])
		if err != nil {
			t.Fatal(err)
		}
		if entry.Destination != relative {
			t.Fatalf("record entry %d names %q, want %q", index, entry.Destination, relative)
		}
		if !entry.Existed {
			t.Fatalf("record entry %d says the committed destination did not exist", index)
		}
		if entry.Backup == nil ||
			*entry.Backup != filepath.Join(work, "backup", strconv.Itoa(index)) {
			t.Fatalf("record entry %d names backup %v", index, entry.Backup)
		}
	}
	for index, path := range created.paths {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("destination %d lost its mode: %v %v", index, info, err)
		}
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
	requireUndecidedRecord(t, work, created)
}

// requireUndecidedRecord holds the retained recovery record to the window it describes.
// Without it the retained directory holds backups and no statement of what happened,
// which is what the package header promises for the two undecided windows.
func requireUndecidedRecord(t *testing.T, work string, created *fixture) {
	t.Helper()
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
		t.Fatalf("an undecided outcome retains its record: %v", err)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("the retained record is not JSON: %v", err)
	}
	if record.Phase != "publication" {
		t.Fatalf("the record names phase %q, want publication", record.Phase)
	}
	if !strings.Contains(record.Error, "undecided") {
		t.Fatalf("the record does not say the outcome is undecided: %q", record.Error)
	}
	if len(record.Destinations) != len(created.paths) {
		t.Fatalf("the record lists %d destinations, want %d", len(record.Destinations),
			len(created.paths))
	}
	for index, entry := range record.Destinations {
		relative, err := filepath.Rel(created.root, created.paths[index])
		if err != nil {
			t.Fatal(err)
		}
		if entry.Destination != relative || !entry.Existed || entry.Backup == nil {
			t.Fatalf("record entry %d is %+v, want %q with its backup", index, entry, relative)
		}
	}
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
	requireUndecidedRecord(t, work, created)
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
	if count := strings.Count(result.stdout, "pin-artifact: "+reportLine); count != 1 {
		t.Fatalf("a decided 0 printed its report once, got %d:\n%s", count, result.stdout)
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

// TestSignalDuringTheSuccessReportKeepsTheOutcomeUndecided: the success line has been
// written and the process has not exited when the signal arrives. The caller has been
// told, and the outcome is still undecided, so the status must be 3 with the material
// and not a quiet 0 over directories that were about to be removed.
//
// The mutation this case holds down: delete the pending-signal check between the report
// and the disposal, and this case finds status 0 with both directories gone.
func TestSignalDuringTheSuccessReportKeepsTheOutcomeUndecided(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:             created.root,
		workVariable:             work,
		ownerVariable:            owner,
		signalFromReportVariable: "1",
	})
	result.requireStatus(t, 3)
	result.require(t, "window report")
	if !strings.Contains(result.stdout, "pin-artifact: "+reportLine) {
		t.Fatalf("the case did not reach the report it signals from:\n%s", result.stdout)
	}
	result.require(t, "publication result undecided when the signal arrived")
	result.require(t, "recovery material retained at "+work)
	result.require(t, "owner scratch retained at "+owner)
	if published := created.republished(); len(published) != len(created.paths) {
		t.Fatalf("the publication stands, republished: %v", published)
	}
	requireRetained(t, work, owner)
	requireBackups(t, work, created)
}

// TestSignalDuringAnOrdinaryRollbackKeepsTheMaterial: a copy failed, the rollback is
// running, and a signal arrives inside it. The rollback finishes, and then the window
// owns the outcome: 128+N with the sources restored and the material retained, not the
// status the rollback was about to report and not a work directory nobody can inspect.
//
// The mutation this case holds down: delete the pending-signal check after the rollback,
// and this case finds status 1 with the work directory already gone.
func TestSignalDuringAnOrdinaryRollbackKeepsTheMaterial(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:              created.root,
		workVariable:              work,
		ownerVariable:             owner,
		failVariable:              "Dockerfile",
		signalFromRestoreVariable: "node.json",
	})
	result.requireStatus(t, 130)
	result.require(t, "window copy backup")
	result.require(t, "publication interrupted; sources restored")
	result.require(t, "recovery material retained at "+work)
	result.require(t, "owner scratch retained at "+owner)
	result.requireNone(t, reportLine)
	if changed := created.changed(); len(changed) > 0 {
		t.Fatalf("the rollback restored every destination, changed: %v", changed)
	}
	requireRetained(t, work, owner)
	requireBackups(t, work, created)
}

// TestSignalDuringAFailingCopyRestores: the signal lands inside the copy that fails, so
// the two exit paths cross — the write returns an error and the window is a mid-copy
// one. Everything touched is restored, from the complete backup set, and the set is
// retained.
//
// The mutation this case holds down: drop the mid-copy restore branch (treat a signal
// with nothing published as the pre-publication window), and this case finds the half
// copy in place and nothing retained.
func TestSignalDuringAFailingCopyRestores(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:           created.root,
		workVariable:           work,
		ownerVariable:          owner,
		failVariable:           "Dockerfile",
		signalFromCopyVariable: "Dockerfile",
	})
	result.requireStatus(t, 130)
	result.require(t, "window copy staged")
	result.require(t, "publication interrupted; sources restored")
	result.require(t, "recovery material retained at "+work)
	if changed := created.changed(); len(changed) > 0 {
		t.Fatalf("a signal inside a failing copy restored every destination, changed: %v", changed)
	}
	requireRetained(t, work, owner)
	requireBackups(t, work, created)
}

// TestAnUnreadableReportStillPublishesAndSaysSo: the publication is decided and the
// caller's report stream accepts nothing. A quiet 0 would leave the caller believing a
// publication it was never told about did not happen, so the failure is named on stderr
// and the status stays the one the publication decided.
//
// The mutation this case holds down: swallow the report's error, and this case finds a
// status 0 with an empty stderr and no report.
func TestAnUnreadableReportStillPublishesAndSaysSo(t *testing.T) {
	created := newFixture(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:        created.root,
		workVariable:        created.work(t),
		ownerVariable:       owner,
		requestModeVariable: "1",
		filesVariable:       "images/artifacts/node.json",
		tempRootVariable:    created.root,
		failReportVariable:  "1",
	})
	result.requireStatus(t, 0)
	result.require(t, "the publication succeeded, but its report could not be written")
	if published := created.republished(); len(published) != 1 || published[0] != "node.json" {
		t.Fatalf("the publication landed, republished: %v", published)
	}
	if _, err := os.Stat(owner); !os.IsNotExist(err) {
		t.Fatalf("a decided 0 discarded the owner scratch: %v", err)
	}
}

// TestAPublicationThatCannotStartDisposesOfTheOwnerScratch: the work directory cannot
// be created, so nothing is published — and the scratch the caller handed over is still
// the publication's to remove, because the caller no longer cleans it up.
//
// The mutation this case holds down: return from that path without disposing, and this
// case finds the scratch left behind.
func TestAPublicationThatCannotStartDisposesOfTheOwnerScratch(t *testing.T) {
	created := newFixture(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:        created.root,
		ownerVariable:       owner,
		requestModeVariable: "1",
		filesVariable:       "images/Dockerfile",
		tempRootVariable:    filepath.Join(created.root, "absent"),
	})
	result.requireStatus(t, 1)
	result.require(t, "cannot create a work directory")
	if _, err := os.Stat(owner); !os.IsNotExist(err) {
		t.Fatalf("a publication that never started left the caller's scratch: %v", err)
	}
	if changed := created.changed(); len(changed) > 0 {
		t.Fatalf("a publication that never started wrote: %v", changed)
	}
}

// TestAnUnresolvableRootTouchesNothing: a root that is not a directory cannot be
// resolved, so nothing is touched and the caller is told.
func TestAnUnresolvableRootTouchesNothing(t *testing.T) {
	created := newFixture(t)
	owner := ownerScratch(t)
	result := runWindow(t, map[string]string{
		rootVariable:        filepath.Join(created.root, "absent"),
		ownerVariable:       owner,
		requestModeVariable: "1",
		filesVariable:       "images/Dockerfile",
		tempRootVariable:    created.root,
	})
	result.requireStatus(t, 1)
	result.require(t, "cannot resolve the repository root")
	if _, err := os.Stat(owner); !os.IsNotExist(err) {
		t.Fatalf("a refused root left the caller's scratch: %v", err)
	}
}

// TestARootReachedThroughASymlinkIsTheSameRepository: the root is resolved once, and the
// destinations joined to the spelling it was given are re-based onto the resolved one.
//
// Two mutations this case holds down: dropping the re-basing leaves every destination
// outside the resolved root, so the publication is refused as "outside the repository";
// dropping the resolution leaves the transaction comparing and recording paths under the
// alias, which the root it reports names.
func TestARootReachedThroughASymlinkIsTheSameRepository(t *testing.T) {
	created := newFixture(t)
	linked := filepath.Join(t.TempDir(), "repo")
	if err := os.Symlink(created.root, linked); err != nil {
		t.Fatalf("cannot link the repository: %v", err)
	}
	// The root as a maintainer might spell it, and the destinations joined to that same
	// spelling: the resolution has to carry both, or the publication is refused as
	// outside a repository it is inside.
	transaction := created.transaction(created.work(t))
	transaction.Root = linked
	for index, path := range created.paths {
		relative, err := filepath.Rel(created.root, path)
		if err != nil {
			t.Fatal(err)
		}
		transaction.Destinations[index] = filepath.Join(linked, relative)
	}
	if status := transaction.Run(""); status != 0 {
		t.Fatalf("a root reached through a symlink is the same repository: status %d", status)
	}
	resolved, err := filepath.EvalSymlinks(created.root)
	if err != nil {
		t.Fatal(err)
	}
	if transaction.Root != resolved {
		t.Fatalf("the transaction kept the root %s, want the resolved %s", transaction.Root,
			resolved)
	}
	if published := created.republished(); len(published) != len(created.paths) {
		t.Fatalf("the publication landed in the one repository, republished: %v", published)
	}
}

// TestADestinationNamedDotDotIsLocal: a file may be called `..x`, and a ".." prefix test
// refuses it as though it climbed out of the repository. What must be refused is a path
// that leaves the root.
//
// The mutation this case holds down: restore the prefix test, and this case is refused
// with "outside the repository".
func TestADestinationNamedDotDotIsLocal(t *testing.T) {
	created := newFixtureOf(t, []string{"..x"})
	result := runWindow(t, map[string]string{
		rootVariable:        created.root,
		requestModeVariable: "1",
		filesVariable:       "..x",
		tempRootVariable:    created.root,
	})
	result.requireStatus(t, 0)
	if published := created.republished(); len(published) != 1 || published[0] != "..x" {
		t.Fatalf("a local ..x destination was published, republished: %v", published)
	}
}

// TestADestinationOutsideTheRepositoryIsRefused: the one path a publication must never
// write, spelled the way a relative destination could spell it.
func TestADestinationOutsideTheRepositoryIsRefused(t *testing.T) {
	created := newFixture(t)
	outside := filepath.Join(filepath.Dir(created.root), "outside.json")
	result := runWindow(t, map[string]string{
		rootVariable:        created.root,
		requestModeVariable: "1",
		filesVariable:       "../outside.json",
		tempRootVariable:    created.root,
	})
	result.requireStatus(t, 1)
	result.require(t, "outside the repository")
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("a path outside the repository was written: %v", err)
	}
}
