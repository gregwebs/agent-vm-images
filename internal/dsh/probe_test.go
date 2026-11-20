package dsh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
	if signal, err := child.teardown(300*time.Millisecond, context.Background()); err != nil || signal != 0 {
		t.Fatalf("teardown = %v, %v", signal, err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("teardown did not bound itself: %s", elapsed)
	}
	if !askGroup(child.pgid).empty() {
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
	if signal, err := child.teardown(200*time.Millisecond, context.Background()); err != nil || signal != 0 {
		t.Fatalf("teardown = %v, %v", signal, err)
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
	child := &webChild{command: command, pgid: command.Process.Pid, done: make(chan int, 1),
		capture: newIdleCapture(t)}
	child.reapInBackground()
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

// TestTeardownProvesEmptinessOrRefuses is the teardown's honesty table. The signal is
// injected because the answers that matter -- a kernel refusing to say and a member
// that stays -- are not reachable from a test on either host.
//
// ESRCH is the only answer that proves the group empty. EPERM escalates to KILL, and a
// group that the last probe still cannot prove empty is a Refusal rather than a pass.
func TestTeardownProvesEmptinessOrRefuses(t *testing.T) {
	saved := signalGroup
	t.Cleanup(func() { signalGroup = saved })
	// The leader is already reaped, so the only question left is the group probe.
	child := func() *webChild {
		done := make(chan int, 1)
		done <- 0
		return &webChild{pgid: 4321, done: done}
	}

	t.Run("an empty group is proved immediately", func(t *testing.T) {
		var signals []syscall.Signal
		signalGroup = func(_ int, signal syscall.Signal) error {
			signals = append(signals, signal)
			return syscall.ESRCH
		}
		if _, err := child().teardown(50*time.Millisecond, context.Background()); err != nil {
			t.Fatalf("teardown = %v, want success", err)
		}
		for _, signal := range signals {
			if signal != 0 {
				t.Fatalf("signals = %v, want no signal to an empty group", signals)
			}
		}
	})

	t.Run("a refused probe escalates to KILL and is proved", func(t *testing.T) {
		killed := false
		signalGroup = func(_ int, signal syscall.Signal) error {
			switch signal {
			case syscall.SIGTERM:
				return nil
			case syscall.SIGKILL:
				killed = true
				return nil
			}
			if killed {
				return syscall.ESRCH
			}
			// A member exists, but the kernel will not say anything about it.
			return syscall.EPERM
		}
		if _, err := child().teardown(50*time.Millisecond, context.Background()); err != nil {
			t.Fatalf("teardown = %v, want success once the KILL proved it", err)
		}
		if !killed {
			t.Fatal("an unproven group was accepted without escalating to KILL")
		}
	})

	t.Run("a group that never proves itself is refused", func(t *testing.T) {
		started := time.Now()
		signalGroup = func(_ int, _ syscall.Signal) error { return syscall.EPERM }
		_, err := child().teardown(50*time.Millisecond, context.Background())
		if err == nil {
			t.Fatal("a group the kernel would not answer for was reported as torn down")
		}
		for _, want := range []string{"not provably gone", "refused to answer"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want %q in it", err, want)
			}
		}
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			t.Fatalf("the refusal did not bound itself: %s", elapsed)
		}
	})

	t.Run("a member that exists is not empty", func(t *testing.T) {
		// kill -0 succeeding means a member is there: neither the poll nor the final
		// phase may read that as success.
		signalGroup = func(_ int, _ syscall.Signal) error { return nil }
		_, err := child().teardown(50*time.Millisecond, context.Background())
		if err == nil {
			t.Fatal("a group with a live member was reported as torn down")
		}
		if !strings.Contains(err.Error(), "members remain") {
			t.Fatalf("error = %v, want the surviving members named", err)
		}
	})
}

// TestTeardownProvesEmptinessOnlyAfterTheLeaderIsReaped is the real-process
// convergence: the leader exits on TERM and stays a group member until its Wait has
// reaped it, so the same child is provable with that Wait running and unprovable
// without it. Deleting the reap breaks the first case; reading the second case's
// EPERM as gone breaks it on the host that answers EPERM (Darwin).
func TestTeardownProvesEmptinessOnlyAfterTheLeaderIsReaped(t *testing.T) {
	t.Run("a reaped leader converges to ESRCH", func(t *testing.T) {
		child := startExitingOnTerm(t)
		child.reapInBackground()
		if signal, err := child.teardown(500*time.Millisecond, context.Background()); err != nil || signal != 0 {
			t.Fatalf("teardown = %v, %v", signal, err)
		}
		if !askGroup(child.pgid).empty() {
			t.Fatalf("group %d was not proved empty after the leader was reaped", child.pgid)
		}
	})

	t.Run("an unreaped leader is not provable", func(t *testing.T) {
		child := startExitingOnTerm(t)
		_, err := child.teardown(100*time.Millisecond, context.Background())
		if err == nil {
			t.Fatal("an unreaped leader counted as an empty group")
		}
		// The kernel answers differently: Darwin refuses to say anything about a group of
		// our own unwaited zombies, Linux counts the zombie as a member.
		want := "members remain"
		if runtime.GOOS == "darwin" {
			want = "the kernel refused to answer"
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("teardown = %v, want %q in it", err, want)
		}
	})
}

// startExitingOnTerm starts one session whose leader leaves on TERM, so what is left of
// the group is exactly the leader's own unreaped exit.
func startExitingOnTerm(t *testing.T) *webChild {
	t.Helper()
	command := exec.Command("sh", "-c", `trap 'exit 0' TERM; while :; do sleep 0.02; done`)
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	child := &webChild{command: command, pgid: command.Process.Pid, done: make(chan int, 1),
		capture: newIdleCapture(t)}
	t.Cleanup(func() {
		_ = syscall.Kill(-child.pgid, syscall.SIGKILL)
		_ = command.Wait()
	})
	return child
}

// TestTeardownReportsASignalInsteadOfSwallowingIt covers the window the readiness
// poll misses: a signal that lands while the group is being emptied must be reported,
// so a caller cannot read the run as a pass.
func TestTeardownReportsASignalInsteadOfSwallowingIt(t *testing.T) {
	child, _ := startIgnoringChild(t)
	// The signal arrives after the teardown has begun, and only once: the escalation
	// wait is its first reader, so the latch is what carries it to the result. A
	// `pending` that kept answering would hide the rule this case exists to pin.
	calls := 0
	child.pending = func() (int, bool) {
		calls++
		if calls == 3 {
			return 143, true
		}
		return 0, false
	}
	signal, err := child.teardown(time.Second, context.Background())
	if err != nil {
		t.Fatalf("teardown = %v", err)
	}
	if signal != 143 {
		t.Fatalf("signal = %d, want the interruption it observed", signal)
	}
	if !askGroup(child.pgid).empty() {
		t.Fatalf("group %d survived an interrupted teardown", child.pgid)
	}
}

// TestTeardownHonoursTheCallerDeadline covers the outer bound: a cancelled context
// skips the graceful wait but still empties the group, so the constant the gate
// passes is a real bound rather than a comment.
func TestTeardownHonoursTheCallerDeadline(t *testing.T) {
	child, _ := startIgnoringChild(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if _, err := child.teardown(30*time.Second, ctx); err != nil {
		t.Fatalf("teardown = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a cancelled context did not shorten the escalation: %s", elapsed)
	}
	if !askGroup(child.pgid).empty() {
		t.Fatalf("group %d survived", child.pgid)
	}
}

// TestContainedByRefusesASymlinkWaypoint is the clause of `no_redirect` the probe
// restates, because the shared module exposes no verb for it: a parent that reaches
// the authorized root through a link is refused even though its lexical path is
// inside the root.
func TestContainedByRefusesASymlinkWaypoint(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "exec-temp")
	mustMkdir(t, inside)
	if !containedBy(inside, root) {
		t.Fatal("a real child of the root was refused")
	}
	// Same bytes, one waypoint: `root/link` is a symlink to a directory outside the
	// root, and `root/link/temp` is lexically below the root.
	outside := t.TempDir()
	mustMkdir(t, filepath.Join(outside, "temp"))
	waypoint := filepath.Join(root, "link")
	if err := os.Symlink(outside, waypoint); err != nil {
		t.Fatal(err)
	}
	if containedBy(filepath.Join(waypoint, "temp"), root) {
		t.Fatal("a symlink waypoint was accepted")
	}
	// The path as a symlink itself is refused too, which is `no_redirect`'s
	// leaf-link rule.
	leaf := filepath.Join(root, "leaf")
	if err := os.Symlink(inside, leaf); err != nil {
		t.Fatal(err)
	}
	if containedBy(leaf, root) {
		t.Fatal("a symlinked parent was accepted")
	}
	if containedBy(root, root) {
		t.Fatal("the root itself was accepted as a strict child")
	}
	if containedBy(filepath.Join(root, "..", "elsewhere"), root) {
		t.Fatal("a sibling of the root was accepted")
	}
}

// TestExecTempParentRefusals pins each clause of the one containment rule the probe
// owns: absolute, no dot components, inside the root, a directory, caller-owned.
func TestExecTempParentRefusals(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "exec-temp")
	mustMkdir(t, inside)
	capability := RuntimeCapability{Root: root}
	owns := func(os.FileInfo) bool { return true }
	parent, err := execTempParent(ProbeOptions{FixtureExecTmpdir: inside}, capability, owns)
	if err != nil || parent != inside {
		t.Fatalf("parent = %q, %v", parent, err)
	}
	cases := []struct {
		name    string
		options ProbeOptions
		owns    func(os.FileInfo) bool
		want    string
		status  int
	}{
		{"relative", ProbeOptions{FixtureExecTmpdir: "tmp"}, owns, "absolute lexical path required", 2},
		{"dot", ProbeOptions{FixtureExecTmpdir: root + "/./x"}, owns, "absolute lexical path required", 2},
		{"outside", ProbeOptions{FixtureExecTmpdir: t.TempDir()}, owns,
			"path must be a strict child of authorized root", 1},
		{"the root", ProbeOptions{FixtureExecTmpdir: root}, owns,
			"path must be a strict child of authorized root", 1},
		{"missing", ProbeOptions{FixtureExecTmpdir: filepath.Join(root, "absent")}, owns,
			"must be a directory", 1},
		{"file", ProbeOptions{FixtureExecTmpdir: filepath.Join(root, "file")}, owns, "must be a directory", 1},
		{"not caller-owned", ProbeOptions{FixtureExecTmpdir: inside}, func(os.FileInfo) bool { return false },
			"must be caller-owned", 1},
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range cases {
		_, err := execTempParent(testCase.options, capability, testCase.owns)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: error = %v", testCase.name, err)
			continue
		}
		if status := statusOf(err); status != testCase.status {
			t.Errorf("%s: status = %d, want %d", testCase.name, status, testCase.status)
		}
	}
	// No flag means the system writable layer, never a caller value.
	if parent, err := execTempParent(ProbeOptions{}, capability, owns); err != nil || parent != "/tmp" {
		t.Fatalf("default parent = %q, %v", parent, err)
	}
}

