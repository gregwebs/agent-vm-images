// Package publication is the shared publication transaction the pin writers use.
//
// It is deliberately not part of any one writer: the artifact writer and the Claude
// writer publish different files through exactly the same phases and statuses, and a
// maintainer must be able to reason about "what did a failed pin write touch" once.
//
// # The signal windows
//
// One process owns every window, from the first backup to the exit, so a signal can
// never be answered by a second owner that read the transaction halfway. What a
// caller may rely on after each window:
//
//	P1  mid-copy                      every destination publication wrote is
//	                                  restored; the complete backup set is retained
//	                                  and printed; 128+N.
//	P2  while the backups are taken   no destination was written, none was restored,
//	                                  and nothing is retained; 128+N.  This window's
//	                                  status is the owner's ruling, below.
//	P3  copies done, a restore failed backups, recovery record and owner scratch are
//	                                  retained and printed; 3.
//	P4  copies done, outcome not yet  the publication is left in place; backups,
//	    taken                         recovery record and owner scratch are retained
//	                                  and printed; 3.
//	P5  outcome taken, not reported   no success line is printed; the publication is
//	                                  left in place; material retained and printed; 3.
//	   (also: the signal arrives
//	    while the report is being
//	    written)
//	P6  scratch removal fails after a the decided status stands (0 for a completed
//	    decided 0                     publication), the report is printed, and the
//	                                  leftover is named once as a leftover, never as
//	                                  recovery material (#138).
//
// A signal that arrives during the ordinary rollback of a failed publication takes
// that same window and leaves 128+N with the material retained, rather than the
// status the rollback was about to report; a signal that arrives once the outcome is
// committed and its directories are being removed is answered with the decided
// status, because nothing is left to retain and the discard is what the status says.
//
// # R1: the backup-phase window reports 128+N and retains nothing, by ruling
//
// P2 is the window this ruling names, and it changes nothing in the Go owner, which
// already behaved this way. It changes the contract relative to the shell publisher:
// the owner ruled A on #213 (comment on that issue), so a signal during the backup
// phase exits 128+N with the sources unchanged and nothing retained, where the shell
// keeps a partial backup set and exits 3. That branch of the shell is its bug rather
// than its contract: 3 means "undecided, or an incomplete rollback"
// (CONTRIBUTING.md), and neither holds when no destination was written.
//
// The other windows moved without a ruling, because the merged Go engine contradicted
// #213's proofs: P4 and P5 reported 128+N over a completed or already-taken outcome
// and now report 3, and P1 kept its status but deleted the backups it had just
// restored from and now retains them.
//
// Statuses, unchanged: 0 published, 1 failed with the sources unchanged or fully
// restored, 2 usage, 3 undecided or incomplete recovery (the work directory, the
// caller's scratch and every caller-named retained directory are retained and
// printed), 128+N after a signal.
package publication

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Staged is one destination's proposed bytes, with the destination relative to the
// repository root.
type Staged struct {
	Path string
	Raw  []byte
}

// Report is what a caller wants printed once the outcome is decided, and where.
//
// It travels in the Request rather than being printed by the caller after Publish
// returns: a report written by the caller can say "published" after a signal has
// already decided otherwise, and a signal that arrives while the report is being
// written must leave the outcome undecided rather than exit 0.
type Report struct {
	To    io.Writer
	Lines []string
}

// Request is one publication.
type Request struct {
	// Prefix is the writer's contract prefix on every diagnostic line, and on every
	// report and retention line.
	Prefix string
	// Root is the repository the destinations are relative to. The transaction
	// resolves it once (EvalSymlinks) before anything is written: a root reached
	// through a symlink is the same repository, and a destination compared against the
	// unresolved spelling would look like a path outside it.
	Root string
	// TempRoot is where the work directory is created. Empty means the system
	// default.
	TempRoot string
	// TempPrefix names the work directory. It is the writer's own name so a
	// maintainer looking at a retained directory knows which tool to re-read, and
	// the suites find it by that name.
	TempPrefix string
	// Stderr receives the contract lines; a signal handler writes here too, so it
	// is a file rather than an arbitrary writer.
	Stderr *os.File
	// Report is the success lines and their destination: they describe an outcome
	// only the publication knows it took.
	Report Report
	// Owner is a directory this invocation created for the publication. The
	// publication removes it when the outcome is decided, and otherwise retains it
	// and prints it beside its own work directory so a maintainer recovers both
	// together. It must never name a directory the caller was given: a scratch this
	// transaction did not create is not its to remove.
	Owner string
	// Retain names directories the caller must keep but did not create. They are never
	// removed, they are printed (labelled, with their absolute paths) only when the
	// outcome is retained, and a decided outcome does not print them.
	Retain []Retention
	Files  []Staged
}

// Retention is one directory the caller must keep but did not create, and the label
// the contract line calls it by — the stage a maintainer handed to `--publish-stage`
// is "staged proposal", and the transaction may not guess that.
type Retention struct {
	Label string
	Path  string
}

// StderrOf is the *os.File a signal handler can still write to. A caller that gave
// the writer a buffer (the tests) gets no signal output, which is why the handler
// writes through the same file the contract lines use.
func StderrOf(errOut any) *os.File {
	file, ok := errOut.(*os.File)
	if !ok {
		return nil
	}
	return file
}

// Publish runs one publication and returns the process status: 0 published, 1
// failed with the sources unchanged or restored, 3 incomplete recovery (the work
// directory, the caller's scratch and its retained directories are retained and
// printed), 128+N after a signal whose rollback succeeded. It ends the process
// itself on every decided outcome, so the status it returns is only reached by a
// test's substituted exit.
func Publish(request Request) int {
	pattern := request.TempPrefix
	if pattern == "" {
		pattern = "pin-publish."
	}
	work, err := os.MkdirTemp(request.TempRoot, pattern)
	if err != nil {
		diagnoseRequest(request, "cannot create a work directory (%s); sources unchanged", err)
		discardOwner(request)
		return 1
	}
	if absolute, err := filepath.Abs(work); err == nil {
		work = absolute
	}
	return request.transaction(work).Run(request.Owner)
}

// transaction is the one mapping from a caller's request to the transaction that
// runs it, so nothing the caller named — its root, its report, a directory it must
// keep — is dropped on the way in.
func (request Request) transaction(work string) *Transaction {
	transaction := &Transaction{
		Prefix: request.Prefix,
		Root:   request.Root,
		Work:   work,
		Stderr: request.Stderr,
		Report: request.Report,
		Retain: request.Retain,
	}
	for _, entry := range request.Files {
		transaction.Destinations = append(transaction.Destinations,
			filepath.Join(request.Root, entry.Path))
		transaction.Staged = append(transaction.Staged, entry.Raw)
	}
	return transaction
}

func diagnoseRequest(request Request, format string, args ...any) {
	if request.Stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(request.Stderr, request.Prefix+format+"\n", args...)
}

// discardOwner removes the caller's scratch when the publication never started. The
// caller handed the directory over, so leaving it behind would be a leak the caller
// no longer knows to clean; a removal that fails is named, not hidden.
func discardOwner(request Request) {
	if request.Owner == "" {
		return
	}
	if err := os.RemoveAll(request.Owner); err != nil {
		diagnoseRequest(request, "owner scratch cleanup failed (%s); %s left behind", err,
			request.Owner)
	}
}
