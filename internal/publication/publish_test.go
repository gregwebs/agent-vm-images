package publication

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a throwaway repository root with three destinations, two of which
// exist: the transaction's own shape.
type fixture struct {
	t     *testing.T
	root  string
	paths []string
	raw   [][]byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	created := &fixture{t: t, root: root}
	for _, relative := range []string{"images/artifacts/node.json", "images/Dockerfile",
		"images/artifacts/node-release-key.asc"} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		before := []byte("committed " + relative + "\n")
		if err := os.WriteFile(path, before, 0o644); err != nil {
			t.Fatal(err)
		}
		created.paths = append(created.paths, path)
		created.raw = append(created.raw, before)
	}
	return created
}

// staged returns what a refresh would publish, rememberable so the assertions can
// name the committed bytes.
func (f *fixture) staged() [][]byte {
	return [][]byte{
		[]byte("staged node pin\n"),
		[]byte("staged dockerfile\n"),
		[]byte("staged key\n"),
	}
}

// transaction is one publication over the fixture. Its exit is the test's own: only
// a case that re-executes this binary has a process to end, and every other case
// asserts the status the transaction decided.
func (f *fixture) transaction(work string) *Transaction {
	return &Transaction{
		Prefix:       "pin-artifact: ",
		Root:         f.root,
		Work:         work,
		Stderr:       os.Stderr,
		Destinations: f.paths,
		Staged:       f.staged(),
		exit:         func(int) {},
	}
}

// unchanged reports the destinations whose committed bytes still match.
func (f *fixture) changed() []string {
	changed := []string{}
	for index, path := range f.paths {
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, f.raw[index]) {
			changed = append(changed, filepath.Base(path))
		}
	}
	return changed
}

// republished reports the destinations that carry a published byte.
func (f *fixture) republished() []string {
	published := []string{}
	for index, path := range f.paths {
		raw, err := os.ReadFile(path)
		if err == nil && bytes.Equal(raw, f.staged()[index]) {
			published = append(published, filepath.Base(path))
		}
	}
	return published
}

// remoded reports the destinations whose permission bits moved. A rollback that
// restored the bytes but not the mode is not a restore.
func (f *fixture) remoded() []string {
	remoded := []string{}
	for _, path := range f.paths {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o644 {
			remoded = append(remoded, filepath.Base(path))
		}
	}
	return remoded
}

func (f *fixture) work(t *testing.T) string {
	work, err := os.MkdirTemp(t.TempDir(), "pin-artifact.")
	if err != nil {
		t.Fatal(err)
	}
	return work
}

