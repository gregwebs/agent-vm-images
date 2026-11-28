package publication

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
	return newFixtureOf(t, []string{"images/artifacts/node.json", "images/Dockerfile",
		"images/artifacts/node-release-key.asc"})
}

// newFixtureOf is the same shape at any width, so a matrix can name its own
// destinations.
func newFixtureOf(t *testing.T, relatives []string) *fixture {
	t.Helper()
	root := t.TempDir()
	created := &fixture{t: t, root: root}
	for _, relative := range relatives {
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
	staged := make([][]byte, 0, len(f.paths))
	for _, path := range f.paths {
		staged = append(staged, []byte("staged "+filepath.Base(path)+"\n"))
	}
	return staged
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

// TestARequestBecomesTheTransactionItDescribes: a writer builds a Request and nothing
// else, so every field it names must reach the transaction — a dropped Retain is a
// directory the caller is told to keep on some other path while this one forgets it,
// and a dropped Report is a caller that cannot be told an outcome happened.
//
// The mutation this case holds down: delete any field of that mapping, and this case
// names it.
func TestARequestBecomesTheTransactionItDescribes(t *testing.T) {
	report := &bytes.Buffer{}
	request := Request{
		Prefix:     "pin-artifact: ",
		Root:       "/repo",
		TempRoot:   "/tmp",
		TempPrefix: "pin-artifact.",
		Stderr:     os.Stderr,
		Report:     Report{To: report, Lines: []string{"published"}},
		Owner:      "/owner",
		Retain:     []Retention{{Label: "staged proposal", Path: "/stage"}},
		Files:      []Staged{{Path: "images/Dockerfile", Raw: []byte("ARG x=1\n")}},
	}
	transaction := request.transaction("/work")
	if transaction.Prefix != request.Prefix || transaction.Root != request.Root ||
		transaction.Work != "/work" || transaction.Stderr != request.Stderr {
		t.Fatalf("the transaction is not the one the request describes: %+v", transaction)
	}
	if transaction.Report.To != report || len(transaction.Report.Lines) != 1 ||
		transaction.Report.Lines[0] != "published" {
		t.Fatalf("the transaction lost the request's report: %+v", transaction.Report)
	}
	if len(transaction.Retain) != 1 || transaction.Retain[0].Label != "staged proposal" ||
		transaction.Retain[0].Path != "/stage" {
		t.Fatalf("the transaction lost the directories the caller must keep: %+v",
			transaction.Retain)
	}
	if len(transaction.Destinations) != 1 ||
		transaction.Destinations[0] != filepath.Join(request.Root, "images/Dockerfile") ||
		len(transaction.Staged) != 1 || string(transaction.Staged[0]) != "ARG x=1\n" {
		t.Fatalf("the transaction lost the request's files: %v", transaction.Destinations)
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

// TestInclusiveRollbackMatrix: the rollback covers the destination being written, and
// a destination it cannot put back must not stop the walk over the rest.
//
// 5 destinations × ({no restore failure} ∪ {a restore failure at each j ≤ i}) is 20
// cells. The floor is the count itself: a matrix that quietly ran fewer cells would
// compare nothing and still print ok.
func TestInclusiveRollbackMatrix(t *testing.T) {
	const width = 5
	relatives := make([]string, 0, width)
	for index := 0; index < width; index++ {
		relatives = append(relatives, fmt.Sprintf("images/artifacts/file%d.json", index))
	}
	executed := 0
	for copyAt := 0; copyAt < width; copyAt++ {
		for restoreAt := -1; restoreAt <= copyAt; restoreAt++ {
			copyAt, restoreAt := copyAt, restoreAt
			executed++
			t.Run(fmt.Sprintf("copy%d-restore%d", copyAt, restoreAt), func(t *testing.T) {
				created := newFixtureOf(t, relatives)
				work := created.work(t)
				transaction := created.transaction(work)
				once := false
				failOn(transaction, filepath.Base(created.paths[copyAt]), &once)
				if restoreAt >= 0 {
					transaction.copyFile = refusingRestoreOf(transaction.copyFile,
						filepath.Base(created.paths[restoreAt]))
				}
				want := 1
				if restoreAt >= 0 {
					want = 3
				}
				if status := transaction.Run(""); status != want {
					t.Fatalf("status %d, want %d", status, want)
				}
				staged := created.staged()
				for index, path := range created.paths {
					raw, err := os.ReadFile(path)
					if err != nil {
						t.Fatalf("destination %d: %v", index, err)
					}
					wished := created.raw[index]
					switch {
					case index > copyAt:
						// Never copied: the committed byte is the whole assertion.
					case index == restoreAt:
						// The restore was refused, so this destination keeps what the
						// publication left: half a copy for the failing one, the staged
						// bytes for one that had been written before it.
						if index == copyAt {
							wished = []byte("half-written\n")
						} else {
							wished = staged[index]
						}
					}
					if !bytes.Equal(raw, wished) {
						t.Fatalf("destination %d is %q, want %q", index, raw, wished)
					}
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0o644 {
						t.Fatalf("destination %d lost its mode: %v %v", index, info, err)
					}
				}
				_, err := os.Stat(work)
				if restoreAt >= 0 {
					if err != nil {
						t.Fatalf("an incomplete rollback retains its material: %v", err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("a complete rollback discards its work directory: %v", err)
				}
			})
		}
	}
	if executed != 20 {
		t.Fatalf("the matrix ran %d cells, want 20", executed)
	}
}

// TestNoExportedFieldReachesTheWindowSeam: the window hook, the copy and removal
// substitutions and the exit are how a test drives a window. They are unexported, so no
// caller and no other package can reach them; the exported field set is frozen here to
// keep it that way, because a new exported field is how an external caller would reach
// the same internals.
//
// The mutation this case holds down: promote the hook (or add any exported seam), and
// this case names the field.
func TestNoExportedFieldReachesTheWindowSeam(t *testing.T) {
	frozen := map[string][]string{
		"Transaction": {"Destinations", "Prefix", "Report", "Retain", "Root", "Staged",
			"Stderr", "Work"},
		"Request": {"Files", "Owner", "Prefix", "Report", "Retain", "Root", "Stderr",
			"TempPrefix", "TempRoot"},
	}
	types := map[string]any{"Transaction": Transaction{}, "Request": Request{}}
	for name, value := range types {
		got := exportedFields(reflect.TypeOf(value))
		want := frozen[name]
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s exports %v, want %v", name, got, want)
		}
	}
	// The seam itself must stay unexported: settable from this package, and only here.
	seam, ok := reflect.TypeOf(Transaction{}).FieldByName("hook")
	if !ok {
		t.Fatal("the window seam is gone; the six window cases cannot run")
	}
	if seam.PkgPath == "" {
		t.Fatal("the window seam is exported")
	}
}

// exportedFields reports a type's exported field names in declaration order.
func exportedFields(subject reflect.Type) []string {
	names := []string{}
	for index := 0; index < subject.NumField(); index++ {
		field := subject.Field(index)
		if field.PkgPath != "" {
			continue
		}
		names = append(names, field.Name)
	}
	sort.Strings(names)
	return names
}
