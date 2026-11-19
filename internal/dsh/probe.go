package dsh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The probe owns the parts a JavaScript runtime probe cannot: monotonic
// deadlines, a private process group, bounded concurrent capture, the token
// handshake and a teardown that proves nothing survived. `check-runtime.mjs` keeps
// the JavaScript probes.
//
// Why the token URL is requested instead of the bare page: `dsh web` prints one
// `dsh web: http://127.0.0.1:<port>/?token=<secret>` line. The token is
// unguessable, so a frontend response that accepts it can only come from the child
// this invocation started -- a competing server on the same port rejects it. On
// Linux the listening socket must additionally appear in the child group's own
// descriptors, because a valid-looking HTML page is not ownership proof either.
const (
	maxCapture     = 64 * 1024
	maxResponse    = 64 * 1024
	drainTimeout   = 10 * time.Second
	requestTimeout = 1 * time.Second
	pollInterval   = 20 * time.Millisecond
	groupSettle    = 1 * time.Second
	maxAttempts    = 3
)

// DefaultReadiness is the production readiness deadline and the largest bound a
// caller may name: a longer wait is not a health gate.
const DefaultReadiness = 60 * time.Second

// DefaultEscalation is how long a TERM is given before a KILL, in production.
const DefaultEscalation = 5 * time.Second

// The markers a shipped dsh frontend carries, lower case, matched case-insensitively.
var probeMarkers = [][]byte{
	[]byte("<!doctype html"),
	[]byte("__dsh_boot__"),
	[]byte("<title>deepseek harness</title>"),
}

// fatalStartupMarkers are the startup failures Node prints and then keeps running
// (or exits 0 after). A gate must read them as failures, not as "still starting".
var fatalStartupMarkers = []string{
	"requires the Cordis HMR service",
	"ERR_MODULE_NOT_FOUND",
	"Cannot find module",
}

var redirectStatuses = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

// webURLPattern is the single advertised line `dsh web` prints. The token charset is
// what a URL-safe base64 secret can carry, at the length the runtime generates.
var webURLPattern = regexp.MustCompile(`^dsh web: http://127\.0\.0\.1:(\d+)/\?token=([A-Za-z0-9._~-]{32,})$`)

// tokenRedaction keeps a failing child's own diagnostics from publishing the
// session token into a build log.
var tokenRedaction = regexp.MustCompile(`(token=)[A-Za-z0-9._~-]+`)

// ProbeOptions is one bounded web-readiness probe.
type ProbeOptions struct {
	// Source is the dsh tools directory: it holds project.py and the fixtures the
	// probe's child environment does not otherwise see.
	Source string
	// Prefix is the installed payload the probe never writes.
	Prefix string
	// Command is the installed command the runtime authorization proved.
	Command string
	// ScratchRoot is the invocation-owned directory the probe's scratch is created
	// in, already authorized as a runtime scratch.
	ScratchRoot string
	// Readiness and Escalation override the production bounds. Nil keeps them, and a
	// production capability refuses an override rather than honouring it.
	Readiness  *time.Duration
	Escalation *time.Duration
	// FixtureChildEnv is the fixture seam: NAME=VALUE entries handed to the child
	// instead of being inherited from this process's environment. Production refuses
	// the flag entirely, so no ambient variable can select fixture behaviour.
	FixtureChildEnv []string
	// FixtureExecTmpdir replaces the system exec temp parent. Fixture only, for the
	// same reason.
	FixtureExecTmpdir string
}

// webFacts is what the frontend handshake proved.
type webFacts struct {
	Status    int
	Redirects int
	Bytes     int
}

// ownershipFacts is the listener proof. Proven is false only on an explicit
// non-Linux fixture host, which records the reason instead of claiming a proof.
type ownershipFacts struct {
	Proven bool
	PID    int
	Reason string
}

type cleanupFacts struct {
	Signalled    []string
	PortReleased bool
	ExecTmpGCD   bool
}

type probeReport struct {
	Attempts  int
	Cleanup   cleanupFacts
	Command   string
	Facts     webFacts
	Ownership ownershipFacts
	Port      int
	URL       string
}

// prober carries the two inputs the socket-ownership proof reads. Production never
// varies either; the self-test and the package tests do, which is why they are
// fields rather than package globals.
type prober struct {
	host string
	proc string
}

func productionProber() prober { return prober{host: hostPlatform(), proc: "/proc"} }

