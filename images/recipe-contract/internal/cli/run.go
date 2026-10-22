package cli

import (
	"io"
	"os"
	"syscall"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/toolaccess"
)

// DiagnosticStream is the stderr the legacy contract describes. CPython built
// sys.stderr once at startup and left it None when file descriptor 2 was closed,
// so the interpreter dropped diagnostics instead of failing to write them; a
// stream that exists but rejects the bytes is the 120 case instead. Modelling
// the two apart here keeps the write path free of errno special cases.
func DiagnosticStream(stream *os.File) io.Writer {
	if _, err := stream.Stat(); err != nil {
		return io.Discard
	}
	return stream
}

// Streams names the independently encoded process streams.
type Streams struct{ Out, Err io.Writer }

// ProcessEnvironment assembles the lazy physical cwd and inherited PATH used
// by both the command and its process-level tests.
func ProcessEnvironment() toolaccess.Environment {
	return toolaccess.Environment{
		WorkingDirectory: func() (toolaccess.PhysicalDirectory, error) {
			// os.Getwd may trust lexical PWD; the legacy getcwd never did.
			directory, err := syscall.Getwd()
			return toolaccess.PhysicalDirectoryOf(directory), err
		},
		Path: toolaccess.PathValue(os.Getenv("PATH")),
	}
}

// Run executes one recipe-contract invocation and returns the process status.
// It is the only place that turns argv, streams and exit statuses into the
// legacy contract; the command layers below it return plain errors.
func Run(args []string, env toolaccess.Environment, streams Streams) int {
	process := &process{out: newStdout(streams.Out), err: newStderr(streams.Err)}
	status := process.dispatch(args, env)
	if err := process.out.flush(); err != nil {
		process.reportDeferredFlushFailure("sys.stdout", err)
		status = streamFailureStatus
	}
	if err := process.err.flush(); err != nil {
		status = streamFailureStatus
	}
	return status
}

type process struct {
	out *pythonStream
	err *pythonStream
}

func (p *process) dispatch(args []string, env toolaccess.Environment) int {
	if len(args) == 0 {
		return p.stderrStatus(2, dispatcherUsage)
	}
	switch args[0] {
	case "install-status":
		return p.runInstallStatus(args[1:])
	case "tool-access":
		return p.runToolAccess(args[1:], env)
	default:
		return p.stderrStatus(2, dispatcherUsage)
	}
}

// stderrStatus prints a fixed diagnostic and keeps the requested status, or 120
// when stderr itself cannot accept it.
func (p *process) stderrStatus(status int, text string) int {
	if err := p.err.writeString(text); err != nil {
		return streamFailureStatus
	}
	return status
}

// stdoutStatus prints to stdout. A synchronous write failure aborts processing
// with the legacy status and an exception line on stderr; the Python source-frame
// traceback is the one documented byte difference (ADR 0004).
func (p *process) stdoutStatus(text string) int {
	if err := p.out.writeString(text); err != nil {
		return p.reportSynchronousFailure(err)
	}
	return 0
}

// reportSynchronousFailure reports a stdout write failure and returns the status
// the legacy helper produced. When the report itself cannot reach stderr the
// interpreter failed its exit flush and returned 120, so the status the caller
// expected may not replace that failure.
func (p *process) reportSynchronousFailure(err error) int {
	if writeErr := p.err.writeString(exceptionLineText(err) + "\n"); writeErr != nil {
		return streamFailureStatus
	}
	return synchronousFailureStatus
}

func (p *process) reportDeferredFlushFailure(streamName string, err error) {
	_ = p.err.writeString("Exception ignored on flushing " + streamName + ":\n" + exceptionLineText(err) + "\n")
}
