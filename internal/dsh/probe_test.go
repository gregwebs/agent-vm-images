package dsh

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The tests in this file exist to make each security-relevant rule fail when it is
// removed. A case that would still pass with a `time.Sleep` or with "the HTTP request
// succeeded" in place of the rule proves nothing, so the fixture is the kernel shape
// the rule reads: one LISTEN row, one process in the group, one descriptor.

// procTree builds a synthetic procfs root. `listeners` maps a port to the socket
// inode a LISTEN row names; `members` maps a pid to the socket inodes its
// descriptors hold.
type procTree struct {
	root      string
	port      int
	inodes    map[int]string
	members   map[int][]string
	noNet     bool
	noSelfFD  bool
	statGroup string
}

func newProcTree(t *testing.T, tree procTree) prober {
	t.Helper()
	root := t.TempDir()
	tree.root = root
	if !tree.noSelfFD {
		mustMkdir(t, filepath.Join(root, "self", "fd"))
	}
	if !tree.noNet {
		mustMkdir(t, filepath.Join(root, "net"))
		rows := []string{"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode"}
		for port, inode := range tree.inodes {
			rows = append(rows, fmt.Sprintf(
				"   0: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000    99        0 %s 1",
				port, inode))
		}
		mustWrite(t, filepath.Join(root, "net", "tcp"), strings.Join(rows, "\n")+"\n")
		mustWrite(t, filepath.Join(root, "net", "tcp6"), rows[0]+"\n")
	}
	for pid, held := range tree.members {
		directory := filepath.Join(root, fmt.Sprintf("%d", pid), "fd")
		mustMkdir(t, directory)
		for index, inode := range held {
			if err := os.Symlink("socket:["+inode+"]", filepath.Join(directory, fmt.Sprintf("%d", index+3))); err != nil {
				t.Fatal(err)
			}
		}
		mustWrite(t, filepath.Join(root, fmt.Sprintf("%d", pid), "stat"),
			fmt.Sprintf("%d (dsh) S 1 %s 0 -1 4194560 0 0 0 0 0 0 0 0 20 0 1 0 100 0 0\n",
				pid, tree.statGroup))
	}
	return prober{host: "linux", proc: root}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOwnershipProvesTheListeningDescriptor pins the whole proof: the LISTEN inode for
// the reserved port must appear in a descriptor held by a member of the child's own
// process group.
func TestOwnershipProvesTheListeningDescriptor(t *testing.T) {
	probe := newProcTree(t, procTree{
		port:      4321,
		inodes:    map[int]string{4321: "111"},
		members:   map[int][]string{900: {"222", "111"}},
		statGroup: "900",
	})
	proof, err := probe.ownership(4321, 900, false)
	if err != nil {
		t.Fatalf("ownership = %v", err)
	}
	if !proof.Proven || proof.PID != 900 {
		t.Fatalf("proof = %+v", proof)
	}
}

// TestOwnershipRefusesAnotherGroupsListener is the case the token handshake cannot
// cover: something is listening on the reserved port, holds a valid descriptor, and is
// not ours.
func TestOwnershipRefusesAnotherGroupsListener(t *testing.T) {
	probe := newProcTree(t, procTree{
		port:      4321,
		inodes:    map[int]string{4321: "111"},
		members:   map[int][]string{900: {"222"}},
		statGroup: "900",
	})
	_, err := probe.ownership(4321, 900, false)
	if err == nil || !strings.Contains(err.Error(), "is not owned by the probed process group") {
		t.Fatalf("ownership = %v", err)
	}
}

// TestOwnershipRefusesAMissingListener keeps "no listener" distinct from "not ours":
// a port with no LISTEN row is never a proof of anything.
func TestOwnershipRefusesAMissingListener(t *testing.T) {
	probe := newProcTree(t, procTree{
		port:      4321,
		inodes:    map[int]string{9999: "111"},
		members:   map[int][]string{900: {"111"}},
		statGroup: "900",
	})
	_, err := probe.ownership(4321, 900, false)
	if err == nil || !strings.Contains(err.Error(), "no listener owns the reserved port 4321") {
		t.Fatalf("ownership = %v", err)
	}
}

// TestOwnershipUsesTheGroupNotTheProcess keeps the scan honest: a descriptor held by a
// process outside the group is not evidence about the group.
func TestOwnershipUsesTheGroupNotTheProcess(t *testing.T) {
	probe := newProcTree(t, procTree{
		port:      4321,
		inodes:    map[int]string{4321: "111"},
		members:   map[int][]string{901: {"111"}},
		statGroup: "777",
	})
	_, err := probe.ownership(4321, 900, false)
	if err == nil || !strings.Contains(err.Error(), "is not owned by the probed process group") {
		t.Fatalf("ownership = %v", err)
	}
}

// TestOwnershipFailsClosedOnLinuxAndInProduction is the degradation rule: a host that
// cannot read procfs has no proof, so a Linux run or a production capability fails
// rather than recording a skip.
func TestOwnershipFailsClosedOnLinuxAndInProduction(t *testing.T) {
	for _, unproven := range []struct {
		label      string
		production bool
		host       string
	}{
		{"linux host without procfs", false, "linux"},
		{"production capability on a host without procfs", true, "darwin"},
	} {
		probe := newProcTree(t, procTree{noNet: true, noSelfFD: true})
		probe.host = unproven.host
		_, err := probe.ownership(4321, 900, unproven.production)
		if err == nil || !strings.Contains(err.Error(), "listener ownership cannot be proven") {
			t.Fatalf("%s: ownership = %v", unproven.label, err)
		}
	}
}

// TestOwnershipFailsClosedOnUnreadableTables separates "no procfs at all" from "procfs
// without the socket tables": the second is a kernel this program cannot read.
func TestOwnershipFailsClosedOnUnreadableTables(t *testing.T) {
	probe := newProcTree(t, procTree{noNet: true})
	_, err := probe.ownership(4321, 900, true)
	if err == nil || !strings.Contains(err.Error(), "kernel socket tables unreadable: listener ownership cannot be proven") {
		t.Fatalf("ownership = %v", err)
	}
}

// TestOwnershipRecordsAnExplicitFixtureSkip is the only path that may pass without a
// proof, and it must say so in the report.
func TestOwnershipRecordsAnExplicitFixtureSkip(t *testing.T) {
	probe := newProberWithoutTables(t)
	proof, err := probe.ownership(4321, 900, false)
	if err != nil {
		t.Fatalf("ownership = %v", err)
	}
	if proof.Proven || !strings.Contains(proof.Reason, "fixture host without procfs") {
		t.Fatalf("proof = %+v", proof)
	}
}

func newProberWithoutTables(t *testing.T) prober {
	t.Helper()
	root := t.TempDir()
	mustMkdir(t, filepath.Join(root, "net"))
	mustWrite(t, filepath.Join(root, "net", "tcp"), "  sl  local_address\n")
	mustWrite(t, filepath.Join(root, "net", "tcp6"), "  sl  local_address\n")
	return prober{host: "darwin", proc: root}
}

// TestTeardownEscalatesAndProvesNoSurvivors is the teardown contract: a child that
// ignores TERM must be killed, its whole session must be empty, and the leader must be
// reaped. A `time.Sleep` in place of this leaves the group alive and the port held.
func TestTeardownEscalatesAndProvesNoSurvivors(t *testing.T) {
	child, descendant := startIgnoringChild(t)
	started := time.Now()
	if err := child.teardown(300 * time.Millisecond); err != nil {
		t.Fatalf("teardown = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("teardown did not bound itself: %s", elapsed)
	}
	if groupAlive(child.pgid) {
		t.Fatalf("the process group %d survived teardown", child.pgid)
	}
	if _, ok := child.doneStatus(); !ok {
		t.Fatal("the leader was not reaped")
	}
	if strings.Join(child.cleanupFacts.Signalled, ",") != "SIGTERM,SIGKILL" {
		t.Fatalf("signalled = %v", child.cleanupFacts.Signalled)
	}
	if stillAlive(descendant) {
		t.Fatalf("the same-session descendant %d survived the teardown", descendant)
	}
}

// TestTeardownReachesTheDescendant is the second half of the same rule: killing only
// the leader is not emptying the group, and the descendant is what keeps the port.
func TestTeardownReachesTheDescendant(t *testing.T) {
	child, descendant := startIgnoringChild(t)
	if err := child.teardown(200 * time.Millisecond); err != nil {
		t.Fatalf("teardown = %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for stillAlive(descendant) {
		if time.Now().After(deadline) {
			t.Fatalf("the descendant %d outlived the group teardown", descendant)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startIgnoringChild starts one session whose leader and descendant both ignore TERM,
// so only an escalating teardown can end it. The descendant's own pid is recorded, not
// read from procfs: this case must run on the host that has none.
func startIgnoringChild(t *testing.T) (*webChild, int) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "descendant")
	script := `trap "" TERM; (trap "" TERM; sleep 60) & printf '%d' "$!" > "$1"; sleep 60`
	command := exec.Command("sh", "-c", script, "dsh", record)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	child := &webChild{command: command, pgid: command.Process.Pid, done: make(chan int, 1)}
	go func() { child.done <- replayStatus(command.Wait()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		source, err := os.ReadFile(record)
		if err == nil && len(source) > 0 {
			descendant, convErr := strconv.Atoi(string(source))
			if convErr != nil {
				t.Fatal(convErr)
			}
			return child, descendant
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("the descendant never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stillAlive is `kill(pid, 0)` for a process this invocation did not parent.
func stillAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// doneStatus is a non-blocking read of the reaped status.
func (c *webChild) doneStatus() (int, bool) {
	polled := c.poll()
	return polled.status, polled.exited
}

// TestCaptureOverflowIsAHardFailure pins the capture bound: the gate must fail rather
// than silently truncate the child's own diagnostics.
func TestCaptureOverflowIsAHardFailure(t *testing.T) {
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	capture := newCapture(stdoutRead, stderrRead)
	payload := strings.Repeat("a", maxCapture+4096)
	if _, err := stdoutWrite.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	if err := capture.drain(); err != nil {
		t.Fatalf("drain = %v", err)
	}
	if !capture.overflowed() {
		t.Fatal("capture did not record the overflow")
	}
	buffered := len(capture.text("stdout")) + len(capture.text("stderr"))
	if buffered > maxCapture {
		t.Fatalf("capture kept %d bytes, bound is %d", buffered, maxCapture)
	}
}

// TestParseWebURLRefusesAnAdvertisedPortMismatch keeps the token handshake tied to the
// port this invocation reserved: a server that moved the port is a different server.
func TestParseWebURLRefusesAnAdvertisedPortMismatch(t *testing.T) {
	token := strings.Repeat("a", 43)
	advertised := "dsh web: http://127.0.0.1:5000/?token=" + token + "\n"
	if _, err := parseWebURL(advertised, 5001); err == nil ||
		!strings.Contains(err.Error(), "web advertised port 5000, but this invocation reserved 5001") {
		t.Fatalf("parse = %v", err)
	}
	found, err := parseWebURL(advertised, 5000)
	if err != nil || !strings.HasSuffix(found, "?token="+token) {
		t.Fatalf("parse = %q, %v", found, err)
	}
	// A line still being written has no trailing newline yet, so it is not compared.
	if partial, err := parseWebURL("dsh web: http://127.0.0.1:5000/?token="+token, 5000); err != nil || partial != "" {
		t.Fatalf("partial = %q, %v", partial, err)
	}
	if _, err := parseWebURL("dsh web: http://evil.invalid/?token="+token+"\n", 5000); err == nil ||
		!strings.Contains(err.Error(), "unexpected web url line") {
		t.Fatalf("foreign line = %v", err)
	}
}

// TestFrontendRefusals pins the handshake refusals. A frontend is accepted only when
// the token request, at most one same-origin redirect and every reviewed marker agree.
func TestFrontendRefusals(t *testing.T) {
	markers := []byte("<!doctype html><html><head><title>DeepSeek Harness</title></head>" +
		"<body><script>window.__DSH_BOOT__ = {}</script></body></html>")
	page := &httpResponse{Status: 200, Headers: map[string][]string{
		"Content-Type": {"text/html"},
	}, Body: markers}
	facts, err := frontend(page, 5000)
	if err != nil || facts.Status != 200 || facts.Redirects != 0 {
		t.Fatalf("frontend = %+v, %v", facts, err)
	}
	cases := []struct {
		name     string
		response *httpResponse
		want     string
	}{
		{"wrong status", &httpResponse{Status: 500}, "token request returned HTTP 500"},
		{"redirect without location", &httpResponse{Status: 302}, "redirected without a Location header"},
		{"wrong body", &httpResponse{Status: 200, Body: []byte("<!doctype html><body>other</body>")},
			"not the shipped dsh frontend (missing __dsh_boot__, <title>deepseek harness</title>)"},
		{"oversized", &httpResponse{Status: 200, Body: append(make([]byte, maxResponse+1), markers...)},
			fmt.Sprintf("frontend response exceeded %d bytes", maxResponse)},
	}
	for _, testCase := range cases {
		_, err := frontend(testCase.response, 5000)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: frontend = %v", testCase.name, err)
		}
	}
	// A redirect may not leave the probed origin: another port is another server.
	_, err = frontend(&httpResponse{Status: 302, Headers: map[string][]string{
		"Location": {"http://127.0.0.1:5999/?token=x"},
	}}, 5000)
	if err == nil || !strings.Contains(err.Error(), "left the probed origin") {
		t.Fatalf("cross-origin redirect = %v", err)
	}
	// A cross-scheme redirect is the same refusal, and no request is made for it.
	_, err = frontend(&httpResponse{Status: 303, Headers: map[string][]string{
		"Location": {"https://127.0.0.1:5000/?token=x"},
	}}, 5000)
	if err == nil || !strings.Contains(err.Error(), "left the probed origin") {
		t.Fatalf("cross-scheme redirect = %v", err)
	}
}

// TestCookieHeaderKeepsTheJar keeps the redirect's session: without the cookies the
// frontend sets, the second request is a new client and the probe could accept a page
// it never authenticated.
func TestCookieHeaderKeepsTheJar(t *testing.T) {
	response := &httpResponse{Status: 200, Headers: map[string][]string{
		"Set-Cookie": {"session=abc; Path=/; HttpOnly", "theme=dark", "session=def"},
	}}
	if got := response.cookieHeader(); got != "session=def; theme=dark" {
		t.Fatalf("cookie header = %q", got)
	}
}

// TestReportJSONMatchesTheConsumer covers the byte shape the shell suite matches:
// `json.dumps(sort_keys=True)` spacing, not Go's compact encoder.
func TestReportJSONMatchesTheConsumer(t *testing.T) {
	report := probeReport{
		Attempts: 1,
		Cleanup:  cleanupFacts{Signalled: []string{"SIGTERM"}, PortReleased: true, ExecTmpGCD: true},
		Command:  "/opt/agent-vm/dsh/bin",
		Facts:    webFacts{Status: 200, Redirects: 1, Bytes: 147},
		Ownership: ownershipFacts{
			Proven: true,
			PID:    42,
		},
		Port: 5000,
		URL:  "http://127.0.0.1:5000/",
	}
	want := `{"attempts": 1, "cleanup": {"execTmpRemoved": true, "portReleased": true, ` +
		`"signalled": ["SIGTERM"], "survivors": 0}, "command": "/opt/agent-vm/dsh/bin", ` +
		`"http": {"bytes": 147, "markers": ["<!doctype html", "__dsh_boot__", ` +
		`"<title>deepseek harness</title>"], "redirects": 1, "status": 200}, ` +
		`"ownership": {"listenerInode": true, "pid": 42}, "port": 5000, "url": "http://127.0.0.1:5000/"}` + "\n"
	if got := string(report.appendJSON(nil)); got != want {
		t.Fatalf("report =\n%s\nwant\n%s", got, want)
	}
	skipped := probeReport{Ownership: ownershipFacts{Reason: "fixture host without procfs: no procfs"}}
	if got := string(skipped.appendJSON(nil)); !strings.Contains(got,
		`"ownership": {"listenerInode": null, "reason": "fixture host without procfs: no procfs"}`) {
		t.Fatalf("skipped report = %s", got)
	}
}

// TestProbeDeadlineDiagnostic keeps the quoted bound in the form the consumer read.
func TestProbeDeadlineDiagnostic(t *testing.T) {
	for _, want := range []struct {
		duration time.Duration
		text     string
	}{
		{60 * time.Second, "60.0"},
		{500 * time.Millisecond, "0.5"},
		{1500 * time.Millisecond, "1.5"},
	} {
		if got := pythonFloat(want.duration); got != want.text {
			t.Errorf("pythonFloat(%s) = %q, want %q", want.duration, got, want.text)
		}
	}
}

// TestPrepareArgumentGrammar pins the usage/refusal split: a malformed argument is 2
// and carries the documented interface, never a refusal.
func TestPrepareArgumentGrammar(t *testing.T) {
	for _, rejected := range [][]string{
		nil,
		{"prefix"},
		{"prefix", "0.1.0"},
		{"prefix", "0.1.0", "11.11.0", "--bogus"},
		{"prefix", "0.1.0", "11.11.0", "--refresh-lock", "--refresh-lock"},
	} {
		_, err := ParsePrepareArguments(rejected)
		var usage *Usage
		if err == nil || !asUsage(err, &usage) || usage.Text != prepareUsage {
			t.Errorf("arguments %v = %v", rejected, err)
		}
	}
	request, err := ParsePrepareArguments([]string{"prefix", "0.1.0", "", "--refresh-lock"})
	if err != nil || !request.refresh || request.pnpmVersion != "" {
		t.Fatalf("request = %+v, %v", request, err)
	}
}

func asUsage(err error, target **Usage) bool {
	usage, ok := err.(*Usage)
	if ok {
		*target = usage
	}
	return ok
}

// TestExactVersionGrammar keeps a range from reaching the manager as an exact slot.
func TestExactVersionGrammar(t *testing.T) {
	for _, accepted := range []string{"0.1.5-rc.2", "1.2.3", "1.0.0-beta.1+build.5"} {
		if !exactVersion(accepted) {
			t.Errorf("exactVersion(%q) = false", accepted)
		}
	}
	for _, rejected := range []string{"latest", "next", "^1.2.3", "1.2", "01.2.3", "1.2.3-01", "1.2.3\n"} {
		if exactVersion(rejected) {
			t.Errorf("exactVersion(%q) = true", rejected)
		}
	}
}
