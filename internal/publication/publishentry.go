// Package publication is the shared publication transaction the pin writers use.
//
// It is deliberately not part of any one writer: the artifact writer and the Claude
// writer publish different files through exactly the same phases and statuses, and a
// maintainer must be able to reason about "what did a failed pin write touch" once.
package publication

import (
	"os"
	"path/filepath"
)

// Staged is one destination's proposed bytes, with the destination relative to the
// repository root.
type Staged struct {
	Path string
	Raw  []byte
}

// Request is one publication.
type Request struct {
	// Prefix is the writer's contract prefix on every diagnostic line.
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
	// Owner names the caller's own scratch directory, prints beside a retained
	// transaction so a maintainer recovers both together, and otherwise says
	// nothing.
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
// succeeded.
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
	}
	for _, entry := range request.Files {
		transaction.Destinations = append(transaction.Destinations,
			filepath.Join(request.Root, entry.Path))
		transaction.Staged = append(transaction.Staged, entry.Raw)
	}
	return transaction.Run(request.Owner)
}
