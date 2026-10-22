package cli

import (
	"errors"
	"strings"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/toolaccess"
)

// runToolAccess parses the audit argv in one ordered pass: help returns at its
// position, an earlier unknown option wins, and repeated flags are idempotent.
func (p *process) runToolAccess(args []string, env toolaccess.Environment) int {
	quiet, content, tree := false, false, false
	var targets []toolaccess.Target
	for _, arg := range args {
		switch {
		case arg == "--quiet":
			quiet = true
		case arg == "--content":
			content = true
		case arg == "--tree":
			tree = true
		case arg == "-h" || arg == "--help":
			return p.stdoutStatus(toolAccessHelp)
		case strings.HasPrefix(arg, "-"):
			return p.stderrStatus(2, toolAccessUnknown+arg+"\n")
		default:
			targets = append(targets, toolaccess.Target(arg))
		}
	}
	if len(targets) == 0 || (content && tree) {
		return p.stderrStatus(2, toolAccessUsage)
	}
	mode := toolaccess.Executable
	switch {
	case tree:
		mode = toolaccess.Tree
	case content:
		mode = toolaccess.Content
	}
	auditor := toolaccess.NewAuditor(env)
	for _, target := range targets {
		final, err := auditor.Audit(target, mode)
		if err != nil {
			var failure *toolaccess.Violation
			if !errors.As(err, &failure) {
				// An internal legacy failure (an unreadable cwd, a readlink race,
				// a path with a NUL byte). The source-frame traceback is covered by
				// the ADR 0004 exception; the exception line and status are not.
				return p.reportSynchronousFailure(err)
			}
			return p.stderrStatus(1, "tool-access: "+failure.Path+": "+failure.Reason()+"\n")
		}
		if !quiet {
			if status := p.stdoutStatus("tool-access: ok " + string(final) + "\n"); status != 0 {
				return status
			}
		}
	}
	return 0
}
