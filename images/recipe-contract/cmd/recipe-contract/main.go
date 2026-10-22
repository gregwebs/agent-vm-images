// Command recipe-contract is the single build-helper binary.
//
// It only assembles the process environment and streams and hands them to the
// CLI adapter: no parsing, permission policy or deployment knowledge lives here.
package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/cli"
)

func main() {
	// A failed write to stdout must surface as an errno the adapter can report,
	// not as Go's SIGPIPE process death.
	signal.Ignore(syscall.SIGPIPE)
	os.Exit(cli.Run(os.Args[1:], cli.ProcessEnvironment(), cli.Streams{Out: os.Stdout, Err: cli.DiagnosticStream(os.Stderr)}))
}