// Probe runs one readiness probe and prints nothing: the report bytes are the
// caller's payload, so a refusal can never appear on stdout.
func Probe(ctx context.Context, options ProbeOptions) ([]byte, error) {
	interrupts := make(chan int, 1)
	stop := notifyInterrupts(interrupts)
	defer stop()
	return probe(ctx, options, interrupts, productionProber())
}

// notifyInterrupts turns the three signals the shell handled into the 128+N status
// its caller expects. The handler is registered once so a second signal is dropped
// instead of being re-entered.
func notifyInterrupts(destination chan<- int) func() {
	signals := make(chan os.Signal, 3)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for received := range signals {
			status := 0
			switch received {
			case syscall.SIGINT:
				status = 130
			case syscall.SIGTERM:
				status = 143
			case syscall.SIGHUP:
				status = 129
			}
			if status == 0 {
				continue
			}
			select {
			case destination <- status:
			default:
			}
			return
		}
	}()
	return func() {
		signal.Stop(signals)
		close(signals)
		<-done
	}
}

func probe(ctx context.Context, options ProbeOptions, interrupts <-chan int, environment prober) ([]byte, error) {
	project := Project{Source: options.Source}
	if err := project.Guard(ctx, options.Prefix); err != nil {
		return nil, err
	}
	capability, err := project.AuthorizeRuntime(ctx, options.Prefix, options.Command, options.ScratchRoot)
	if err != nil {
		return nil, err
	}
	readiness := DefaultReadiness
	if options.Readiness != nil {
		readiness = *options.Readiness
	}
	escalation := DefaultEscalation
	if options.Escalation != nil {
		escalation = *options.Escalation
	}
	overrides := options.Readiness != nil || options.Escalation != nil
	if capability.Production && overrides {
		return nil, &Usage{Text: "production readiness and escalation bounds are fixed"}
	}
	if capability.Production && (len(options.FixtureChildEnv) > 0 || options.FixtureExecTmpdir != "") {
		return nil, &Usage{Text: "production runtime does not accept a fixture seam"}
	}
	work, err := os.MkdirTemp(options.ScratchRoot, "dsh-")
	if err != nil {
		return nil, refusal(err.Error())
	}
	// The scratch is this process's own child inside a root runtime authorization has
	// already proved (absolute, symlink-free, caller-owned, below HOME). The shared
	// module's `cleanup` verb re-authorizes a path against the consumer's scratch
	// parent and the fixture root only, so it cannot describe a runtime scratch; the
	// removal therefore happens here, where the creator is known.
	defer func() { _ = os.RemoveAll(work) }()
	execParent, err := execTempParent(options, capability)
	if err != nil {
		return nil, err
	}
	execTmp, err := os.MkdirTemp(execParent, "dsh-probe-")
	if err != nil {
		return nil, refusal(err.Error())
	}
	if err := os.Chmod(execTmp, 0o700); err != nil {
		return nil, refusal(err.Error())
	}
	defer func() { _ = os.RemoveAll(execTmp) }()
	report, err := runProbe(probeRun{
		ctx:        ctx,
		interrupts: interrupts,
		command:    options.Command,
		work:       work,
		execTmp:    execTmp,
		readiness:  readiness,
		escalation: escalation,
		production: capability.Production,
		prober:     environment,
		fixtureEnv: options.FixtureChildEnv,
	})
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(execTmp); err != nil {
		return nil, refusal("private exec TMPDIR could not be removed: " + err.Error())
	}
	if fileExists(execTmp) {
		return nil, &Refusal{Text: "private exec TMPDIR survived cleanup"}
	}
	report.Cleanup.ExecTmpGCD = true
	return report.appendJSON(nil), nil
}

// execTempParent is the system writable layer: the product dlopens its native
// internals binding through TMPDIR, HOME may be a noexec tmpfs, and a noexec TMPDIR
// is unsupported by `dsh web`. A fixture may name the parent, but only inside the
// root runtime authorization proved, so the seam cannot move the exec directory out
// of the invocation.
func execTempParent(options ProbeOptions, capability RuntimeCapability) (string, error) {
	if options.FixtureExecTmpdir == "" {
		return "/tmp", nil
	}
	parent := options.FixtureExecTmpdir
	if !filepath.IsAbs(parent) || hasDotComponent(parent) {
		return "", &Usage{Text: "absolute lexical path required: " + parent}
	}
	if !containedBy(parent, capability.Root) {
		return "", &Refusal{Text: "path must be a strict child of authorized root: " + parent}
	}
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() {
		return "", &Refusal{Text: "fixture exec temp parent must be a directory: " + parent}
	}
	if ownerOf(info) != os.Getuid() {
		return "", &Refusal{Text: "fixture exec temp parent must be caller-owned"}
	}
	return parent, nil
}

