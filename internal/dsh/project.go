// Package dsh is the dsh layer's build and verification surface: the bounded web
// readiness probe, the owning install hook, the reviewed-input preparation and the
// installed-payload gate.
//
// The filesystem, manager and publication policy of a dsh prefix belongs to the
// shared pnpm project module (#62), which is still Python. This package therefore
// asks for every rule it needs through that module's own verbs instead of
// restating them: `images/tools/dsh/project.py` is loaded beside the sources and
// run as a child program, so there is one copy of the authorization, scratch,
// manager and publication rules.
package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Refusal is a diagnostic a consumer parses or a build log quotes, so it is
// printed verbatim rather than under this program's own message.
type Refusal struct{ Text string }

func (r *Refusal) Error() string { return r.Text }

// ChildFailure is a program this helper ran that failed on its own. Parity is the
// status, not only the wording: a child that keeps its own status is what stops a
// refusal inside `pnpm` collapsing into a generic 1.
type ChildFailure struct {
	Status int
	// Stderr is what the caller would have seen in the build log. Empty when the
	// predecessor captured the child's stderr into a file instead of inheriting it.
	Stderr string
}

func (f *ChildFailure) Error() string { return fmt.Sprintf("exit status %d", f.Status) }

// Interrupted is a signal that reached this invocation. The status is the shell's
// 128+N so a caller's own signal handling still recognises it.
type Interrupted struct{ Status int }

func (i *Interrupted) Error() string { return fmt.Sprintf("signal %d", i.Status) }

func refusal(text string) error { return &Refusal{Text: text} }

func refusalf(format string, args ...any) error {
	return &Refusal{Text: fmt.Sprintf(format, args...)}
}

// Project is the shared pnpm project module under the dsh consumer. Source is the
// directory holding dsh's adapter (`images/tools/dsh`, the read-only build mount
// in an image); the adapter loads `../pnpm-project.py` beside it.
type Project struct{ Source string }

func (p Project) script() string { return filepath.Join(p.Source, "project.py") }

// Facts is what mutation authorization names for a caller: the base pin is the
// only field the dsh hooks read, and it is a string because it travels through a
// diagnostic and a comparison rather than arithmetic.
type Facts struct {
	Prefix     string `json:"prefix"`
	Root       string `json:"root"`
	BasePin    string `json:"basePin"`
	Production bool   `json:"production"`
	Consumer   string `json:"consumer"`
}

// verb runs one project.py verb and returns its stdout. A failing child keeps its
// own status and its own stderr, because those are the lines the shell entry point
// printed.
func (p Project) verb(ctx context.Context, args ...string) (string, error) {
	script := p.script()
	if !regularFile(script) {
		return "", refusal("dsh project helper is missing at " + script)
	}
	command := exec.CommandContext(ctx, "python3", append([]string{script}, args...)...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), nil
	}
	return "", &ChildFailure{Status: exitStatusOf(err), Stderr: stderr.String()}
}

// Authorize is the mutation authorization: the prefix, the command destination and
// the status destination are all validated before anything is created.
func (p Project) Authorize(ctx context.Context, prefix, command, statusDir string) (Facts, error) {
	args := []string{"authorize", prefix}
	if command != "" {
		args = append(args, "--command", command)
	}
	if statusDir != "" {
		args = append(args, "--status-dir", statusDir)
	}
	out, err := p.verb(ctx, args...)
	if err != nil {
		return Facts{}, err
	}
	var facts Facts
	if err := json.Unmarshal([]byte(out), &facts); err != nil {
		return Facts{}, refusal("authorization returned no base pin")
	}
	if facts.BasePin == "" {
		return Facts{}, refusal("authorization returned no base pin")
	}
	return facts, nil
}

