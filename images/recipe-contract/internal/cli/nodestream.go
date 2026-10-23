package cli

import (
	"fmt"
	"io"
)

// The ported subcommands bypass the Python stream emulation: their contract is a
// single payload on stdout, no stdout on failure, and one prefixed diagnostic on
// stderr. That is a plain write, so it is written plainly.
func (p *process) nodeUsage(status int, text string) int {
	if _, err := io.WriteString(p.streams.Err, text); err != nil {
		return 1
	}
	return status
}

func (p *process) nodeDiagnostic(prefix string, err error) int {
	// A stdout write failure becomes a diagnostic, never a success with dropped
	// output (the retired helper's async EPIPE crash was D12).
	if _, writeErr := io.WriteString(p.streams.Err, prefix+err.Error()+"\n"); writeErr != nil {
		return 1
	}
	return 1
}

// nodeCommand runs one ported helper. A panic is converted into the same status 1
// diagnostic the retired try/catch produced, so "any failure is status 1" holds
// even for an internal error.
func (p *process) nodeCommand(prefix string, action func() (payload string, err error)) (status int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			status = p.nodeDiagnostic(prefix, fmt.Errorf("internal error: %v", recovered))
		}
	}()
	payload, err := action()
	if err != nil {
		return p.nodeDiagnostic(prefix, err)
	}
	if payload == "" {
		return 0
	}
	if _, err := io.WriteString(p.streams.Out, payload); err != nil {
		return p.nodeDiagnostic(prefix, err)
	}
	return 0
}
