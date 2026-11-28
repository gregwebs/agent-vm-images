// Package publication is the shared publication transaction the pin writers use.
//
// It is deliberately not part of any one writer: the artifact writer and the Claude
// writer publish different files through exactly the same phases and statuses, and a
// maintainer must be able to reason about "what did a failed pin write touch" once.
//
// # The six signal windows
//
// One process owns every window, from the first backup to the exit, so a signal can
// never be answered by a second owner that read the transaction halfway. What a
// caller may rely on after each window:
//
//	P1  mid-copy                      every destination publication wrote is
//	                                  restored; the complete backup set is retained
//	                                  and printed; 128+N.
//	P2  while the backups are taken   no destination was written, none was restored,
//	                                  and nothing is retained; 128+N.  See the ruling
//	                                  below: this window's status changed.
//	P3  copies done, a restore failed backups, recovery record and owner scratch are
//	                                  retained and printed; 3.
//	P4  copies done, outcome not yet  the publication is left in place; backups,
//	    taken                         recovery record and owner scratch are retained
//	                                  and printed; 3.
//	P5  outcome taken, not reported   no success line is printed; the publication is
//	                                  left in place; material retained and printed; 3.
//	P6  scratch removal fails after a the decided status stands (0 for a completed
//	    decided 0                     publication), the report is printed, and the
//	                                  leftover is named once as a leftover, never as
//	                                  recovery material (#138).
//
// # R1: the backup-phase window reports 128+N and retains nothing, by ruling
//
// P2's status changed, and it changed by the owner's ruling A on #213 (comment on
// that issue): a signal during the backup phase exits 128+N with the sources
// unchanged and nothing retained. The shell publisher's opposite branch — a partial
// backup set kept and status 3 — is its bug rather than its contract: 3 means
// "undecided, or an incomplete rollback" (CONTRIBUTING.md), and neither holds when no
// destination was written.
//
// No other window moved by a ruling. P4 and P5 changed from 128+N to 3, and P1 kept
// its status but now retains the backups it restored from, because the merged Go
// engine contradicted #213's proofs: it restored a completed publication, reported a
// signal over a taken outcome, and deleted the backups a restore had used.
//
// Statuses, unchanged: 0 published, 1 failed with the sources unchanged or fully
// restored, 2 usage, 3 undecided or incomplete recovery (the work directory and the
// caller's scratch are retained and printed), 128+N after a signal.
package publication

import (
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
// already decided otherwise, and a signal that arrives between the decision and the
// report must leave the outcome undecided rather than print it.
type Report struct {
	To    io.Writer
	Lines []string
}

// Request is one publication.
type Request struct {
	// Prefix is the writer's contract prefix on every diagnostic line, and on every
	// report line.
	Prefix string
	// Root is the repository the destinations are relative to.
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
	// Owner is the caller's own scratch directory. The publication removes it when
	// the outcome is decided, and otherwise retains it and prints it beside its own
	// work directory so a maintainer recovers both together.
	Owner string
	Files []Staged
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
// directory is retained and printed), 128+N after a signal whose rollback
// succeeded. It ends the process itself on every decided outcome, so the status it
// returns is only reached by a test's substituted exit.
func Publish(request Request) int {
	pattern := request.TempPrefix
	if pattern == "" {
		pattern = "pin-publish."
	}
	work, err := os.MkdirTemp(request.TempRoot, pattern)
	if err != nil {
		if request.Stderr != nil {
			_, _ = request.Stderr.WriteString(request.Prefix +
				"cannot create a work directory (" + err.Error() + "); sources unchanged\n")
		}
		return 1
	}
	if absolute, err := filepath.Abs(work); err == nil {
		work = absolute
	}
	transaction := &Transaction{
		Prefix: request.Prefix,
		Root:   request.Root,
		Work:   work,
		Stderr: request.Stderr,
		Report: request.Report,
	}
	for _, entry := range request.Files {
		transaction.Destinations = append(transaction.Destinations,
			filepath.Join(request.Root, entry.Path))
		transaction.Staged = append(transaction.Staged, entry.Raw)
	}
	return transaction.Run(request.Owner)
}