// containedBy is the shared module's `below` plus its symlink-waypoint refusal,
// restated for the one input the module has no verb for: the fixture exec temp
// parent. It is deliberately narrow -- absolute, no `.`/`..`, no symlink anywhere on
// the way down, and a strict child of the authorized root.
func containedBy(path, root string) bool {
	if path == root || !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return false
	}
	for current := path; current != root; current = filepath.Dir(current) {
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if current == string(filepath.Separator) {
			break
		}
	}
	return true
}

// probeRun is one attempt sequence: everything the retry loop needs.
type probeRun struct {
	ctx        context.Context
	interrupts <-chan int
	command    string
	work       string
	execTmp    string
	readiness  time.Duration
	escalation time.Duration
	production bool
	prober     prober
	fixtureEnv []string
}

// runProbe is the retry loop. Only a failure a second attempt can clear is retried:
// a port another process already holds is not a property of the payload.
func runProbe(run probeRun) (probeReport, error) {
	cwd := filepath.Join(run.work, "cwd")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		return probeReport{}, refusal(err.Error())
	}
	var failure error
	for attempts := 1; attempts <= maxAttempts; attempts++ {
		var report probeReport
		report, failure = run.attempt(attempts, cwd)
		if failure == nil {
			return report, nil
		}
		var retry *retryable
		if !errors.As(failure, &retry) {
			break
		}
	}
	return probeReport{}, failure
}

// retryable marks a failure a second attempt may clear.
type retryable struct{ error }

// attempt starts one child, proves readiness and ownership, and tears the group
// down. Teardown runs on every path, including an interrupt, and a teardown that
// could not empty the group replaces whatever was being reported.
func (run probeRun) attempt(number int, cwd string) (probeReport, error) {
	port, err := reservePort()
	if err != nil {
		return probeReport{}, refusal(err.Error())
	}
	child, err := startWeb(run, cwd, port)
	if err != nil {
		return probeReport{}, refusal(err.Error())
	}
	facts, readyErr := func() (*webFacts, error) {
		_, readyFacts, err := awaitReady(run, child, port)
		if err != nil {
			return nil, err
		}
		if child.poll().exited {
			return nil, &Refusal{Text: "web exited immediately after the readiness response"}
		}
		proof, err := run.prober.ownership(port, child.pgid, run.production)
		if err != nil {
			return nil, err
		}
		child.proof = proof
		return readyFacts, nil
	}()
	teardownErr := child.teardown(run.escalation)
	if teardownErr != nil {
		return probeReport{}, teardownErr
	}
	if readyErr != nil {
		return probeReport{}, readyErr
	}
	// The readers drain before the success report: a read error or an overflow that
	// lands after the readiness response is still a failed proof, and a reader that
	// stopped at the capture bound would silently truncate it.
	if err := child.capture.drain(); err != nil {
		return probeReport{}, &Refusal{Text: "the child output could not be read completely (" + err.Error() + ")"}
	}
	if child.capture.overflowed() {
		return probeReport{}, refusalf("child output exceeded %d bytes", maxCapture)
	}
	if !portFree(port) {
		return probeReport{}, refusalf("reserved port %d still has a listener after cleanup", port)
	}
	cleanup := child.cleanupFacts
	cleanup.PortReleased = true
	return probeReport{
		Attempts:  number,
		Cleanup:   cleanup,
		Command:   run.command,
		Facts:     *facts,
		Ownership: child.proof,
		Port:      port,
		URL:       fmt.Sprintf("http://127.0.0.1:%d/", port),
	}, nil
}

// startWeb launches the child in a private session: the group is the only handle a
// teardown has on descendants, so it must not be the caller's group.
func startWeb(run probeRun, cwd string, port int) (*webChild, error) {
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
		return nil, err
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = devNull.Close() }()
	command := exec.CommandContext(context.Background(), run.command,
		"web", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--no-open")
	command.Dir = cwd
	command.Env = childEnvironment(run)
	command.Stdin = devNull
	command.Stdout = stdoutWrite
	command.Stderr = stderrWrite
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		_ = stdoutRead.Close()
		_ = stderrRead.Close()
		_ = stdoutWrite.Close()
		_ = stderrWrite.Close()
		return nil, err
	}
	// The parent's copies must go, or the readers never see EOF once the child exits.
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	child := &webChild{command: command, pgid: command.Process.Pid, stdout: stdoutRead, stderr: stderrRead}
	child.capture = newCapture(stdoutRead, stderrRead)
	child.done = make(chan int, 1)
	go func() { child.done <- replayStatus(command.Wait()) }()
	return child, nil
}