// ownerScratch is the caller's own scratch directory: a directory the writer staged
// the proposal into, which the publication disposes of.
func ownerScratch(t *testing.T) string {
	t.Helper()
	owner := filepath.Join(t.TempDir(), "pin-artifact.work.")
	if err := os.MkdirAll(filepath.Join(owner, "proposal"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner, "proposal", "node.json"),
		[]byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return owner
}

// failOn replaces the transaction's write so one destination fails after its own
// bytes landed — the case the rollback exists for.
func failOn(transaction *Transaction, name string, once *bool) {
	real := transaction.copyFile
	if real == nil {
		real = copyFile
	}
	transaction.copyFile = func(source, destination string, mode os.FileMode) error {
		if !*once && filepath.Base(destination) == name &&
			strings.Contains(source, "staged") {
			*once = true
			if err := os.WriteFile(destination, []byte("half-written\n"), 0o644); err != nil {
				return err
			}
			return os.ErrInvalid
		}
		return real(source, destination, mode)
	}
}

func TestPublicationFailureRestoresEveryTouchedDestination(t *testing.T) {
	created := newFixture(t)
	transaction := created.transaction(created.work(t))
	once := false
	failOn(transaction, "node-release-key.asc", &once)
	if status := transaction.Run(""); status != 1 {
		t.Fatalf("status %d, want 1", status)
	}
	if changed := created.changed(); len(changed) > 0 {
		t.Errorf("a failed publication restored every touched destination, changed: %v", changed)
	}
	if _, err := os.Stat(transaction.Work); !os.IsNotExist(err) {
		t.Errorf("a clean rollback removes the work directory")
	}
}

// TestPublicationFailureDisposesOfTheOwnerScratch: the package owns the caller's
// scratch, so a decided outcome removes it and the writer has nothing left to do.
func TestPublicationFailureDisposesOfTheOwnerScratch(t *testing.T) {
	created := newFixture(t)
	owner := ownerScratch(t)
	transaction := created.transaction(created.work(t))
	once := false
	failOn(transaction, "node-release-key.asc", &once)
	if status := transaction.Run(owner); status != 1 {
		t.Fatalf("status %d, want 1", status)
	}
	if _, err := os.Stat(owner); !os.IsNotExist(err) {
		t.Errorf("a decided outcome disposes of the owner scratch")
	}
}

func TestBackupFailureTouchesNothing(t *testing.T) {
	created := newFixture(t)
	// A destination that is a directory cannot be copied, so the backup phase
	// fails before publication.
	target := created.paths[2]
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	transaction := created.transaction(created.work(t))
	if status := transaction.Run(""); status != 1 {
		t.Fatalf("status %d, want 1", status)
	}
	if transaction.touched != 0 {
		t.Errorf("a failed backup touched %d destinations", transaction.touched)
	}
	changed := created.changed()
	if len(changed) != 1 || changed[0] != "node-release-key.asc" {
		t.Errorf("a failed backup left the other destinations identical, changed: %v", changed)
	}
}

func TestIncompleteRecoveryExitsThreeAndRetainsTheRecord(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	transaction := created.transaction(work)
	once := false
	failOn(transaction, "node-release-key.asc", &once)
	transaction.copyFile = refusingRestore(transaction.copyFile)
	if status := transaction.Run(""); status != 3 {
		t.Fatalf("status %d, want 3", status)
	}
	raw, err := os.ReadFile(filepath.Join(work, "recovery.json"))
	if err != nil {
		t.Fatalf("the incomplete recovery retains recovery.json: %v", err)
	}
	var record struct {
		Phase        string `json:"phase"`
		Destinations []struct {
			Destination string `json:"destination"`
			Existed     bool   `json:"existed"`
			Backup      *string
		} `json:"destinations"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Phase != "publication" {
		t.Errorf("the record names the phase, got %q", record.Phase)
	}
	if len(record.Destinations) != len(created.paths) {
		t.Errorf("the record lists every destination, got %d", len(record.Destinations))
	}
}

// refusingRestore makes every copy *from* the backup fail, which is what a damaged
// backup directory looks like to the restore.
func refusingRestore(copyFile func(source, destination string, mode os.FileMode) error) func(string, string, os.FileMode) error {
	return func(source, destination string, mode os.FileMode) error {
		if strings.Contains(source, string(filepath.Separator)+"backup"+string(filepath.Separator)) {
			return os.ErrClosed
		}
		return copyFile(source, destination, mode)
	}
}

// refusingRestoreOf refuses the restore of one destination only, which is what a
// damaged backup for that one destination looks like: every other source still comes
// back.
func refusingRestoreOf(copyFile func(source, destination string, mode os.FileMode) error,
	name string) func(string, string, os.FileMode) error {
	return func(source, destination string, mode os.FileMode) error {
		if strings.Contains(source, string(filepath.Separator)+"backup"+string(filepath.Separator)) &&
			filepath.Base(destination) == name {
			return os.ErrClosed
		}
		return copyFile(source, destination, mode)
	}
}

func TestRecoveryRecordFailureStillExitsThree(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	// recovery.json cannot be written: its path is a directory.
	if err := os.Mkdir(filepath.Join(work, "recovery.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	transaction := created.transaction(work)
	once := false
	failOn(transaction, "node-release-key.asc", &once)
	transaction.copyFile = refusingRestore(transaction.copyFile)
	if status := transaction.Run(""); status != 3 {
		t.Fatalf("status %d, want 3", status)
	}
	if _, err := os.Stat(filepath.Join(work, "backup", ".complete")); err != nil {
		t.Errorf("the retained backup is marked complete: %v", err)
	}
}

func TestPartialBackupIsNotRestored(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	transaction := created.transaction(work)
	if err := transaction.prepare(""); err != nil {
		t.Fatal(err)
	}
	if err := transaction.backUp(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(work, "backup", ".complete")); err != nil {
		t.Fatal(err)
	}
	transaction.touched = 1
	if err := transaction.restore(); err == nil {
		t.Fatal("a restore without a complete backup was accepted")
	}
}

func TestRollbackRemovesADestinationPublicationCreated(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	transaction := created.transaction(work)
	created_ := filepath.Join(created.root, "images/artifacts/created.json")
	transaction.Destinations = []string{created_}
	transaction.Staged = [][]byte{[]byte("published\n")}
	if err := transaction.prepare(""); err != nil {
		t.Fatal(err)
	}
	if err := transaction.stage(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.backUp(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.publishFile(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(created_); !os.IsNotExist(err) {
		t.Errorf("a rollback removes a destination publication created")
	}
}

// TestReplacementKeepsTheDestinationsMode: a published file that already exists
// keeps its permission bits, and only one the transaction creates gets the default.
//
// This is the difference between "a refresh replaced the bytes" and "a refresh
// widened the file": a committed 0600 destination that came back 0644 would be a
// change no reviewer of the diff could see.
func TestReplacementKeepsTheDestinationsMode(t *testing.T) {
	created := newFixture(t)
	if err := os.Chmod(created.paths[0], 0o600); err != nil {
		t.Fatal(err)
	}
	transaction := created.transaction(created.work(t))
	attribute := filepath.Join(created.root, "images/artifacts/created.json")
	transaction.Destinations = append(append([]string{}, created.paths...), attribute)
	transaction.Staged = append(append([][]byte{}, created.staged()...), []byte("a b c\n"))
	status := transaction.Run("")
	if status != 0 {
		t.Fatalf("publication: status %d", status)
	}
	info, err := os.Stat(created.paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("a replaced destination came back %04o, want 0600", mode)
	}
	info, err = os.Stat(attribute)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Errorf("a created destination is %04o, want 0644", mode)
	}
}