// RuntimeCapability is what runtime authorization names. The probe runs the
// installed command but may never mutate the payload.
type RuntimeCapability struct {
	Prefix     string `json:"prefix"`
	Root       string `json:"root"`
	BasePin    string `json:"basePin"`
	Production bool   `json:"production"`
	Consumer   string `json:"consumer"`
}

// AuthorizeRuntime authorizes a health probe: the prefix, the command and the
// invocation-owned scratch root, all before the web process starts.
func (p Project) AuthorizeRuntime(ctx context.Context, prefix, command, scratchRoot string) (RuntimeCapability, error) {
	out, err := p.verb(ctx, "authorize-runtime", prefix, "--command", command, "--scratch-root", scratchRoot)
	if err != nil {
		return RuntimeCapability{}, err
	}
	var capability RuntimeCapability
	if err := json.Unmarshal([]byte(out), &capability); err != nil {
		return RuntimeCapability{}, refusal("runtime authorization is unreadable: " + err.Error())
	}
	return capability, nil
}

// Guard refuses an injected package-manager environment before any Node process
// starts.
func (p Project) Guard(ctx context.Context, prefixes ...string) error {
	_, err := p.verb(ctx, append([]string{"guard"}, prefixes...)...)
	return err
}

// ContractDir is the authorized directory whose helpers this invocation may
// execute.
func (p Project) ContractDir(ctx context.Context, prefixes ...string) (string, error) {
	out, err := p.verb(ctx, append([]string{"contract-dir"}, prefixes...)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Scratch creates this invocation's private scratch below the capability's parent.
func (p Project) Scratch(ctx context.Context, prefix, parent string) (string, error) {
	args := []string{"scratch", prefix}
	if parent != "" {
		args = append(args, "--parent", parent)
	}
	out, err := p.verb(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Cleanup removes a scratch an earlier invocation created. It re-authorizes the
// path instead of consulting a registry, so it works across a process boundary.
func (p Project) Cleanup(ctx context.Context, path, prefix string) error {
	args := []string{"cleanup", path}
	if prefix != "" {
		args = append(args, "--prefix", prefix)
	}
	_, err := p.verb(ctx, args...)
	return err
}

// Manager is the manager argv the composed project selects: the fixed base
// launcher in an image, an authorized fixture fake under a fixture root.
func (p Project) Manager(ctx context.Context, prefix string) ([]string, error) {
	out, err := p.verb(ctx, "manager", prefix)
	if err != nil {
		return nil, err
	}
	argv := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(argv) == 0 || argv[0] == "" {
		return nil, refusal("authorization returned no manager argv")
	}
	return argv, nil
}

// Publish is the four-input transaction. A status of 3 means the rollback is
// incomplete, which is a distinct outcome: the staged graph and the recovery
// material must survive.
func (p Project) Publish(ctx context.Context, staged, prefix string) error {
	if _, err := p.verb(ctx, "publish", staged, prefix); err != nil {
		return err
	}
	return nil
}

// CommandIdentity proves the first PATH candidate for name is the installed file.
func (p Project) CommandIdentity(ctx context.Context, name, expected string) error {
	_, err := p.verb(ctx, "command-identity", name, expected)
	return err
}

// RemoveInstall removes a partial node_modules so no half-installed agent
// survives a hard failure.
func (p Project) RemoveInstall(ctx context.Context, prefix string) error {
	_, err := p.verb(ctx, "remove-install", prefix)
	return err
}

// exitStatusOf keeps a child's own status. A child stopped by a signal reports
// 128+N, which is what the shell's `set -e` produced.
func exitStatusOf(err error) int {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return 1
	}
	if wait, ok := exit.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
		return 128 + signalNumber(wait)
	}
	if code := exit.ExitCode(); code >= 0 {
		return code
	}
	return 1
}

// Silent is a child that already reported its own refusal: the caller's status is
// the child's, and this program adds nothing to the diagnostic.
type Silent struct{ Status int }

func (s *Silent) Error() string { return "exit status " + strconv.Itoa(s.Status) }