// childEnvironment is an allowlist, not a scrub: a harness token, proxy or caller
// config must not reach the web process, and PATH/LANG are all a real dsh needs from
// outside.
func childEnvironment(run probeRun) []string {
	environment := []string{}
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TZ"} {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	home := filepath.Join(run.work, "home")
	environment = append(environment,
		"HOME="+home,
		"DSH_HOME="+filepath.Join(home, "dsh"),
		"XDG_CONFIG_HOME="+filepath.Join(run.work, "config"),
		"XDG_CACHE_HOME="+filepath.Join(run.work, "cache"),
		"XDG_DATA_HOME="+filepath.Join(run.work, "data"),
		"XDG_STATE_HOME="+filepath.Join(run.work, "state"),
		"TMPDIR="+run.execTmp,
		"TMP="+run.execTmp,
		"TEMP="+run.execTmp,
	)
	if !run.production {
		environment = append(environment, run.fixtureEnv...)
	}
	return environment
}

// webChild is one started `dsh web` and the proof handles it leaves behind.
type webChild struct {
	command      *exec.Cmd
	pgid         int
	stdout       *os.File
	stderr       *os.File
	capture      *capture
	done         chan int
	exitStatus   int
	exited       bool
	proof        ownershipFacts
	cleanupFacts cleanupFacts
}

type pollResult struct {
	exited bool
	status int
}

// poll is `process.poll()`: it never blocks, so the readiness loop can keep reading.
func (c *webChild) poll() pollResult {
	if c.exited {
		return pollResult{exited: true, status: c.exitStatus}
	}
	select {
	case status := <-c.done:
		c.exited, c.exitStatus = true, status
		return pollResult{exited: true, status: status}
	default:
		return pollResult{}
	}
}

// replayStatus is the shell's view of a child's exit: a signal reports 128+N.
func replayStatus(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if wait, ok := exit.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
			return 128 + signalNumber(wait)
		}
		if code := exit.ExitCode(); code >= 0 {
			return code
		}
	}
	return 1
}

// teardown kills the whole session, not the leader: a descendant that ignored the
// parent's death is a leaking process, and the port it holds is the proof.
func (c *webChild) teardown(escalation time.Duration) error {
	signalled := []string{}
	for _, step := range []struct {
		name string
		sig  syscall.Signal
	}{{"SIGTERM", syscall.SIGTERM}, {"SIGKILL", syscall.SIGKILL}} {
		if c.poll().exited && !groupAlive(c.pgid) {
			break
		}
		if err := syscall.Kill(-c.pgid, step.sig); err == nil || errors.Is(err, syscall.ESRCH) {
			signalled = append(signalled, step.name)
		}
		if c.waitGroupEmpty(escalation) {
			break
		}
	}
	c.cleanupFacts.Signalled = signalled
	settle := escalation
	if settle < groupSettle {
		settle = groupSettle
	}
	if !c.waitExited(settle) {
		_ = syscall.Kill(-c.pgid, syscall.SIGKILL)
		_ = c.waitExited(settle)
	}
	if groupAlive(c.pgid) {
		return &Refusal{Text: "process group still has members after the leader was reaped: " + pythonIntList(groupMembers("/proc", c.pgid))}
	}
	return nil
}

// waitExited waits for the leader to be reaped.
func (c *webChild) waitExited(limit time.Duration) bool {
	if c.poll().exited {
		return true
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case status := <-c.done:
		c.exited, c.exitStatus = true, status
		return true
	case <-timer.C:
		return false
	}
}

// waitGroupEmpty waits until no member of the group is left. An exited-but-unreaped
// leader is itself a group member, so it is reaped while waiting; otherwise the
// group could never look empty.
func (c *webChild) waitGroupEmpty(seconds time.Duration) bool {
	until := time.Now().Add(seconds)
	for groupAlive(c.pgid) {
		c.poll()
		if !time.Now().Before(until) {
			return false
		}
		time.Sleep(pollInterval)
	}
	return true
}

// groupAlive is `killpg(pgid, 0)`: ESRCH means gone, and EPERM means no member of
// the group is ours any more (Darwin reports it for a group holding only the
// just-exited leader), so the id was reused.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM)
}