// statusOf is the refusal classification the CLI maps onto an exit status.
func statusOf(err error) int {
	var usage *Usage
	if errors.As(err, &usage) {
		return 2
	}
	return 1
}

// TestProbeSeamRefusals keeps production free of fixture inputs, and keeps the
// distinction the predecessors made: a well-formed request the capability cannot
// honour is a refusal (1), a malformed one is usage (2).
func TestProbeSeamRefusals(t *testing.T) {
	cases := []struct {
		name       string
		production bool
		options    ProbeOptions
		want       string
		status     int
	}{
		{"production child env", true, ProbeOptions{FixtureChildEnv: []string{"DSH_WEB_FIXTURE_CASE=valid"}},
			"does not accept a fixture child environment", 1},
		{"production exec temp", true, ProbeOptions{FixtureExecTmpdir: "/tmp"}, "exec temp parent is fixed", 1},
		{"malformed entry", false, ProbeOptions{FixtureChildEnv: []string{"NOT_AN_ASSIGNMENT"}},
			"takes NAME=VALUE", 2},
		{"bad name", false, ProbeOptions{FixtureChildEnv: []string{"1BAD=value"}}, "takes NAME=VALUE", 2},
		{"shadowing PATH", false, ProbeOptions{FixtureChildEnv: []string{"PATH=/evil"}},
			"may not set PATH", 1},
		{"injected preload", false, ProbeOptions{FixtureChildEnv: []string{"NODE_OPTIONS=--require=x"}},
			"may not set NODE_OPTIONS", 1},
		{"redirect", false, ProbeOptions{FixtureChildEnv: []string{"TMPDIR=/evil"}}, "may not set TMPDIR", 1},
	}
	for _, testCase := range cases {
		err := probeSeam(testCase.production, testCase.options)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: error = %v", testCase.name, err)
			continue
		}
		if status := statusOf(err); status != testCase.status {
			t.Errorf("%s: status = %d, want %d", testCase.name, status, testCase.status)
		}
	}
	if err := probeSeam(false, ProbeOptions{FixtureChildEnv: []string{"DSH_WEB_FIXTURE_CASE=valid"}}); err != nil {
		t.Fatalf("a fixture entry was refused in a fixture: %v", err)
	}
	// A bound is not a seam: production honours a shorter one, because a bound only
	// limits the run and the production deadline stays the default.
	short := time.Second
	if err := probeSeam(true, ProbeOptions{Readiness: &short, Escalation: &short}); err != nil {
		t.Fatalf("production refused a bound: %v", err)
	}
}

