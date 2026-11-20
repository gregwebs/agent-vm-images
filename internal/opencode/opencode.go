// Package opencode is the OpenCode layer's build surface: reviewed-input
// preparation, the installed-command gate and the installed-plugin gate.
//
// The three shell files that used to carry this policy are entry points now: each
// reads the container's environment, names it as flags and execs the composed
// `recipe-contract` helper, so an image build, a fixture and the finished-image
// certification run one copy of the rules (ADR 0017, #218).
//
// Two things deliberately stay outside this package. The `.mjs` programs are the
// plugin's own runtime, so this driver runs them as children rather than
// reimplementing them: their bytes are what the guest loads. `probe-opencode.py`
// stays Python because its bytes are rendered into the released image's own
// self-certification, which runs with no network, no mounts and no composed helper
// (ADR 0017, #185).
package opencode

import (
	"fmt"
	"strconv"
)

// Refusal is a well-formed request this program refuses: status 1 and one exact
// diagnostic line.
//
// Text is the whole line because the predecessors did not share a prefix:
// `prepare-lock.sh` named itself, the two gates indented under two spaces, and a
// composed guard printed its own. Re-adding a prefix in the CLI would rewrite
// diagnostics a build log already contains.
type Refusal struct{ Text string }

func (r *Refusal) Error() string { return r.Text }

func refusal(text string) error { return &Refusal{Text: text} }

func refusalf(format string, args ...any) error {
	return &Refusal{Text: fmt.Sprintf(format, args...)}
}

// Usage is a malformed invocation: status 2, not a refused request. Text is the
// documented interface line, or empty when the predecessor exited 2 with no
// diagnostic at all.
type Usage struct{ Text string }

func (u *Usage) Error() string {
	if u.Text == "" {
		return "usage"
	}
	return u.Text
}

// Silent is a failure the predecessor reported with no diagnostic of its own, or
// one whose bytes already reached the caller's streams. A child keeps its own
// status and adds nothing here; this type exists so that status survives the trip
// to the process exit without being collapsed into a generic 1.
type Silent struct{ Status int }

func (s *Silent) Error() string { return "exit status " + strconv.Itoa(s.Status) }

// Interrupted is a signal that reached the invocation. The status is the shell's
// 128+N: the predecessors trapped INT, TERM and HUP and exited 130, 143 and 129,
// and a caller's own trap has to keep recognising them.
type Interrupted struct{ Status int }

func (i *Interrupted) Error() string { return "signal " + strconv.Itoa(i.Status) }

// interruptedOr prefers a signal over the failure it caused: an interrupted run
// reports the signal, never the generic failure the teardown produced.
func interruptedOr(watcher *interruption, otherwise error) error {
	if status, interrupted := watcher.exited(); interrupted {
		return &Interrupted{Status: status}
	}
	return otherwise
}
