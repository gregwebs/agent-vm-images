package publication

import (
	"encoding/json"
	"fmt"
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

	Destinations []string
	Staged       [][]byte

	existed []bool
	phase   string
	// touched counts destinations publication may have written. It is set before
	// each copy, so the file being written is included in a rollback.
	touched int
	// copyFile is the write the transaction makes; a test substitutes its own to
	// fail one destination once.
	copyFile func(source, destination string, mode os.FileMode) error
	owner    string
	exit     func(status int)

	// guard serializes the two ways out of a publication. A signal that arrives
	// while a failing write is being rolled back must not produce a second, other
	// status: whichever path acts first owns the outcome, and the other one waits
	// for the process to end.
	mu         sync.Mutex
	restored   bool
	incomplete bool
	settled    bool
	cancelled  atomic.Bool
	exited     chan struct{}
}

func (t *Transaction) backupDir() string { return filepath.Join(t.Work, "backup") }
func (t *Transaction) stagedDir() string { return filepath.Join(t.Work, "staged") }

// prepare creates the scratch tree and records the rendered recovery lines.
func (t *Transaction) prepare(owner string) error {
	t.owner = owner
	t.phase = "staging"
	t.existed = nil
	for _, directory := range []string{t.backupDir(), t.stagedDir()} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	if t.copyFile == nil {
		t.copyFile = copyFile
	}
	if t.exit == nil {
		t.exit = os.Exit
	}
	if t.exited == nil {
		t.exited = make(chan struct{})
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
	t.phase = "backup"
	t.existed = make([]bool, len(t.Destinations))
	for index, destination := range t.Destinations {
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
	t.phase = "publication"
	for index, destination := range t.Destinations {
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
	}
	return nil
}

// restore walks back over what publication could have touched.
//
// Nothing to do before publication, and nothing to trust without a complete
// backup: restoring from a partial backup directory would replace good bytes with
// half a file.
func (t *Transaction) restore() error {
	if t.touched == 0 {
		return nil
	}
	if !t.backupComplete() {
		return fmt.Errorf("the backup in %s is incomplete", t.backupDir())
	}
	for index := t.touched - 1; index >= 0; index-- {
		destination := t.Destinations[index]
		if !t.existed[index] {
			if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		backup := filepath.Join(t.backupDir(), strconv.Itoa(index))
		info, err := os.Stat(backup)
		if err != nil {
			return err
		}
		if err := t.copyFile(backup, destination, info.Mode().Perm()); err != nil {
			return err
		}
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

// Run publishes, reporting the caller's own scratch directory as well.
func (t *Transaction) Run(owner string) int {
	if err := t.prepare(owner); err != nil {
		t.diagnose("%s failed (%s); no destination was touched, sources unchanged", t.phase, err)
		return 1
	}
	refused := refusedSymlinks(t.Root, t.Destinations)
	if len(refused) > 0 {
		t.diagnose("refusing a path reached through a symlink: [%s]", strings.Join(refused, " "))
		_ = os.RemoveAll(t.Work)
		return 1
	}
	stop := t.watchSignals()
	defer stop()
	// The phases hold `mu` so a signal cannot restore a destination while a copy
	// into it is still running: the handler waits, then restores what publication
	// actually wrote.
	t.mu.Lock()
	err := t.stage()
	if err == nil {
		err = t.backUp()
	}
	if err == nil {
		err = t.publishFile()
	}
	t.mu.Unlock()
	if err != nil && t.phase != "publication" {
		_ = os.RemoveAll(t.Work)
		t.diagnose("%s failed (%s); no destination was touched, sources unchanged",
			t.phase, err)
		return 1
	}
	if err != nil {
		return t.recover(err)
	}
	// Settle and remove the work directory *under* the lock: a signal that arrives
	// from here on is too late to restore anything, and it must not find a settled
	// transaction whose backups are already gone.
	t.mu.Lock()
	t.settled = true
	_ = os.RemoveAll(t.Work)
	t.mu.Unlock()
	t.awaitSignal()
	return 0
}

// awaitSignal waits for an interrupt to finish its own unwind before the ordinary
// path removes the work directory or returns: the handler restores from that
// directory, so it has to be the one that decides the outcome after a signal.
func (t *Transaction) awaitSignal() {
	if !t.cancelled.Load() {
		return
	}
	t.signalExit()
}

// signalExit waits for the interrupt handler to end the process and never returns.
func (t *Transaction) signalExit() {
	<-t.exited
	// The handler is between closing the channel and calling exit; returning here
	// would let the ordinary path's status win the race.
	select {}
}

// recover restores what publication touched and reports the outcome.
func (t *Transaction) recover(cause error) int {
	t.mu.Lock()
	if t.cancelled.Load() {
		t.mu.Unlock()
		t.signalExit()
	}
	t.diagnose("publication failed (%s); restoring", cause)
	if err := t.restore(); err != nil {
		recordError, recordFailed := t.recordRecoveryBestEffort(err.Error())
		t.diagnose("restore incomplete (%s); recovery material retained at %s", err, t.Work)
		if recordFailed {
			t.diagnose("could not write the recovery record in %s (%s); the backups listed "+
				"below are still authoritative", t.Work, recordError)
		}
		if t.owner != "" {
			t.diagnose("owner scratch retained at %s", t.owner)
		}
		t.diagnose("STOP: recover %s%s before further edits/builds/commits", t.Work,
			ownerSuffix(t.owner))
		t.incomplete = true
		t.settled = true
		t.mu.Unlock()
		t.awaitSignal()
		return 3
	}
	t.restored = true
	t.settled = true
	_ = os.RemoveAll(t.Work)
	t.mu.Unlock()
	t.awaitSignal()
	return 1
}

func ownerSuffix(owner string) string {
	if owner == "" {
		return ""
	}
	return " together with " + owner
}

// watchSignals restores on the signals a maintainer actually sends, and exits
// 128+N once it has. The restore runs on a goroutine rather than in a handler, so
// the contract lines it prints are ordinary writes.
func (t *Transaction) watchSignals() func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	var once sync.Once
	done := make(chan struct{})
	go func() {
		select {
		case received := <-signals:
			once.Do(func() { t.interrupted(received) })
		case <-done:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}

// interrupted restores the sources and leaves through exit(128+N).
func (t *Transaction) interrupted(received os.Signal) {
	number, ok := received.(syscall.Signal)
	if !ok {
		number = syscall.SIGINT
	}
	t.cancelled.Store(true)
	t.mu.Lock()
	// A publication that already settled owns its outcome: an incomplete recovery
	// must stay status 3, and a completed one has nothing left to restore.
	if t.incomplete {
		t.mu.Unlock()
		t.leave(3)
		return
	}
	if t.restored || t.settled {
		t.mu.Unlock()
		t.leave(128 + int(number))
		return
	}
	if t.phase == "publication" && t.touched > 0 {
		if err := t.restore(); err != nil {
			recordError, recordFailed := t.recordRecoveryBestEffort(err.Error())
			t.diagnose("interrupted and restore incomplete")
			if recordFailed {
				t.diagnose("could not write the recovery record in %s (%s); the backups "+
					"listed below are still authoritative", t.Work, recordError)
			}
			if t.owner != "" {
				t.diagnose("owner scratch retained at %s", t.owner)
			}
			t.diagnose("recovery material retained at %s", t.Work)
			t.diagnose("STOP: recover both directories above before further " +
				"edits/builds/commits")
			t.restored = true
			t.incomplete = true
			t.settled = true
			t.mu.Unlock()
			t.leave(3)
			return
		}
		t.diagnose("interrupted; sources restored")
		t.restored = true
		t.settled = true
		_ = os.RemoveAll(t.Work)
		t.mu.Unlock()
		t.leave(128 + int(number))
		return
	}
	t.settled = true
	t.mu.Unlock()
	_ = os.RemoveAll(t.Work)
	t.diagnose("interrupted before publication; sources unchanged")
	t.leave(128 + int(number))
}

// leave releases the ordinary path, which is waiting for this outcome, and exits.
func (t *Transaction) leave(status int) {
	close(t.exited)
	t.exit(status)
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

func relativeTo(root, path string) (string, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(relative, "..") {
		return "", fmt.Errorf("%s is outside the repository", path)
	}
	return relative, nil
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
	relative, err := filepath.Rel(root, destination)
	if err != nil || strings.HasPrefix(relative, "..") {
		return root
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