// awaitReady polls the child's streams and the advertised URL until the frontend
// answers with the token this invocation reserved.
func awaitReady(run probeRun, child *webChild, port int) (string, *webFacts, error) {
	deadline := time.Now().Add(run.readiness)
	advertised := ""
	for {
		if status, interrupted := interrupted(run.interrupts); interrupted {
			return "", nil, &Interrupted{Status: status}
		}
		stdout, stderr := child.capture.text("stdout"), child.capture.text("stderr")
		if child.capture.overflowed() {
			return "", nil, refusalf("startup output exceeded %d bytes", maxCapture)
		}
		for _, marker := range fatalStartupMarkers {
			for _, text := range []string{stdout, stderr} {
				line, found := lineContaining(text, marker)
				if !found {
					continue
				}
				return "", nil, &Refusal{Text: "fatal startup diagnostic: " + bounded(strings.TrimSpace(line), 300)}
			}
		}
		if advertised == "" {
			found, err := parseWebURL(stdout, port)
			if err != nil {
				return "", nil, err
			}
			advertised = found
		}
		if polled := child.poll(); polled.exited {
			// A crashed Node process ends with its version banner, so the last line
			// alone hides the real cause. Publish a bounded tail of both streams.
			detail := childTail(stderr, stdout, polled.status)
			failure := &Refusal{Text: "web exited before readiness:\n" + detail}
			if strings.Contains(stderr, "EADDRINUSE") || strings.Contains(stderr, "address already in use") {
				return "", nil, &retryable{failure}
			}
			return "", nil, failure
		}
		if advertised != "" {
			outcome, ok := tryRequest(advertised, "")
			if ok {
				facts, err := frontend(outcome, port)
				if err != nil {
					return "", nil, err
				}
				if facts != nil {
					return advertised, facts, nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			return "", nil, &Refusal{Text: "web readiness deadline of " + pythonFloat(run.readiness) + " s elapsed"}
		}
		time.Sleep(pollInterval)
	}
}

// childTail bounds what a failing child publishes and keeps the token out of it.
func childTail(stderr, stdout string, status int) string {
	parts := []string{}
	if tail := bounded(strings.TrimSpace(stderr), 6000); tail != "" {
		parts = append(parts, tail)
	}
	if tail := bounded(strings.TrimSpace(stdout), 600); tail != "" {
		parts = append(parts, tail)
	}
	detail := tokenRedaction.ReplaceAllString(strings.Join(parts, "\n"), "$1<redacted>")
	if detail == "" {
		return fmt.Sprintf("exit status %d", status)
	}
	return detail
}

// bounded keeps the last n bytes of text.
func bounded(text string, n int) string {
	if len(text) <= n {
		return text
	}
	cut := len(text) - n
	for cut < len(text) && !utf8Start(text[cut]) {
		cut++
	}
	return text[cut:]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func lineContaining(text, marker string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, marker) {
			return line, true
		}
	}
	return "", false
}

// parseWebURL reads the advertised line. A line that is still being written is
// dropped, so a partial token is never compared.
func parseWebURL(text string, port int) (string, error) {
	lines := strings.Split(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "dsh web:") {
			continue
		}
		match := webURLPattern.FindStringSubmatch(line)
		if match == nil {
			return "", &Refusal{Text: "unexpected web url line: " + bounded(line, 200)}
		}
		advertised, err := strconv.Atoi(match[1])
		if err != nil {
			return "", &Refusal{Text: "unexpected web url line: " + bounded(line, 200)}
		}
		if advertised != port {
			return "", refusalf("web advertised port %d, but this invocation reserved %d", advertised, port)
		}
		return fmt.Sprintf("http://127.0.0.1:%d/?token=%s", port, match[2]), nil
	}
	return "", nil
}