// TestBoundedKeepsTheTail pins the direction: the tail is what a crash publishes, so
// a failure that kept the head would hide the cause.
func TestBoundedKeepsTheTail(t *testing.T) {
	if got := bounded("short", 10); got != "short" {
		t.Fatalf("bounded(short) = %q", got)
	}
	if got := bounded("0123456789", 4); got != "6789" {
		t.Fatalf("bounded = %q, want the tail", got)
	}
	// A cut that lands inside a multi-byte rune backs off to the rune boundary
	// rather than publishing a broken sequence.
	if got := bounded("aé", 2); got != "é" {
		t.Fatalf("bounded = %q", got)
	}
}

// TestProbeBounds keeps the production deadline a ceiling rather than a default, and it
// lives here so the exported Probe cannot be called with a slower gate than the CLI
// allows.
func TestProbeBounds(t *testing.T) {
	bound := func(seconds time.Duration) *time.Duration { return &seconds }
	if err := validateBounds(nil, nil); err != nil {
		t.Fatalf("no bound = %v", err)
	}
	if err := validateBounds(bound(DefaultReadiness), bound(DefaultEscalation)); err != nil {
		t.Fatalf("the production deadline was refused: %v", err)
	}
	for _, rejected := range []time.Duration{0, -time.Second, DefaultReadiness + time.Millisecond} {
		for _, pair := range [][2]*time.Duration{{bound(rejected), nil}, {nil, bound(rejected)}} {
			err := validateBounds(pair[0], pair[1])
			var usage *Usage
			if err == nil || !errors.As(err, &usage) || usage.Text != ProbeBoundRefusal {
				t.Errorf("validateBounds(%v) = %v", rejected, err)
			}
		}
	}
}

