package cli

import (
	"errors"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/legacy"
)

// Legacy help and usage text. The retired `.py` names are frozen on purpose:
// These are compatibility bytes, not current source/delivery documentation.
// See ADR 0004; updating even the help text would change the port contract.
const (
	installStatusUnknown = "install-status.py: unknown subcommand "
	toolAccessUnknown    = "check-tool-access.py: unknown option "
	toolAccessHelp       = "Shared recipe-contract helper: the T5 \"usable by any uid\" command audit.\n\nCanonical source: images/recipe-contract/check-tool-access.py\nBind-mounted directly by images/standard/Dockerfile; no tool-local copies.\n\nUsage: check-tool-access.py [--content | --tree] TARGET [TARGET ...]\n\nEach TARGET is a bare command name (resolved on PATH) or a path. Resolves the\npath component by component, following symlinks, and requires the *all-class*\npredicate on every path that must be traversed:\n\n  * every directory, including `/`, satisfies (mode & 0o111) == 0o111, so a\n    0701 directory that denies a non-owner in its group, or a 0001 executable\n    that denies its owner, both fail;\n  * the final target is a regular file with (mode & 0o111) == 0o111;\n  * if the final target is a script (a `#!` shebang), its content is\n    (mode & 0o444) == 0o444 and its shebang interpreter is audited the same way.\n    The shebang is split the way the Linux kernel splits it: the interpreter is\n    the path up to the first space/tab, and the remainder (trailing spaces/tabs\n    removed) is passed to it as ONE argument. For `/usr/bin/env` that argument\n    is the command name unless it is an `-S` split-string; `sh -e` names the\n    (nonexistent) program `sh -e`, never `sh` with option `-e`. A `PATH`\n    assignment (only reachable through `-S`) selects the effective PATH the\n    interpreter is resolved under (or the auditor's PATH when it makes none);\n    lookup-changing options we do not model are rejected rather than skipped.\n\nWith `--content`, instead of the executable predicate the final target only has\nto be readable by all ((mode & 0o444) == 0o444) and no shebang interpreter is\nresolved. That is how a recipe audits required *data* it ships (a bridge source,\na mandatory extension) whose ancestors must still be traversable by every uid.\n\nWith `--tree`, each TARGET is an explicit directory beneath cwd, never a PATH\ncommand. Every physical directory/file needs all-class read/traverse or read,\nrespectively. Each subordinate symlink hop must stay inside the authorized\nphysical root. Tree access does not certify executables or their interpreters.\n\nThis is deliberately stricter than \"other execute\": Unix permission selection\ndoes not fall through from owner/group to other, so only all-class bits prove\naccess for *any* uid/gid. A present-but-unsuitable command is a failure; a later\nPATH candidate is never substituted for it. Dangling and cyclic links and\nnon-regular final targets are rejected. Symlink permission bits themselves are\nignored (the kernel does not consult them); the target and both the lexical and\nresolved ancestors are audited. Control characters are never treated as\nwhitespace (a CRLF shebang names `/bin/sh\\r`, which does not exist).\n\nA POSIX access ACL on any audited path is rejected: st_mode cannot prove it does\nnot grant an arbitrary uid more than the mode bits suggest.\n"
	installStatusUsage   = "usage: install-status.py {path-state PATH | code FILE | record FILE}\n"
	toolAccessUsage      = "usage: check-tool-access.py [--content | --tree] TARGET [TARGET ...]\n"
	dispatcherUsage      = "usage: recipe-contract {install-status | tool-access | pnpm-lock | pnpm-environment | pnpm-yaml | dsh-lock-update | dsh-cordis-overrides | dsh-upgrade-stage} ...\n"
)

// exceptionLineText renders the final line of the legacy exception report.
func exceptionLineText(err error) string {
	var nullByte *legacy.NullByteError
	if errors.As(err, &nullByte) {
		return "ValueError: " + nullByte.Error()
	}
	if errno, ok := legacy.ErrnoOf(err); ok {
		return legacy.ExceptionLine(errno, "")
	}
	return "OSError: " + err.Error()
}