// frontend returns the readiness facts, or nil while the port is not up yet.
func frontend(outcome *httpResponse, port int) (*webFacts, error) {
	redirects := 0
	final := outcome
	if redirectStatuses[outcome.Status] {
		location := outcome.header("Location")
		if location == "" {
			return nil, &Refusal{Text: "token request redirected without a Location header"}
		}
		target := resolveAgainst(fmt.Sprintf("http://127.0.0.1:%d/", port), location)
		parts, err := url.Parse(target)
		if err != nil {
			return nil, &Refusal{Text: "redirect left the probed origin: " + target}
		}
		targetPort := 0
		if value, err := strconv.Atoi(parts.Port()); err == nil {
			targetPort = value
		}
		if parts.Scheme != "http" || parts.Hostname() != "127.0.0.1" || targetPort != port {
			return nil, &Refusal{Text: fmt.Sprintf("redirect left the probed origin: %s://%s:%s",
				parts.Scheme, parts.Hostname(), parts.Port())}
		}
		next, ok := tryRequest(target, outcome.cookieHeader())
		if !ok {
			return nil, nil
		}
		final = next
		redirects = 1
	} else if outcome.Status != 200 {
		return nil, refusalf("token request returned HTTP %d", outcome.Status)
	}
	if final.Status != 200 {
		return nil, refusalf("final frontend response returned HTTP %d", final.Status)
	}
	if len(final.Body) > maxResponse {
		return nil, refusalf("frontend response exceeded %d bytes", maxResponse)
	}
	lowered := bytes.ToLower(final.Body)
	missing := []string{}
	for _, marker := range probeMarkers {
		if !bytes.Contains(lowered, marker) {
			missing = append(missing, string(marker))
		}
	}
	if len(missing) > 0 {
		return nil, &Refusal{Text: "response is not the shipped dsh frontend (missing " + strings.Join(missing, ", ") + ")"}
	}
	return &webFacts{Status: final.Status, Redirects: redirects, Bytes: len(final.Body)}, nil
}

// httpResponse is one response, kept in wire shape: the frontend handshake reads
// Location and Set-Cookie, which a decoded map would lose the order of.
type httpResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}

func (r *httpResponse) header(name string) string { return r.Headers.Get(name) }

func (r *httpResponse) cookieHeader() string {
	jar := []struct{ name, value string }{}
	for _, value := range r.Headers.Values("Set-Cookie") {
		pair := value
		if index := strings.IndexByte(pair, ';'); index >= 0 {
			pair = pair[:index]
		}
		name, raw, found := strings.Cut(pair, "=")
		name, raw = strings.TrimSpace(name), strings.TrimSpace(raw)
		if !found || name == "" || raw == "" {
			continue
		}
		replaced := false
		for index := range jar {
			if jar[index].name == name {
				jar[index].value, replaced = raw, true
				break
			}
		}
		if !replaced {
			jar = append(jar, struct{ name, value string }{name, raw})
		}
	}
	parts := make([]string, 0, len(jar))
	for _, cookie := range jar {
		parts = append(parts, cookie.name+"="+cookie.value)
	}
	return strings.Join(parts, "; ")
}

// resolveAgainst is urljoin for the one base the probe uses.
func resolveAgainst(base, location string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return location
	}
	reference, err := url.Parse(location)
	if err != nil {
		return location
	}
	return baseURL.ResolveReference(reference).String()
}

// request is one bounded GET. The connection is closed before returning, so a
// server that keeps it open cannot hold the probe.
func request(rawURL, cookie string) (*httpResponse, error) {
	parts, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(parts.Hostname(), parts.Port()), requestTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(requestTimeout)); err != nil {
		return nil, err
	}
	target := parts.RequestURI()
	if target == "" {
		target = "/"
	}
	head := "GET " + target + " HTTP/1.1\r\n" +
		"Host: " + net.JoinHostPort(parts.Hostname(), parts.Port()) + "\r\n" +
		"Connection: close\r\n" +
		"Accept: text/html\r\n"
	if cookie != "" {
		head += "Cookie: " + cookie + "\r\n"
	}
	head += "\r\n"
	if _, err := io.WriteString(conn, head); err != nil {
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return nil, err
	}
	return &httpResponse{Status: response.StatusCode, Headers: response.Header, Body: body}, nil
}

// tryRequest is a request whose network failure only means "not up yet".
func tryRequest(rawURL, cookie string) (*httpResponse, bool) {
	response, err := request(rawURL, cookie)
	if err != nil {
		return nil, false
	}
	return response, true
}

// reservePort asks the kernel for one free loopback port. The reservation is closed
// immediately: the child binds it, and the ownership proof reads who did.
func reservePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	return port, listener.Close()
}

// portFree reports whether the port can still be bound, which is the last check
// after a teardown.
func portFree(port int) bool {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	return listener.Close() == nil
}

