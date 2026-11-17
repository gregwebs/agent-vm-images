package publication

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func (f *fixture) transaction(work string) *Transaction {
	transaction := &Transaction{
		Prefix:       "pin-artifact: ",
		Root:         f.root,
		Work:         work,
		Stderr:       os.Stderr,
		Destinations: f.paths,
		Staged:       f.staged(),
	}
	return transaction
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

func (f *fixture) work(t *testing.T) string {
	work, err := os.MkdirTemp(t.TempDir(), "pin-artifact.")
	if err != nil {
		t.Fatal(err)
	}
	return work
}

// failOn replaces the transaction's write so one destination fails after its own
// bytes landed — the case the rollback exists for.
func failOn(transaction *Transaction, name string, signal os.Signal, once *bool) {
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
			if signal != nil {
				if err := syscall.Kill(os.Getpid(), signal.(syscall.Signal)); err != nil {
					return err
				}
				// The interrupt and the failing write are two goroutines, and the
				// handler must record the signal before this one reports its error or
				// the run exits 1 as if nothing had interrupted it. A real Ctrl-C is
				// microseconds ahead of the write it lands in; a loaded CI runner
				// schedules the handler late, so the case waits for it explicitly.
				time.Sleep(200 * time.Millisecond)
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
	failOn(transaction, "node-release-key.asc", nil, &once)
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
	failOn(transaction, "node-release-key.asc", nil, &once)
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

func TestRecoveryRecordFailureStillExitsThree(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	// recovery.json cannot be written: its path is a directory.
	if err := os.Mkdir(filepath.Join(work, "recovery.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	transaction := created.transaction(work)
	once := false
	failOn(transaction, "node-release-key.asc", nil, &once)
	transaction.copyFile = refusingRestore(transaction.copyFile)
	var stderr bytes.Buffer
	transaction.Stderr = nil
	if status := transaction.Run(""); status != 3 {
		t.Fatalf("status %d, want 3", status)
	}
	if _, err := os.Stat(filepath.Join(work, "backup", ".complete")); err != nil {
		t.Errorf("the retained backup is marked complete: %v", err)
	}
	_ = stderr
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

// The signal path is a contract with the maintainer's shell: exit 128+N with the
// sources restored. It needs a real process to observe the status, so the case
// re-executes this test binary.
const signalCaseVariable = "ARTIFACTWRITER_SIGNAL_CASE"

func TestMain(m *testing.M) {
	if os.Getenv(signalCaseVariable) != "" {
		os.Exit(signalChild())
	}
	os.Exit(m.Run())
}

func signalChild() int {
	root := os.Getenv("ARTIFACTWRITER_SIGNAL_ROOT")
	work := os.Getenv("ARTIFACTWRITER_SIGNAL_WORK")
	refuseRestore := os.Getenv("ARTIFACTWRITER_SIGNAL_REFUSE_RESTORE") != ""
	transaction := &Transaction{
		Prefix: "pin-artifact: ",
		Root:   root,
		Work:   work,
		Stderr: os.Stderr,
		Destinations: []string{
			filepath.Join(root, "images/artifacts/node.json"),
			filepath.Join(root, "images/Dockerfile"),
			filepath.Join(root, "images/artifacts/node-release-key.asc"),
		},
		Staged: [][]byte{[]byte("a\n"), []byte("b\n"), []byte("c\n")},
	}
	once := false
	failOn(transaction, "Dockerfile", syscall.SIGINT, &once)
	if refuseRestore {
		// A restore that cannot read the backups is the case that must stay status 3
		// rather than report the interrupt: the maintainer needs the material, not a
		// status that says the tree is fine.
		publishing := transaction.copyFile
		transaction.copyFile = func(source, destination string, mode os.FileMode) error {
			if strings.Contains(source, "backup") {
				return os.ErrInvalid
			}
			return publishing(source, destination, mode)
		}
	}
	status := transaction.Run("")
	// The handler exits 130 or 3 itself; reaching here means it did not.
	return status
}

func TestSignalDuringPublicationRestoresAndExits130(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(),
		signalCaseVariable+"=1",
		"ARTIFACTWRITER_SIGNAL_ROOT="+created.root,
		"ARTIFACTWRITER_SIGNAL_WORK="+work)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("the signal case did not exit through the handler: %v (%s)", err, stderr.String())
	}
	if exit.ExitCode() != 128+int(syscall.SIGINT) {
		t.Fatalf("status %d, want %d: %s", exit.ExitCode(),
			128+int(syscall.SIGINT), stderr.String())
	}
	// Both orders of the two exit paths are legitimate — the handler may restore
	// before the failing copy reports, or the ordinary rollback may get there
	// first — and both leave the sources restored and exit 130.
	if !strings.Contains(stderr.String(), "restor") {
		t.Errorf("the signal case reports the restore: %s", stderr.String())
	}
	if changed := created.changed(); len(changed) > 0 {
		t.Errorf("a signal during publication restores the sources, changed: %v", changed)
	}
	if _, err := os.Stat(work); !os.IsNotExist(err) {
		t.Errorf("a restore that succeeded removes the work directory")
	}
}

// TestSignalWithARefusedRestoreExitsThree: the interrupt restores the sources and
// leaves 128+N, but only if the restore succeeds. When it does not, the status is 3
// and the material is printed, because "interrupted" plus "half restored" is not a
// state whose status may claim the tree is intact.
//
// The case is separate from the two it sits between (a signal that restores, and a
// failed write whose restore is refused) because it is the crossing of them, and it
// is the one an exit status can get wrong without any other case noticing.
func TestSignalWithARefusedRestoreExitsThree(t *testing.T) {
	created := newFixture(t)
	work := created.work(t)
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(),
		signalCaseVariable+"=1",
		"ARTIFACTWRITER_SIGNAL_REFUSE_RESTORE=1",
		"ARTIFACTWRITER_SIGNAL_ROOT="+created.root,
		"ARTIFACTWRITER_SIGNAL_WORK="+work)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err := command.Run()
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("the signal case did not exit through the handler: %v (%s)", err, stderr.String())
	}
	if exit.ExitCode() != 3 {
		t.Fatalf("status %d, want 3: %s", exit.ExitCode(), stderr.String())
	}
	for _, needle := range []string{"restore incomplete", "recovery material retained at", "STOP: recover"} {
		if !strings.Contains(stderr.String(), needle) {
			t.Errorf("the incomplete recovery does not say %q: %s", needle, stderr.String())
		}
	}
	if _, err := os.Stat(work); err != nil {
		t.Errorf("an incomplete recovery retains the work directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(work, "backup")); err != nil {
		t.Errorf("the retained material holds the backups: %v", err)
	}
}
