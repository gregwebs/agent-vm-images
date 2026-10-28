package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/candidatestage"
)

// candidateFixture mints the driver's layout around a copy of the tiny dsh
// project: a 0700 pnpm-project-* scratch directly under the fixed system
// temporary directory with a 0700 candidate child holding exactly three inputs.
func candidateFixture(t *testing.T, source string) (string, int, string) {
	t.Helper()
	scratch, err := os.MkdirTemp(candidatestage.SystemTempDir(), "pnpm-project-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	fd, err := os.Open(scratch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fd.Close() })
	candidate := filepath.Join(scratch, "candidate")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"} {
		payload, err := os.ReadFile(filepath.Join(source, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(candidate, file), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return scratch, int(fd.Fd()), candidate
}

func writePins(t *testing.T, prefix string) {
	t.Helper()
	body := &strings.Builder{}
	for _, file := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"} {
		payload, err := os.ReadFile(filepath.Join(prefix, file))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		body.WriteString(hex.EncodeToString(sum[:]) + "  " + file + "\n")
	}
	if err := os.WriteFile(filepath.Join(prefix, "pnpm-inputs.sha256"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// piProject rewrites a dsh fixture copy into the exact pi consumer schema so the
// candidate and selection-update commands are exercised for a non-dsh kind.
func piProject(t *testing.T, source string) string {
	t.Helper()
	prefix := t.TempDir()
	for _, file := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"} {
		payload, err := os.ReadFile(filepath.Join(source, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, file), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	replace := func(file string) {
		path := filepath.Join(prefix, file)
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rewritten := strings.ReplaceAll(string(payload), "@deepseek-ai/dsh", "@earendil-works/pi-coding-agent")
		if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	replace("package.json")
	replace("pnpm-lock.yaml")
	lockPath := filepath.Join(prefix, "pnpm-lock.yaml")
	var lock map[string]any
	payload, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &lock); err != nil {
		t.Fatal(err)
	}
	delete(lock, "overrides")
	lock["settings"].(map[string]any)["autoInstallPeers"] = true
	encoded, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	workspace := `{"packages":["."],"autoInstallPeers":true,"strictPeerDependencies":false,` +
		`"enableGlobalVirtualStore":false,"pmOnFail":"error","sideEffectsCache":false,"allowBuilds":{},` +
		`"extendNodePath":false,"nodeLinker":"isolated","hoist":false,"preferSymlinkedExecutables":true,` +
		`"trustPolicyExclude":["undici-types@6.21.0"]}`
	if err := os.WriteFile(filepath.Join(prefix, "pnpm-workspace.yaml"), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	writePins(t, prefix)
	return prefix
}

func TestPnpmLockCandidateGraph(t *testing.T) {
	before := nodeFixture(t)
	scratch, fd, candidate := candidateFixture(t, before)
	scratchFD := strconv.Itoa(fd)
	got := runNode(t, nil, "pnpm-lock-candidate", "graph", "dsh",
		"--scratch", scratch, "--scratch-fd", scratchFD, "--before", before,
		"--requested-version", "0.1.5-rc.2")
	if got.status != 0 {
		t.Fatalf("candidate graph = %d %q", got.status, got.stderr)
	}
	if strings.Count(string(got.stdout), "\n") != 1 {
		t.Fatalf("candidate report is not one line: %q", got.stdout)
	}
	var report map[string]any
	if err := json.Unmarshal(got.stdout, &report); err != nil {
		t.Fatal(err)
	}
	if report["inputState"] != "candidate" {
		t.Errorf("inputState = %v", report["inputState"])
	}
	digests, ok := report["inputDigests"].(map[string]any)
	if !ok || len(digests) != 3 {
		t.Fatalf("inputDigests = %v", report["inputDigests"])
	}
	for _, file := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"} {
		payload, err := os.ReadFile(filepath.Join(candidate, file))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		if digests[file] != hex.EncodeToString(sum[:]) {
			t.Errorf("%s digest = %v", file, digests[file])
		}
	}
	delta, ok := report["selectionDelta"].(map[string]any)
	if !ok || delta["mode"] != "equal" {
		t.Fatalf("selectionDelta = %v", report["selectionDelta"])
	}
	graph, ok := report["graph"].(map[string]any)
	if !ok || graph["selection"].(map[string]any)["kind"] != "dsh" {
		t.Fatalf("graph = %v", report["graph"])
	}
	if _, ok := graph["packages"].([]any); !ok {
		t.Fatalf("graph has no packages: %v", graph)
	}
	// Ordinary pinned readers stay pinned: the candidate directory has no pin
	// file, so it can never be read back as reviewed input.
	if ordinary := runNode(t, nil, "pnpm-lock", "check", "dsh", scratch); ordinary.status != 1 {
		t.Errorf("candidate directory read as a project = %d %q", ordinary.status, ordinary.stderr)
	}
	if ordinary := runNode(t, nil, "pnpm-lock", "graph", "dsh", scratch); ordinary.status != 1 {
		t.Errorf("candidate directory graphed as a project = %d %q", ordinary.status, ordinary.stderr)
	}
}

func TestPnpmLockCandidateAdoptionGraph(t *testing.T) {
	before := nodeFixture(t)
	scratch, fd, _ := candidateFixture(t, before)
	got := runNode(t, nil, "pnpm-lock-candidate", "adoption-graph", "dsh",
		"--scratch", scratch, "--scratch-fd", itoa(fd))
	if got.status != 0 {
		t.Fatalf("adoption graph = %d %q", got.status, got.stderr)
	}
	var report map[string]any
	if err := json.Unmarshal(got.stdout, &report); err != nil {
		t.Fatal(err)
	}
	if _, present := report["selectionDelta"]; present {
		t.Errorf("adoption report carries a predecessor delta: %v", report)
	}
	if report["inputState"] != "candidate" {
		t.Errorf("inputState = %v", report["inputState"])
	}
}

func TestPnpmLockCandidatePiKind(t *testing.T) {
	project := piProject(t, "../../../../script/test/fixtures/pnpm-lock/tiny-dsh")
	scratch, fd, _ := candidateFixture(t, project)
	got := runNode(t, nil, "pnpm-lock-candidate", "graph", "pi",
		"--scratch", scratch, "--scratch-fd", itoa(fd), "--before", project,
		"--requested-version", "0.1.5-rc.2")
	if got.status != 0 {
		t.Fatalf("pi candidate = %d %q", got.status, got.stderr)
	}
	var report map[string]any
	if err := json.Unmarshal(got.stdout, &report); err != nil {
		t.Fatal(err)
	}
	if report["graph"].(map[string]any)["selection"].(map[string]any)["kind"] != "pi" {
		t.Errorf("graph kind = %v", report["graph"])
	}
	// The kind is a policy selector, never a relabel: the same dsh graph is not a
	// bridge candidate.
	dshScratch, dshFD, _ := candidateFixture(t, nodeFixture(t))
	if rejected := runNode(t, nil, "pnpm-lock-candidate", "adoption-graph", "bridge",
		"--scratch", dshScratch, "--scratch-fd", itoa(dshFD)); rejected.status != 1 {
		t.Errorf("dsh graph as bridge = %d %q", rejected.status, rejected.stderr)
	}
}

func TestPnpmLockCandidateUsage(t *testing.T) {
	before := nodeFixture(t)
	scratch, fd, _ := candidateFixture(t, before)
	fdText := itoa(fd)
	for _, args := range [][]string{
		{"pnpm-lock-candidate"},
		{"pnpm-lock-candidate", "graph"},
		{"pnpm-lock-candidate", "graph", "dsh"},
		{"pnpm-lock-candidate", "wat", "dsh", "--scratch", scratch, "--scratch-fd", fdText},
		{"pnpm-lock-candidate", "graph", "copilot", "--scratch", scratch, "--scratch-fd", fdText, "--before", before, "--requested-version", "0.1.5-rc.2"},
		{"pnpm-lock-candidate", "graph", "dsh", "--scratch", scratch, "--scratch-fd", fdText, "--requested-version", "0.1.5-rc.2"},
		{"pnpm-lock-candidate", "graph", "dsh", "--scratch", scratch, "--scratch-fd", fdText, "--before", before},
		{"pnpm-lock-candidate", "graph", "dsh", "--scratch", "", "--scratch-fd", fdText, "--before", before, "--requested-version", "0.1.5-rc.2"},
		{"pnpm-lock-candidate", "graph", "dsh", "--scratch", scratch, "--scratch-fd", "", "--before", before, "--requested-version", "0.1.5-rc.2"},
		{"pnpm-lock-candidate", "graph", "dsh", "--scratch", scratch, "--scratch-fd", fdText, "--before", before, "--requested-version", "0.1.5-rc.2", "--unknown", "x"},
		{"pnpm-lock-candidate", "graph", "dsh", "--scratch", scratch, "--scratch-fd", fdText, "--scratch-fd", fdText, "--before", before, "--requested-version", "0.1.5-rc.2"},
		{"pnpm-lock-candidate", "graph", "dsh", "--scratch", scratch, "--scratch-fd"},
		{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", "three"},
		{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", fdText, "--before", before},
		{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", fdText, "--requested-version", "0.1.5-rc.2"},
		{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", fdText, "--refresh-lock"},
	} {
		got := runNode(t, nil, args...)
		if got.status != 2 || !strings.HasPrefix(string(got.stderr), "usage: recipe-contract pnpm-lock-candidate ") {
			t.Errorf("%v = %d %q", args, got.status, got.stderr)
		}
		if len(got.stdout) != 0 {
			t.Errorf("%v leaked stdout", args)
		}
	}
}

func TestPnpmLockCandidateConfinement(t *testing.T) {
	before := nodeFixture(t)
	scratch, fd, candidate := candidateFixture(t, before)
	for _, vector := range []struct {
		name    string
		args    []string
		match   string
		prepare func()
		cleanup func()
	}{
		{"stdio-fd", []string{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", "1"}, "above 2", nil, nil},
		{"negative-fd", []string{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", "-1"}, "above 2", nil, nil},
		{"absent-fd", []string{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", "9999"}, "", nil, nil},
		{"wrong-scratch", []string{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", filepath.Join(candidatestage.SystemTempDir(), "not-a-stage"), "--scratch-fd", itoa(fd)}, "pnpm-project-", nil, nil},
		{"before-overlap", []string{"pnpm-lock-candidate", "graph", "dsh", "--scratch", scratch, "--scratch-fd", itoa(fd),
			"--before", scratch, "--requested-version", "0.1.5-rc.2"}, "overlap", nil, nil},
		{"before-absent", []string{"pnpm-lock-candidate", "graph", "dsh", "--scratch", scratch, "--scratch-fd", itoa(fd),
			"--before", filepath.Join(candidatestage.SystemTempDir(), "absent-prefix"), "--requested-version", "0.1.5-rc.2"}, "lstat", nil, nil},
		{"bad-version", []string{"pnpm-lock-candidate", "graph", "dsh", "--scratch", scratch, "--scratch-fd", itoa(fd), "--before", before, "--requested-version", "latest"}, "", nil, nil},
		{"extra-candidate-entry", []string{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", itoa(fd)},
			"unexpected candidate entry: package-lock.json",
			func() { _ = os.WriteFile(filepath.Join(candidate, "package-lock.json"), []byte("{}"), 0o644) },
			func() { _ = os.Remove(filepath.Join(candidate, "package-lock.json")) }},
		{"pin-in-candidate", []string{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", itoa(fd)},
			"unexpected candidate entry: pnpm-inputs.sha256",
			func() { writePins(t, candidate) },
			func() { _ = os.Remove(filepath.Join(candidate, "pnpm-inputs.sha256")) }},
	} {
		t.Run(vector.name, func(t *testing.T) {
			if vector.prepare != nil {
				vector.prepare()
			}
			if vector.cleanup != nil {
				defer vector.cleanup()
			}
			got := runNode(t, nil, vector.args...)
			if got.status != 1 {
				t.Fatalf("%s = %d %q", vector.name, got.status, got.stderr)
			}
			if !strings.HasPrefix(string(got.stderr), "pnpm-lock-candidate: ") {
				t.Errorf("%s stderr = %q", vector.name, got.stderr)
			}
			if vector.match != "" && !strings.Contains(string(got.stderr), vector.match) {
				t.Errorf("%s stderr = %q, want %q", vector.name, got.stderr, vector.match)
			}
			if len(got.stdout) != 0 {
				t.Errorf("%s leaked stdout", vector.name)
			}
		})
	}
}

func TestPnpmLockCandidateRejectsDangerousWorkspace(t *testing.T) {
	before := nodeFixture(t)
	scratch, fd, candidate := candidateFixture(t, before)
	path := filepath.Join(candidate, "pnpm-workspace.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(payload), `"hoist": true`, `"hoist": true, "trustLockfile": true`, 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runNode(t, nil, "pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", itoa(fd))
	if got.status != 1 || !strings.Contains(string(got.stderr), "unexpected key") {
		t.Fatalf("dangerous candidate workspace = %d %q", got.status, got.stderr)
	}
	// The same unsafe workspace re-pinned in an ordinary project fails the ordinary
	// reader too: the candidate path is not a weaker policy.
	ordinary := nodeFixture(t)
	workspacePath := filepath.Join(ordinary, "pnpm-workspace.yaml")
	ordinaryPayload, err := os.ReadFile(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspacePath, []byte(strings.Replace(string(ordinaryPayload), `"hoist": true`, `"hoist": true, "trustLockfile": true`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	writePins(t, ordinary)
	if rejected := runNode(t, nil, "pnpm-lock", "graph", "dsh", ordinary); rejected.status != 1 {
		t.Errorf("re-pinned unsafe ordinary project = %d %q", rejected.status, rejected.stderr)
	}
}

func TestPnpmLockCandidateInheritedDescriptor(t *testing.T) {
	before := nodeFixture(t)
	scratch, _, _ := candidateFixture(t, before)
	directory, err := os.Open(scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	result := runChildProcess(t, childSpec{
		args:       []string{"pnpm-lock-candidate", "adoption-graph", "dsh", "--scratch", scratch, "--scratch-fd", "3"},
		cwd:        t.TempDir(),
		env:        []string{"PATH=/usr/bin:/bin"},
		extraFiles: []*os.File{directory},
	})
	if result.status != 0 {
		t.Fatalf("inherited descriptor = %d %q", result.status, result.stderr)
	}
	if !strings.HasPrefix(string(result.stdout), `{"inputState":"candidate"`) {
		t.Fatalf("stdout = %q", result.stdout)
	}
}

func TestPnpmSelectionUpdateArgv(t *testing.T) {
	before := nodeFixture(t)
	piBefore := piProject(t, "../../../../script/test/fixtures/pnpm-lock/tiny-dsh")
	for _, vector := range []struct {
		name   string
		args   []string
		status int
		match  string
	}{
		{"equal-dsh", []string{"pnpm-selection-update", "dsh", before, before, "0.1.5-rc.2"}, 0, "pnpm selection update: equal 0.1.5-rc.2 -> 0.1.5-rc.2"},
		{"equal-omitted-version", []string{"pnpm-selection-update", "dsh", before, before}, 0, "equal"},
		{"equal-pi", []string{"pnpm-selection-update", "pi", piBefore, piBefore}, 0, "equal"},
		{"json", []string{"pnpm-selection-update", "dsh", before, before, "0.1.5-rc.2", "--json"}, 0, `{"mode":"equal"`},
		{"refresh", []string{"pnpm-selection-update", "dsh", before, before, "0.1.5-rc.2", "--refresh-lock"}, 0, "refresh"},
		{"bad-version", []string{"pnpm-selection-update", "dsh", before, before, "latest"}, 1, ""},
		{"wrong-kind", []string{"pnpm-selection-update", "pi", before, before, "0.1.5-rc.2"}, 1, ""},
	} {
		t.Run(vector.name, func(t *testing.T) {
			got := runNode(t, nil, vector.args...)
			if got.status != vector.status {
				t.Fatalf("%s = %d %q", vector.name, got.status, got.stderr)
			}
			if vector.status == 0 && !strings.Contains(string(got.stdout), vector.match) {
				t.Errorf("%s stdout = %q, want %q", vector.name, got.stdout, vector.match)
			}
			if vector.status == 1 && !strings.HasPrefix(string(got.stderr), "pnpm selection update: ") {
				t.Errorf("%s stderr = %q", vector.name, got.stderr)
			}
		})
	}
	for _, args := range [][]string{
		{"pnpm-selection-update"},
		{"pnpm-selection-update", "dsh"},
		{"pnpm-selection-update", "dsh", before},
		{"pnpm-selection-update", "dsh", before, before, "0.1.5-rc.2", "extra"},
		{"pnpm-selection-update", "copilot", before, before},
		{"pnpm-selection-update", "dsh", before, before, "--unknown"},
		{"pnpm-selection-update", "dsh", before, before, "--json", "--json"},
	} {
		got := runNode(t, nil, args...)
		if got.status != 2 || !strings.HasPrefix(string(got.stderr), "usage: recipe-contract pnpm-selection-update ") {
			t.Errorf("%v = %d %q", args, got.status, got.stderr)
		}
	}
	// A dangerous re-pinned workspace fails the post-pin comparator as well.
	unsafe := nodeFixture(t)
	workspacePath := filepath.Join(unsafe, "pnpm-workspace.yaml")
	payload, err := os.ReadFile(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspacePath, []byte(strings.Replace(string(payload), `"hoist": true`, `"hoist": true, "trustLockfile": true`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runNode(t, nil, "pnpm-selection-update", "dsh", unsafe, unsafe); got.status != 1 {
		t.Errorf("unsafe workspace = %d %q", got.status, got.stderr)
	}
}

func TestPnpmLockCandidateStdoutFailure(t *testing.T) {
	before := nodeFixture(t)
	scratch, fd, _ := candidateFixture(t, before)
	_ = fd
	status := Run([]string{"pnpm-lock-candidate", "adoption-graph", "dsh",
		"--scratch", scratch, "--scratch-fd", itoa(fd)}, Environment{},
		Streams{Out: failingWriter{}, Err: &recorder{}})
	if status != 1 {
		t.Fatalf("candidate stdout failure = %d", status)
	}
}

func itoa(value int) string { return strconv.Itoa(value) }
