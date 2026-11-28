package publication

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

// Transaction is one publication over files that may not exist yet.
//
// Three explicit phases — staging, a *complete* backup, publication — because the
// order decides what a failure may undo. Staging and a failed backup touch no
// destination at all; only publication can, so only publication is restored, and
// only as far as it actually got (the destination being written counts). The
// `.complete` marker is what makes that safe: a restore never guesses from a
// half-written backup directory.
//
// The conventions are the shell publisher's: an invocation-owned backup directory,
// a numbered backup per destination, a `.complete` marker, restore in reverse
// order, and a retained work directory plus `recovery.json` when a restore fails so
// nothing is guessed afterwards.
type Transaction struct {
	// Prefix is the program's contract prefix on every diagnostic line. It is
	// frozen per writer: the suites read those lines to find retained recovery
	// material.
	Prefix string
	// Root is the repository the destinations are relative to.
	Root string
	// Work is this invocation's scratch directory, absolute before anything is
	// published: a retained work directory is printed during recovery, and a
	// relative path resolved from a different directory would point nowhere.
	Work string
	// Stderr receives the contract lines; a signal handler writes here too.
	Stderr *os.File
	// Report is what the caller wants printed once the outcome is decided, and
	// where. The transaction prints it, after it has taken the outcome and before
	// it ends the process, so a caller can never print "published" after an exit
	// status that says otherwise.
	Report Report
	// Retain names directories the caller must keep but did not create. They are
	// printed, and named in the STOP line, exactly when the outcome is retained —
	// never otherwise, and never removed.
	Retain []Retention

	Destinations []string
	Staged       [][]byte

	existed []bool
	phase   phase
	// touched counts destinations publication may have written. It is set before
	// each copy, so the file being written is included in a rollback.
	touched int
	// copied records that every copy returned. A signal that arrives after it is
	// no longer a mid-copy one: the publication is complete and undoing it would
	// destroy bytes nothing is inconsistent with.
	copied bool
	// copyFile and removeAll are the transaction's own writes and removals; a test
	// substitutes them to fail one destination or one removal once.
	copyFile  func(source, destination string, mode os.FileMode) error
	removeAll func(path string) error
	// hook is a test's seam at a named window: it delivers the signal the case
	// names, at the moment that case names. It is nil in every production run, and it
	// is an unexported field of a type whose window is unexported, so no caller and no
	// exported API can set it: only a test in this package can
	// (frozen by TestNoExportedFieldReachesTheWindowSeam).
	hook  func(at window, index int)
	owner string
	exit  func(status int)

	// mu is held from the first stage write to the process exit. It is what makes
	// the copies, the outcome and `settled` one critical section: a signal handler
	// that needs the transaction's state either finds the whole of it (and takes
	// the outcome itself) or is handed the outcome by the publication.
	mu      sync.Mutex
	settled bool
	// committed records that the outcome is decided and the exit is imminent. It is
	// what a handler that arrives during the removal reads: nothing is retained from
	// here on, so its answer is the decided status and not a retention claim.
	committed bool
	// decided is the status the outcome gave. The handler sets it before it ends
	// the process, so a publication waiting for that exit reports the same status.
	decided int
	// interrupted is closed once a signal has been accepted, and number records
	// which one. Recording happens before the handler takes the lock: that is how a
	// publication already inside the critical section learns a signal is pending.
	interrupted chan struct{}
	number      atomic.Int32
	exited      chan struct{}
}

// phase names where a publication had reached when a signal arrived.
type phase string

const (
	phaseStaging     phase = "staging"
	phaseBackup      phase = "backup"
	phasePublication phase = "publication"
)

// window names the moment a test delivers its signal. No clock-based seam can name
// these reliably, and each of them is a window #213 requires a proof for.
type window string

const (
	// windowBackup is after a numbered backup was written and before `.complete`.
	windowBackup window = "backup"
	// windowCopy is after one destination's bytes landed and before the next copy.
	windowCopy window = "copy"
	// windowCopied is after every copy returned and before the outcome is taken.
	windowCopied window = "copied"
	// windowDecided is after the outcome was taken and before it is reported.
	windowDecided window = "decided"
)

// fate is what the end of a publication does with this invocation's directories.
type fate int