// ownership ties the listening socket to the child's own process group. procfs is
// the only way to do that: a Linux run, or a production capability that cannot read
// it, has no ownership proof at all, so the proof fails closed. Only an explicit
// non-Linux fixture records a skip, and the token handshake remains the proof there.
func (p prober) ownership(port, pgid int, production bool) (ownershipFacts, error) {
	unproven := production || strings.HasPrefix(p.host, "linux")
	skip := func(reason string) (ownershipFacts, error) {
		if unproven {
			return ownershipFacts{}, &Refusal{Text: reason + ": listener ownership cannot be proven"}
		}
		return ownershipFacts{Reason: "fixture host without procfs: " + reason}, nil
	}
	if info, err := os.Stat(filepath.Join(p.proc, "self/fd")); err != nil || !info.IsDir() {
		return skip("no procfs")
	}
	inodes, err := p.listenerInodes(port)
	if err != nil {
		return skip("kernel socket tables unreadable")
	}
	if len(inodes) == 0 {
		return ownershipFacts{}, refusalf("no listener owns the reserved port %d", port)
	}
	for _, pid := range groupMembers(p.proc, pgid) {
		for _, link := range p.descriptorLinks(pid) {
			if !strings.HasPrefix(link, "socket:[") || !strings.HasSuffix(link, "]") {
				continue
			}
			if inodes[link[len("socket:["):len(link)-1]] {
				return ownershipFacts{Proven: true, PID: pid}, nil
			}
		}
	}
	return ownershipFacts{}, &Refusal{Text: "the reserved-port listener is not owned by the probed process group"}
}

// listenerInodes reads the LISTEN sockets on port from the kernel tables. A table
// that cannot be read is reported as such, never as "no listener".
func (p prober) listenerInodes(port int) (map[string]bool, error) {
	found := map[string]bool{}
	for _, table := range []string{"tcp", "tcp6"} {
		source, err := os.ReadFile(filepath.Join(p.proc, "net", table))
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(source), "\n")
		for _, line := range lines[1:] {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" {
				continue
			}
			index := strings.LastIndexByte(fields[1], ':')
			if index < 0 {
				continue
			}
			local, err := strconv.ParseUint(fields[1][index+1:], 16, 32)
			if err != nil || int(local) != port {
				continue
			}
			found[fields[9]] = true
		}
	}
	return found, nil
}

// descriptorLinks lists what one process holds open.
func (p prober) descriptorLinks(pid int) []string {
	directory := filepath.Join(p.proc, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	links := make([]string, 0, len(entries))
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(directory, entry.Name()))
		if err != nil {
			continue
		}
		links = append(links, target)
	}
	return links
}

// groupMembers is the process-group scan the survivor diagnostic names. It is empty
// on a host without procfs, where the diagnostic is not reachable anyway.
func groupMembers(proc string, pgid int) []int {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return nil
	}
	members := []int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		source, err := os.ReadFile(filepath.Join(proc, entry.Name(), "stat"))
		if err != nil {
			continue
		}
		text := string(source)
		closing := strings.LastIndexByte(text, ')')
		if closing < 0 {
			continue
		}
		fields := strings.Fields(text[closing+1:])
		if len(fields) > 2 && fields[2] == strconv.Itoa(pgid) {
			members = append(members, pid)
		}
	}
	return members
}

// capture is the bounded, concurrent capture of both pipes. Overflow is a hard
// failure: a gate that silently truncated the child's output would certify a
// payload whose own diagnostics were never read.
type capture struct {
	mutex    sync.Mutex
	buffers  map[string][]byte
	overflow bool
	failure  error
	// closing records that this process closed the pipes itself, so the read error
	// a closed descriptor produces is not reported as the child's output being
	// unreadable.
	closing bool
	readers []*os.File
	group   sync.WaitGroup
}

func newCapture(stdout, stderr *os.File) *capture {
	capture := &capture{buffers: map[string][]byte{"stdout": nil, "stderr": nil},
		readers: []*os.File{stdout, stderr}}
	capture.group.Add(2)
	go capture.pump(stdout, "stdout")
	go capture.pump(stderr, "stderr")
	return capture
}

func (c *capture) pump(stream *os.File, name string) {
	defer c.group.Done()
	buffer := make([]byte, 4096)
	for {
		read, err := stream.Read(buffer)
		if read > 0 {
			c.append(name, buffer[:read])
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return
		}
		c.mutex.Lock()
		if c.failure == nil && !c.closing {
			c.failure = errors.New(name + ": " + err.Error())
		}
		c.mutex.Unlock()
		return
	}
}

func (c *capture) append(name string, chunk []byte) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	room := maxCapture - len(c.buffers["stdout"]) - len(c.buffers["stderr"])
	if room <= 0 {
		c.overflow = true
		return
	}
	kept := chunk
	if len(kept) > room {
		kept = kept[:room]
		c.overflow = true
	}
	c.buffers[name] = append(c.buffers[name], kept...)
}

func (c *capture) text(name string) string {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return strings.ToValidUTF8(string(c.buffers[name]), "\uFFFD")
}

