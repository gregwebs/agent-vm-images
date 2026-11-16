// Package toolaccess owns the "usable by any uid" audit: path resolution,
// all-class permission predicates, shebang interpreters and tree containment.
//
// It knows nothing about argv, streams or process exit; the CLI adapter owns
// those. Every violation carries the legacy reason text so that the adapter only
// has to add the stream prefix.
package toolaccess

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// MaxSymlinks bounds a symlink chain. The kernel's limit is 40 followed links;
// the 41st is a cycle or an unusable chain.
const MaxSymlinks = 40

// MaxInterpreterDepth bounds nested script interpreters: a chain of five scripts
// runs, a chain of six fails.
const MaxInterpreterDepth = 5

// Mode is the closed audit predicate set.
type Mode uint8

const (
	// Executable audits a command the way a build would execute it.
	Executable Mode = iota
	// Content audits data: all-class readable, no shebang interpreter.
	Content
	// Tree audits a directory subtree beneath cwd.
	Tree
)

// Target is one user-supplied audit subject, still raw argv bytes.
type Target string

// ResolvedPath is the physical path an audit certified.
type ResolvedPath string

// PathValue is a PATH value to resolve bare command names against.
type PathValue string

// PhysicalDirectory labels the physical cwd provided by process assembly.
// PhysicalDirectoryOf trusts that caller's syscall result, not a lexical PWD.
type PhysicalDirectory struct{ path string }

// PhysicalDirectoryOf is the single constructor: the syscall boundary belongs to
// process assembly, never to the audit.
func PhysicalDirectoryOf(path string) PhysicalDirectory { return PhysicalDirectory{path: path} }

// String returns the physical path the resolver walks from.
func (d PhysicalDirectory) String() string { return d.path }

// DirectoryProvider resolves the physical working directory on demand. It is a
// function, not a value, because a relative target may never need it and a
// removed cwd must not break help, usage or absolute-only audits.
type DirectoryProvider func() (PhysicalDirectory, error)

// Environment is everything the audit reads from the process.
type Environment struct {
	WorkingDirectory DirectoryProvider
	Path             PathValue
	// ReadDirectory defaults to native unsorted enumeration. Tests can bound
	// filesystem traversal without changing a process-global function.
	ReadDirectory func(string) ([]string, error)
}

// Violation is a rule failure: a path plus the legacy reason text.
type Violation struct {
	Path   string
	reason string
}

func (v *Violation) Error() string { return v.Path + ": " + v.reason }

// Reason renders the legacy reason suffix.
func (v *Violation) Reason() string { return v.reason }

func violation(path, reason string) *Violation {
	return &Violation{Path: path, reason: reason}
}

// Auditor carries the state shared by the targets of one invocation.
type Auditor struct {
	env       Environment
	active    map[visitKey]struct{}
	completed map[visitKey]struct{}
}

// NewAuditor starts one batch. Interpreter memoisation never crosses batches,
// matching a single process invocation.
func NewAuditor(env Environment) *Auditor {
	if env.ReadDirectory == nil {
		env.ReadDirectory = readDirNative
	}
	return &Auditor{
		env:       env,
		active:    make(map[visitKey]struct{}),
		completed: make(map[visitKey]struct{}),
	}
}

// Audit runs one target under the requested predicate.
func (a *Auditor) Audit(target Target, mode Mode) (ResolvedPath, error) {
	switch mode {
	case Executable:
		final, err := a.auditExecutable(string(target), noPathOverride, 0)
		return ResolvedPath(final), err
	case Content:
		final, err := a.auditContent(string(target))
		return ResolvedPath(final), err
	case Tree:
		final, err := a.auditTree(string(target))
		return ResolvedPath(final), err
	default:
		return "", fmt.Errorf("toolaccess: unknown mode %d", mode)
	}
}

func (a *Auditor) physicalCwd() (string, error) {
	directory, err := a.env.WorkingDirectory()
	if err != nil {
		return "", err
	}
	return directory.String(), nil
}

func (a *Auditor) auditExecutable(command string, envPath pathOverride, depth int) (string, error) {
	final, info, err := a.resolveAndAuditDirs(command, envPath)
	if err != nil {
		return "", err
	}
	mode := unixMode(info)
	if !info.Mode().IsRegular() {
		return "", violation(final, "final target is not a regular file")
	}
	if mode&allExecute != allExecute {
		return "", violation(final, fmt.Sprintf("not executable by all (mode %o)", mode&permissionBits))
	}
	if err := checkACL(final); err != nil {
		return "", err
	}

	// The key carries the effective PATH as well as the final path: the same
	// interpreter reached under a different inherited PATH must be re-resolved.
	key := visitKey{final: final, envPath: envPath}
	if _, active := a.active[key]; active {
		return "", violation(final, "interpreter cycle (already being audited)")
	}
	if _, done := a.completed[key]; done {
		return final, nil
	}

	head, err := readHead(final)
	if err != nil {
		return "", err
	}
	interpreters, err := parseShebang(head, envPath, final)
	if err != nil {
		return "", err
	}
	if len(interpreters) > 0 {
		if mode&allRead != allRead {
			return "", violation(final, fmt.Sprintf("script is not readable by all (mode %o)", mode&permissionBits))
		}
		if depth >= MaxInterpreterDepth {
			return "", violation(final, fmt.Sprintf("interpreter nesting exceeds %d scripts", MaxInterpreterDepth))
		}
		a.active[key] = struct{}{}
		defer delete(a.active, key)
		for _, interpreter := range interpreters {
			if _, err := a.auditExecutable(interpreter.command, interpreter.envPath, depth+1); err != nil {
				return "", err
			}
		}
		a.completed[key] = struct{}{}
	}
	return final, nil
}

func (a *Auditor) auditContent(command string) (string, error) {
	final, info, err := a.resolveAndAuditDirs(command, noPathOverride)
	if err != nil {
		return "", err
	}
	mode := unixMode(info)
	if !info.Mode().IsRegular() {
		return "", violation(final, "final target is not a regular file")
	}
	if mode&allRead != allRead {
		return "", violation(final, fmt.Sprintf("not readable by all (mode %o)", mode&permissionBits))
	}
	return final, checkACL(final)
}

func (a *Auditor) resolveAndAuditDirs(command string, envPath pathOverride) (string, os.FileInfo, error) {
	target, err := a.resolveCommand(command, envPath)
	if err != nil {
		return "", nil, err
	}
	resolver := &resolver{cwd: a.physicalCwd}
	final, info, err := resolver.resolve(target, finalComponent)
	if err != nil {
		return "", nil, err
	}
	resolver.addDir("/")
	if err := resolver.auditDirs(); err != nil {
		return "", nil, err
	}
	return final, info, nil
}

// readHead returns at most 4096 bytes, mirroring the legacy read that keeps a
// shebang line honest while refusing to load a whole binary.
func readHead(path string) ([]byte, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, openViolation(path, err)
	}
	defer func() { _ = handle.Close() }()
	head := make([]byte, 4096)
	total := 0
	for total < len(head) {
		n, err := handle.Read(head[total:])
		total += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, readViolation(path, err)
		}
		if n == 0 {
			break
		}
	}
	return head[:total], nil
}