const (
	// discard removes them: the outcome is decided and nothing is left to recover.
	discard fate = iota
	// retain keeps them and prints where they are, because the outcome is undecided
	// or the rollback is incomplete.
	retain
)

// errInterrupted stops the copy phases between two destinations when a signal has
// been recorded. The handler reports the outcome; this error only stops the work.
var errInterrupted = errors.New("publication interrupted")

func (t *Transaction) backupDir() string { return filepath.Join(t.Work, "backup") }
func (t *Transaction) stagedDir() string { return filepath.Join(t.Work, "staged") }

// signalled reports whether a signal has been accepted.
func (t *Transaction) signalled() bool {
	select {
	case <-t.interrupted:
		return true
	default:
		return false
	}
}

// prepare creates the scratch tree and records the rendered recovery lines.
func (t *Transaction) prepare(owner string) error {
	t.owner = owner
	t.phase = phaseStaging
	t.existed = nil
	if t.copyFile == nil {
		t.copyFile = copyFile
	}
	if t.removeAll == nil {
		t.removeAll = os.RemoveAll
	}
	if t.exit == nil {
		t.exit = os.Exit
	}
	if t.interrupted == nil {
		t.interrupted = make(chan struct{})
	}
	if t.exited == nil {
		t.exited = make(chan struct{})
	}
	for _, directory := range []string{t.backupDir(), t.stagedDir()} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// stage writes one destination's bytes into the scratch tree.
func (t *Transaction) stage() error {
	for index, destination := range t.Destinations {
		relative, err := relativeTo(t.Root, destination)
		if err != nil {
			return err
		}
		path := filepath.Join(t.stagedDir(), relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, t.Staged[index], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// backUp copies every destination that exists, and only then marks the backup
// complete: a partial backup directory is never trusted.
func (t *Transaction) backUp() error {
	t.phase = phaseBackup
	t.existed = make([]bool, len(t.Destinations))
	for index, destination := range t.Destinations {
		if t.signalled() {
			return errInterrupted
		}
		info, err := os.Lstat(destination)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		t.existed[index] = true
		temporary := filepath.Join(t.backupDir(), ".tmp"+strconv.Itoa(index))
		if err := t.copyFile(destination, temporary, info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Rename(temporary, filepath.Join(t.backupDir(), strconv.Itoa(index))); err != nil {
			return err
		}
		if t.hook != nil {
			t.hook(windowBackup, index)
		}
	}
	if t.signalled() {
		return errInterrupted
	}
	return os.WriteFile(filepath.Join(t.backupDir(), ".complete"), nil, 0o600)
}

// backupComplete reports whether a restore may trust the backup directory.
func (t *Transaction) backupComplete() bool {
	if t.existed == nil || len(t.existed) != len(t.Destinations) {
		return false
	}
	_, err := os.Stat(filepath.Join(t.backupDir(), ".complete"))
	return err == nil
}

// publish copies each staged file into place.
func (t *Transaction) publishFile() error {
	t.phase = phasePublication
	for index, destination := range t.Destinations {
		if t.signalled() {
			return errInterrupted
		}
		relative, err := relativeTo(t.Root, destination)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		t.touched = index + 1
		// Replacing a file is not a licence to widen it: an existing destination keeps
		// its own permission bits, and only a destination publication creates gets the
		// default. The publisher this replaced (Python's `shutil.copyfile`) left an
		// existing mode alone for the same reason, so a committed 0600 file stayed 0600
		// across a refresh.
		mode := os.FileMode(0o644)
		if info, err := os.Lstat(destination); err == nil && info.Mode().IsRegular() {
			mode = info.Mode().Perm()
		}
		if err := t.copyFile(filepath.Join(t.stagedDir(), relative), destination, mode); err != nil {
			return err
		}
		if t.hook != nil {
			t.hook(windowCopy, index)
		}
	}
	// A signal that lands while the last copy is still running is a mid-copy one,
	// not a completed publication: the copy that landed counts, so it is restored.
	if t.signalled() {
		return errInterrupted
	}
	t.copied = true
	return nil
}

// restore walks back over what publication could have touched.
//
// Nothing to do before publication, and nothing to trust without a complete
// backup: restoring from a partial backup directory would replace good bytes with
// half a file.
//
// A destination that will not come back does not stop the walk: one unreadable
// backup must not leave every other destination published, and the caller is told
// the first failure. That is what makes an incomplete rollback "incomplete" rather
// than "not attempted".
func (t *Transaction) restore() error {
	if t.touched == 0 {
		return nil
	}
	if !t.backupComplete() {
		return fmt.Errorf("the backup in %s is incomplete", t.backupDir())
	}
	var failure error
	for index := t.touched - 1; index >= 0; index-- {
		if err := t.restoreOne(index); err != nil && failure == nil {
			failure = err
		}
	}
	return failure
}

// restoreOne puts one destination back, or takes back out a destination publication
// created.
func (t *Transaction) restoreOne(index int) error {
	destination := t.Destinations[index]
	if !t.existed[index] {
		if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("%s: %w", destination, err)
		}
		return nil
	}
	backup := filepath.Join(t.backupDir(), strconv.Itoa(index))
	info, err := os.Stat(backup)
	if err != nil {
		return fmt.Errorf("%s: %w", destination, err)
	}
	if err := t.copyFile(backup, destination, info.Mode().Perm()); err != nil {
		return fmt.Errorf("%s: %w", destination, err)
	}
	return nil
}

// recoveryRecord describes the transaction so a human can finish it by hand.
func (t *Transaction) recoveryRecord(message string) error {
	destinations := make([]map[string]any, 0, len(t.Destinations))
	for index, destination := range t.Destinations {
		relative, err := relativeTo(t.Root, destination)
		if err != nil {
			return err
		}
		existed := false
		hasBackup := false
		if index < len(t.existed) {
			existed = t.existed[index]
			hasBackup = existed
		}
		entry := map[string]any{"destination": relative, "existed": existed}
		if hasBackup {
			entry["backup"] = filepath.Join(t.backupDir(), strconv.Itoa(index))
		} else {
			entry["backup"] = nil
		}
		destinations = append(destinations, entry)
	}
	record := map[string]any{
		"error":        message,
		"phase":        t.phase,
		"destinations": destinations,
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(t.Work, "recovery.json"), append(raw, '\n'), 0o600)
}

// recordUndecided writes the recovery record for a window that ends undecided, so the
// retained directory says what happened rather than leaving a maintainer to guess
// from the backups. Its failure is named but does not change the status: the retained
// directory and the printed paths are the way back.
func (t *Transaction) recordUndecided(message string) {
	if recordError, failed := t.recordRecoveryBestEffort(message); failed {
		t.diagnose("could not write the recovery record in %s (%s); the retained "+
			"directory and the backups in it are still authoritative", t.Work, recordError)
	}
}

// recordRecoveryBestEffort writes the record without letting its failure escape.
//
// A rollback that failed and then cannot even describe itself is still a status-3
// outcome: the retained `backup/` directory is the way back, and `recovery.json` is
// a convenience.
func (t *Transaction) recordRecoveryBestEffort(message string) (string, bool) {
	if err := t.recoveryRecord(message); err != nil {
		return err.Error(), true
	}
	return "", false
}

// Run publishes, disposes of the caller's own scratch directory, and ends the
// process.
//
// Run is the one owner of the outcome: it holds the transaction's lock from the
// first stage write to the exit, prints the caller's report once the outcome is
// decided, disposes of the caller's scratch, and ends the process. A signal either
// arrives before that — and then takes the outcome itself, with the publication
// waiting for it — or it arrives too late to be reported as anything but the
// outcome that was already taken.
func (t *Transaction) Run(owner string) int {
	if err := t.prepare(owner); err != nil {
		t.diagnose("%s failed (%s); no destination was touched, sources unchanged", t.phase, err)
		t.removeScratch()
		return 1
	}
	if err := t.resolveRoot(); err != nil {
		t.diagnose("cannot resolve the repository root (%s); sources unchanged", err)
		t.removeScratch()
		return 1
	}
	refused := refusedSymlinks(t.Root, t.Destinations)
	if len(refused) > 0 {
		t.diagnose("refusing a path reached through a symlink: [%s]", strings.Join(refused, " "))
		t.removeScratch()
		return 1
	}
	stop := t.watchSignals()
	// The watch is installed until the process ends, not until the publication has
	// taken the outcome: a signal between the decision and the exit must reach the
	// handler, which is why stopping it is the last thing that could run.
	defer stop()

	t.mu.Lock()
	err := t.stage()
	if err == nil {
		err = t.backUp()
	}
	if err == nil {
		err = t.publishFile()
	}
	if t.hook != nil {
		t.hook(windowCopied, len(t.Destinations))
	}
	if t.handedOver() {
		return t.decided
	}
	if err != nil {
		if t.phase != phasePublication {
			return t.failBeforePublication(err)
		}
		return t.recover(err)
	}
	t.settle(0)
	if t.hook != nil {
		t.hook(windowDecided, 0)
	}
	if t.handedOver() {
		return t.decided
	}
	if err := t.printReport(); err != nil {
		// The publication is decided and its bytes are in place; the caller was not
		// told, and a quiet status 0 would leave it believing it was.
		t.diagnose("the publication succeeded, but its report could not be written "+
			"(%s); the destinations are published", err)
	}
	// A signal that arrives while the report is being written owns the outcome: the
	// report may have reached the caller, but nothing has been disposed of yet, and
	// an undecided 3 with the material is the only status that does not claim an
	// outcome nobody reported.
	if t.handedOver() {
		return t.decided
	}
	return t.end(0, discard)
}

// resolveRoot canonicalises the repository root once, and re-bases the destinations the
// caller joined to the spelling it used.
//
// The resolution is what makes the paths this transaction compares, prints and records
// name the repository that was written rather than the alias the caller happened to use
// (a maintainer reading a retained directory should not have to work out which symlink it
// was published through). The re-basing is what makes that safe: the destinations were
// joined to the unresolved root, so without it every one of them is "outside the
// repository" on any host whose TMPDIR is a symlink — the ordinary case on macOS.
func (t *Transaction) resolveRoot() error {
	resolved, err := filepath.EvalSymlinks(t.Root)
	if err != nil {
		return err
	}
	for index, destination := range t.Destinations {
		relative, ok := localRelative(t.Root, destination)
		if !ok {
			continue
		}
		t.Destinations[index] = filepath.Join(resolved, relative)
	}
	t.Root = resolved
	return nil
}

// handedOver gives the outcome to a signal that has already been recorded and waits
// for the handler to end the process. It releases the lock so the handler reads the
// transaction exactly as the signal found it.
func (t *Transaction) handedOver() bool {
	if !t.signalled() {
		return false
	}
	t.mu.Unlock()
	<-t.exited
	return true
}

// settle takes the outcome. It is the last state change before the report and the
// exit, so a signal that arrives afterwards is undecided (status 3) rather than the
// outcome it arrived too late for.
func (t *Transaction) settle(status int) {
	t.settled = true
	t.decided = status
}

// failBeforePublication reports a failure that happened before the first copy:
// nothing was written, so there is nothing to restore and nothing to retain.
func (t *Transaction) failBeforePublication(cause error) int {
	t.diagnose("%s failed (%s); no destination was touched, sources unchanged", t.phase, cause)
	return t.end(1, discard)
}

// recover restores what publication touched and reports the outcome.
func (t *Transaction) recover(cause error) int {
	t.diagnose("publication failed (%s); restoring", cause)
	restoreErr := t.restore()
	if t.handedOver() {
		// A signal landed while the rollback was running. The rollback already put
		// back what it could, so the handler's window (not this status) is the one
		// the caller sees: it decides whether to restore again and what to retain.
		return t.decided
	}
	if err := restoreErr; err != nil {
		recordError, recordFailed := t.recordRecoveryBestEffort(err.Error())
		t.diagnose("restore incomplete (%s)", err)
		if recordFailed {
			t.diagnose("could not write the recovery record in %s (%s); the backups listed "+
				"below are still authoritative", t.Work, recordError)
		}
		t.retained()
		t.settle(3)
		return t.end(3, retain)
	}
	t.settle(1)
	return t.end(1, discard)
}

// end disposes of this invocation's directories and ends the process. It is the
// only exit path, so a status can never be produced without the report that
// describes it, nor a report without the status.
//
// A removal that fails keeps the status the outcome decided and names the leftover
// once (#138): a directory that would not go away does not turn a completed
// publication into a failure, and it is not recovery material, because nothing is
// left to recover.
func (t *Transaction) end(status int, f fate) int {
	t.decided = status
	t.committed = true
	// The lock is released before the removal, so a handler that arrives now is not
	// blocked behind it: the outcome is committed, and the handler's answer is the
	// decided status rather than a retention claim it could not honour.
	t.mu.Unlock()
	if f == discard {
		t.removeScratch()
	}
	// `exited` is closed only after the removal: the path waiting on it returns the
	// decided status, and a process that reported "discarded" must have discarded.
	close(t.exited)
	t.exit(status)
	// Only a test's substituted exit returns here.
	return status
}

// removeScratch removes the transaction's work directory and the caller's own
// scratch, naming each failure once.
func (t *Transaction) removeScratch() {
	if err := t.removeAll(t.Work); err != nil {
		t.diagnose("work directory cleanup failed (%s); %s left behind", err, t.Work)
	}
	if t.owner == "" {
		return
	}
	if err := t.removeAll(t.owner); err != nil {
		t.diagnose("owner scratch cleanup failed (%s); %s left behind", err, t.owner)
	}
}

// retained names the complete way back: this invocation's work directory with its
// backups and recovery record, and the caller's own scratch when it has one. Both
// are printed, because a maintainer needs the paths rather than a status.
func (t *Transaction) retained() {
	t.diagnose("recovery material retained at %s", t.Work)
	if t.owner != "" {
		t.diagnose("owner scratch retained at %s", t.owner)
	}
	// The caller's own directories are part of what a maintainer has to come back
	// to, so they are named here and in the STOP line: this is the only place that
	// knows the outcome was retained.
	for _, retention := range t.Retain {
		t.diagnose("%s retained at %s", retention.Label, retention.Path)
	}
	t.diagnose("STOP: recover %s%s before further edits/builds/commits", t.Work,
		togetherWith(t.owner, t.Retain))
}

// printReport writes the caller's success lines, which the caller cannot write
// itself: they describe an outcome only this transaction knows it took. It reports a
// failure rather than swallowing it: a lost report leaves a caller believing a
// publication it was never told about did not happen.
func (t *Transaction) printReport() error {
	if t.Report.To == nil {
		return nil
	}
	for _, line := range t.Report.Lines {
		if _, err := io.WriteString(t.Report.To, t.Prefix+line+"\n"); err != nil {
			return err
		}
	}
	// A caller that buffered the lines has not written them yet, and this process
	// ends next.
	if flusher, ok := t.Report.To.(interface{ Flush() error }); ok {
		return flusher.Flush()
	}
	return nil
}

// togetherWith names every other directory a retained outcome keeps, in the one
// sentence that tells a maintainer what to recover first.
func togetherWith(owner string, retentions []Retention) string {
	paths := []string{}
	if owner != "" {
		paths = append(paths, owner)
	}
	for _, retention := range retentions {
		paths = append(paths, retention.Path)
	}
	if len(paths) == 0 {
		return ""
	}
	return " together with " + strings.Join(paths, " and ")
}

// watchSignals records the signals a maintainer actually sends. The handler runs on
// a goroutine rather than in a signal handler so the contract lines it prints are
// ordinary writes.
func (t *Transaction) watchSignals() func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case received := <-signals:
			t.onSignal(received)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}

// onSignal records the signal and then takes the outcome, unless the
// publication is inside the critical section and takes it first (in which case that
// path hands the outcome back and waits for this one to end the process).
func (t *Transaction) onSignal(received os.Signal) {
	number, ok := received.(syscall.Signal)
	if !ok {
		number = syscall.SIGINT
	}
	if t.number.CompareAndSwap(0, int32(number)) {
		close(t.interrupted)
	}
	t.mu.Lock()
	if t.committed {
		// The outcome is decided and the exit is imminent; this invocation's
		// directories are already being removed. Nothing is retained to claim, and the
		// publication is the path that ends the process.
		t.mu.Unlock()
		return
	}
	status, f := t.signalOutcome(int(t.number.Load()))
	t.end(status, f)
}

// signalOutcome is the one place a signal decides a status, and therefore the one
// place the windows P1–P5 are told apart:
//
//   - undecided or incomplete keeps the material, writes the recovery record and
//     reports 3: the caller must inspect before continuing, and the publication stays
//     as the copies left it;
//   - mid-copy restores every destination it may have written and reports 128+N with
//     the complete backup set retained: the sources are as committed, and the
//     backups are still the way back;
//   - before any copy reports 128+N with nothing retained (R1 = A, ruled on #213):
//     no destination was written, so there is nothing to restore and a partial
//     backup set is not recovery material.
func (t *Transaction) signalOutcome(number int) (int, fate) {
	interrupt := 128 + number
	if t.settled || (t.phase == phasePublication && t.copied) {
		// The outcome is either already taken or the publication is complete. Neither
		// the decided status nor the signal status may claim what happened, and undoing
		// a complete publication would destroy bytes that are consistent with the
		// backups beside them.
		t.diagnose("publication result undecided when the signal arrived")
		t.recordUndecided("publication result undecided when the signal arrived")
		t.retained()
		return 3, retain
	}
	if t.phase == phasePublication && t.touched > 0 {
		if err := t.restore(); err != nil {
			recordError, recordFailed := t.recordRecoveryBestEffort(err.Error())
			t.diagnose("interrupted and restore incomplete (%s)", err)
			if recordFailed {
				t.diagnose("could not write the recovery record in %s (%s); the backups "+
					"listed below are still authoritative", t.Work, recordError)
			}
			t.retained()
			return 3, retain
		}
		t.diagnose("publication interrupted; sources restored")
		t.retained()
		return interrupt, retain
	}
	if t.phase == phasePublication {
		t.diagnose("interrupted before the first copy; sources unchanged")
	} else {
		t.diagnose("interrupted before publication; sources unchanged")
	}
	return interrupt, discard
}

// copyFile copies bytes and applies the source's permission bits, so a published
// file keeps the mode its staged copy has and a restored file keeps the mode the
// backup recorded.
func copyFile(source, destination string, mode os.FileMode) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(destination, raw, mode); err != nil {
		return err
	}
	return os.Chmod(destination, mode)
}

func (t *Transaction) diagnose(format string, args ...any) {
	if t.Stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(t.Stderr, t.Prefix+format+"\n", args...)
}

// relativeTo is the repository-relative name of one destination, refused when it is
// not a local path.
//
// filepath.IsLocal is the rule rather than a ".." prefix test: a committed file may
// legitimately be called `..x`, and a prefix test refuses it as though it climbed out
// of the repository. What must be refused is a relative path that leaves the root by
// any spelling — `..`, `../x`, an absolute path, or an empty one.
func relativeTo(root, path string) (string, error) {
	relative, ok := localRelative(root, path)
	if !ok {
		return "", fmt.Errorf("%s is outside the repository", path)
	}
	return relative, nil
}

// localRelative is path relative to root, reported only when it is a local path.
//
// One rule for both callers: the restore and the symlink walk must agree with the write
// about which paths are inside the repository, or a destination named `..x` is written
// by one and refused by the other.
func localRelative(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(relative) {
		return "", false
	}
	return relative, true
}

// refusedSymlinks reports every destination reached through a symlink.
//
// A destination reached through a symlink is a redirection, not a layout:
// rejecting only a symlinked *leaf* let the publisher change files through a
// symlinked parent directory, so the leaf and every ancestor between the root and
// it are checked.
func refusedSymlinks(root string, destinations []string) []string {
	seen := map[string]bool{}
	refused := []string{}
	for _, destination := range destinations {
		found := symlinked(root, destination)
		if found == "" || seen[found] {
			continue
		}
		seen[found] = true
		refused = append(refused, found)
	}
	sortStrings(refused)
	return refused
}

func symlinked(root, destination string) string {
	info, err := os.Lstat(destination)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return destination
	}
	relative, ok := localRelative(root, destination)
	if !ok {
		// Not a path under the root at all. The phase that writes refuses it, with the
		// reason it actually has; naming a symlink here would name the wrong one.
		return ""
	}
	parts := strings.Split(relative, string(filepath.Separator))
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return current
		}
		if !info.IsDir() {
			return current
		}
	}
	return ""
}

// SortedStrings orders values for a diagnostic, so a refusal names the same paths
// in the same order on every host.
func SortedStrings(values []string) []string {
	sorted := append([]string(nil), values...)
	sortStrings(sorted)
	return sorted
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for place := index; place > 0 && values[place] < values[place-1]; place-- {
			values[place], values[place-1] = values[place-1], values[place]
		}
	}
}