func (c *capture) overflowed() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.overflow
}

// drain waits for both readers so a late read error is still reported: joining is
// what makes a delayed error a failure rather than a success with unobserved output.
// A surviving writer keeps the pipes open, which is caught by the group and port
// checks instead of here, so a timeout is not itself a failure.
func (c *capture) drain() error {
	finished := make(chan struct{})
	go func() { c.group.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(drainTimeout):
	}
	c.mutex.Lock()
	c.closing = true
	c.mutex.Unlock()
	for _, reader := range c.readers {
		_ = reader.Close()
	}
	<-finished
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.failure
}

// interrupted reports whether a signal arrived, and which status it carries.
func interrupted(signals <-chan int) (int, bool) {
	if signals == nil {
		return 0, false
	}
	select {
	case status := <-signals:
		return status, true
	default:
		return 0, false
	}
}

// pythonFloat renders a duration the way the retired driver formatted a float
// seconds value, because the deadline diagnostic quotes it.
func pythonFloat(duration time.Duration) string {
	text := strconv.FormatFloat(duration.Seconds(), 'g', -1, 64)
	if strings.ContainsAny(text, ".eEnN") {
		return text
	}
	return text + ".0"
}

// pythonIntList renders a list of pids the way the retired diagnostic did.
func pythonIntList(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// appendJSON writes the report with the spacing `json.dumps(sort_keys=True)`
// produced, because `script/test/dsh-verify.sh` matches `"status": 200` and friends
// as literal bytes.
func (r probeReport) appendJSON(dst []byte) []byte {
	dst = append(dst, '{')
	dst = appendField(dst, "attempts", true)
	dst = strconv.AppendInt(dst, int64(r.Attempts), 10)
	dst = appendField(dst, "cleanup", false)
	dst = append(dst, '{')
	dst = appendField(dst, "execTmpRemoved", true)
	dst = strconv.AppendBool(dst, r.Cleanup.ExecTmpGCD)
	dst = appendField(dst, "portReleased", false)
	dst = strconv.AppendBool(dst, r.Cleanup.PortReleased)
	dst = appendField(dst, "signalled", false)
	dst = appendStringList(dst, r.Cleanup.Signalled)
	dst = appendField(dst, "survivors", false)
	dst = strconv.AppendInt(dst, 0, 10)
	dst = append(dst, '}')
	dst = appendField(dst, "command", false)
	dst = append(dst, strconv.Quote(r.Command)...)
	dst = appendField(dst, "http", false)
	dst = append(dst, '{')
	dst = appendField(dst, "bytes", true)
	dst = strconv.AppendInt(dst, int64(r.Facts.Bytes), 10)
	dst = appendField(dst, "markers", false)
	markers := make([]string, 0, len(probeMarkers))
	for _, marker := range probeMarkers {
		markers = append(markers, string(marker))
	}
	dst = appendStringList(dst, markers)
	dst = appendField(dst, "redirects", false)
	dst = strconv.AppendInt(dst, int64(r.Facts.Redirects), 10)
	dst = appendField(dst, "status", false)
	dst = strconv.AppendInt(dst, int64(r.Facts.Status), 10)
	dst = append(dst, '}')
	dst = appendField(dst, "ownership", false)
	dst = append(dst, '{')
	dst = appendField(dst, "listenerInode", true)
	if r.Ownership.Proven {
		dst = append(dst, "true"...)
	} else {
		dst = append(dst, "null"...)
	}
	if r.Ownership.Proven {
		dst = appendField(dst, "pid", false)
		dst = strconv.AppendInt(dst, int64(r.Ownership.PID), 10)
	} else {
		dst = appendField(dst, "reason", false)
		dst = append(dst, strconv.Quote(r.Ownership.Reason)...)
	}
	dst = append(dst, '}')
	dst = appendField(dst, "port", false)
	dst = strconv.AppendInt(dst, int64(r.Port), 10)
	dst = appendField(dst, "url", false)
	dst = append(dst, strconv.Quote(r.URL)...)
	return append(dst, '}', '\n')
}

func appendField(dst []byte, name string, first bool) []byte {
	if !first {
		dst = append(dst, ',', ' ')
	}
	dst = append(dst, strconv.Quote(name)...)
	return append(dst, ':', ' ')
}

func appendStringList(dst []byte, values []string) []byte {
	dst = append(dst, '[')
	for index, value := range values {
		if index > 0 {
			dst = append(dst, ',', ' ')
		}
		dst = append(dst, strconv.Quote(value)...)
	}
	return append(dst, ']')
}