// TestScratchTokenMatchesTheSharedModule keeps the one literal this package owns in
// step with the consumer table: the token names the directory a leftover belongs to,
// and no verb exposes it.
func TestScratchTokenMatchesTheSharedModule(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "images", "tools", "pnpm-project.py"))
	if err != nil {
		t.Skipf("the shared module is not beside this package: %v", err)
	}
	row := regexp.MustCompile(`Consumer\('dsh',\s*Path\('([^']*)'\),\s*Path\('([^']*)'\),\s*'([^']*)'`).
		FindStringSubmatch(string(source))
	if row == nil {
		t.Fatal("the shared module's dsh consumer row is not recognisable")
	}
	if row[3] != scratchToken {
		t.Fatalf("scratchToken = %q, the module says %q", scratchToken, row[3])
	}
}

// TestAwaitReadyHonoursTheCallerDeadline pins the outer bound where it is checked: a
// readiness loop with a cancelled context fails instead of polling until its own
// deadline. Without it the gate's 70 s allowance would be a comment.
func TestAwaitReadyHonoursTheCallerDeadline(t *testing.T) {
	child, _ := startIgnoringChild(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := probeRun{
		ctx:        ctx,
		command:    "/bin/true",
		readiness:  30 * time.Second,
		escalation: 100 * time.Millisecond,
		prober:     prober{host: "darwin", proc: t.TempDir()},
	}
	started := time.Now()
	_, _, err := awaitReady(run, child, 1)
	if err == nil || !strings.Contains(err.Error(), "exceeded its caller's deadline") {
		t.Fatalf("awaitReady = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a cancelled context did not shorten the readiness loop: %s", elapsed)
	}
}

// TestFinishReportsASignalThatLandedAfterTeardown covers the window between the group
// being emptied and the report being built: a signal there must report, not pass.
func TestFinishReportsASignalThatLandedAfterTeardown(t *testing.T) {
	port, err := reservePort()
	if err != nil {
		t.Fatal(err)
	}
	interrupts := make(chan int, 1)
	run := probeRun{interrupts: interrupts, command: "/bin/true"}
	report, err := run.finish(newIdleChild(t), port, webFacts{Status: 200}, 1)
	if err != nil {
		t.Fatalf("finish = %v", err)
	}
	if report.Port != port || report.Facts.Status != 200 {
		t.Fatalf("report = %+v", report)
	}
	interrupts <- 143
	if _, err := run.finish(newIdleChild(t), port, webFacts{Status: 200}, 1); err == nil ||
		!strings.Contains(err.Error(), "signal") {
		t.Fatalf("finish = %v, want the interruption", err)
	} else {
		var interrupted *Interrupted
		if !errors.As(err, &interrupted) || interrupted.Status != 143 {
			t.Fatalf("finish = %v, want Interrupted{143}", err)
		}
	}
}

// TestFinishRefusesAListenerThatSurvived keeps the residual-port rule where the survey
// reads it: a listener left on the reserved port is a failed proof, not a released one.
func TestFinishRefusesAListenerThatSurvived(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	run := probeRun{command: "/bin/true"}
	if _, err := run.finish(newIdleChild(t), port, webFacts{Status: 200}, 1); err == nil ||
		!strings.Contains(err.Error(), "still has a listener after cleanup") {
		t.Fatalf("finish = %v", err)
	}
}

// newIdleCapture is a capture over two already-closed pipes: a case that reads the
// child's output only needs an empty one.
func newIdleCapture(t *testing.T) *capture {
	t.Helper()
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	return newCapture(stdoutRead, stderrRead)
}

// newIdleChild is a child whose pipes are already closed: the survey's inputs are all
// that matter. Its pgid is one no group can hold, so a probe that is never supposed to
// happen answers ESRCH instead of signalling this process's own group.
func newIdleChild(t *testing.T) *webChild {
	t.Helper()
	return &webChild{capture: newIdleCapture(t), pgid: 1 << 30}
}

// TestInterruptedReportIsTheLastWord covers the decision `Probe` makes after its work
// returns: the window is sub-millisecond, so the rule is tested here rather than through
// a run that would have to lose a race to observe it.
func TestInterruptedReportIsTheLastWord(t *testing.T) {
	report := []byte(`{"attempts": 1}`)

	// No signal: the report and the error pass through unchanged, including the nil pair.
	if payload, err := interruptedReport(nil, nil, nil); payload != nil || err != nil {
		t.Fatalf("nil pair = %q, %v", payload, err)
	}
	if payload, err := interruptedReport(make(chan int), report, nil); string(payload) != string(report) || err != nil {
		t.Fatalf("quiet = %q, %v", payload, err)
	}
	failure := refusal("something else failed")
	if payload, err := interruptedReport(make(chan int), nil, failure); payload != nil || err != failure {
		t.Fatalf("quiet failure = %q, %v", payload, err)
	}

	// A delivered signal outranks both: a report the caller never witnessed must not be
	// published, and the interruption is what it asked to know about.
	for _, queued := range []int{129, 130, 143} {
		// A fresh channel per call, so a refusal to read it fails the assertion instead of
		// blocking the second send.
		withReport := make(chan int, 1)
		withReport <- queued
		payload, err := interruptedReport(withReport, report, nil)
		var interruption *Interrupted
		if payload != nil || !errors.As(err, &interruption) || interruption.Status != queued {
			t.Errorf("queued %d with a report = %q, %v", queued, payload, err)
		}
		withFailure := make(chan int, 1)
		withFailure <- queued
		payload, err = interruptedReport(withFailure, nil, failure)
		if payload != nil || !errors.As(err, &interruption) || interruption.Status != queued {
			t.Errorf("queued %d with a failure = %q, %v", queued, payload, err)
		}
	}
}
