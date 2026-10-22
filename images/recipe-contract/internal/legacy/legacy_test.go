package legacy

import (
	"errors"
	"fmt"
	"strconv"
	"syscall"
	"testing"
)

func TestBackslashReplaceEncodesStreamsDifferently(t *testing.T) {
	t.Parallel()
	raw := "a\xffb"
	if got := string(RawIdentity(raw)); got != raw {
		t.Fatalf("stdout encoding changed the bytes: %q", got)
	}
	if got := string(BackslashReplace(raw)); got != `a\udcffb` {
		t.Fatalf("stderr encoding = %q, want a\\udcffb", got)
	}
	if got := string(BackslashReplace("héllo\n")); got != "héllo\n" {
		t.Fatalf("valid UTF-8 and ASCII changed: %q", got)
	}
}

func TestReprMatchesPython(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain":          `'plain'`,
		"it's":           `"it's"`,
		`both'"`:         `'both\'"'`,
		"tab\there":      `'tab\there'`,
		"nl\nhere":       `'nl\nhere'`,
		"back\\slash":    `'back\\slash'`,
		"\x00\x7f":       `'\x00\x7f'`,
		"é":              `'é'`,
		"a\xffb":         `'a\udcffb'`,
		"\u2003":         `'\u2003'`,
		"--bad-\xff":     `'--bad-\udcff'`,
		"quote'and\"q\"": `'quote\'and"q"'`,
	}
	for input, want := range cases {
		if got := Repr(input); got != want {
			t.Fatalf("Repr(%q) = %s, want %s", input, got, want)
		}
	}
	// decode("utf-8", "replace") yields a printable U+FFFD, so repr keeps the
	// character itself rather than an escape.
	if got := ReprReplacing("--bad-\xff"); got != "'--bad-\uFFFD'" {
		t.Fatalf("ReprReplacing = %s", got)
	}
}

func TestOSErrorTextUsesCStrerrorAndRepr(t *testing.T) {
	t.Parallel()
	if got := OSErrorText(syscall.ENOSPC, ""); got != "[Errno 28] No space left on device" {
		t.Fatalf("errno text = %q", got)
	}
	if got := OSErrorText(syscall.EACCES, "/a b\xff"); got != "[Errno 13] Permission denied: '/a b\\udcff'" {
		t.Fatalf("filename repr = %q", got)
	}
	if got := ExceptionLine(syscall.EPIPE, ""); got != "BrokenPipeError: [Errno 32] Broken pipe" {
		t.Fatalf("exception line = %q", got)
	}
}

func TestErrnoOfUnwrapsPathErrors(t *testing.T) {
	t.Parallel()
	errno, ok := ErrnoOf(&testPathError{})
	if !ok || errno != syscall.ENOENT {
		t.Fatalf("ErrnoOf = %v, %v", errno, ok)
	}
}

type testPathError struct{}

func (e *testPathError) Error() string { return "open: no such file" }
func (e *testPathError) Unwrap() error { return syscall.ENOENT }

// TestReplaceInvalidMatchesPythonReplaceDecoding pins CPython's maximal-subpart
// rule, measured with 3.13.5 (`bytes.decode("utf-8", "replace")`). One U+FFFD per
// run and one per byte are both wrong: the truncated prefix stays one replacement
// and an encoded surrogate is three.
func TestReplaceInvalidMatchesPythonReplaceDecoding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		input    string
		replaced string
		repr     string
	}{
		{name: "two invalid start bytes", input: "\xff\xff", replaced: "\uFFFD\uFFFD", repr: `'��'`},
		{name: "truncated three-byte then invalid", input: "\xe2\x80\xff", replaced: "\uFFFD\uFFFD", repr: `'��'`},
		{name: "truncated three-byte then ascii", input: "\xe2\x80A", replaced: "\uFFFDA", repr: `'�A'`},
		{name: "truncated four-byte then invalid", input: "\xf0\x9f\x98\xff", replaced: "\uFFFD\uFFFD", repr: `'��'`},
		{name: "encoded surrogate", input: "-\xed\xa0\x80", replaced: "-\uFFFD\uFFFD\uFFFD", repr: `'-���'`},
		{name: "truncated two-byte prefix", input: "\xe2\x82", replaced: "\uFFFD", repr: `'�'`},
		{name: "truncated four-byte prefix", input: "\xf0\x9f", replaced: "\uFFFD", repr: `'�'`},
		{name: "overlong prefix then ascii", input: "\xc3\x28", replaced: "\uFFFD(", repr: `'�('`},
		{name: "above unicode maximum", input: "\xf4\x90\x80\x80", replaced: "\uFFFD\uFFFD\uFFFD\uFFFD", repr: `'����'`},
		{name: "truncated surrogate prefix", input: "\xed\xa0", replaced: "\uFFFD\uFFFD", repr: `'��'`},
		{name: "valid utf-8 untouched", input: "héllo", replaced: "héllo", repr: `'héllo'`},
	}
	for _, test := range cases {
		if got := ReplaceInvalid(test.input); got != test.replaced {
			t.Fatalf("%s: ReplaceInvalid(%q) = %q, want %q", test.name, test.input, got, test.replaced)
		}
		if got := ReprReplacing(test.input); got != test.repr {
			t.Fatalf("%s: ReprReplacing(%q) = %s, want %s", test.name, test.input, got, test.repr)
		}
	}
}

// TestExceptionClassMatchesCPython pins CPython's OSError subclass table
// (Objects/exceptions.c) for the errnos the table covers, plus two errnos that
// deliberately stay a plain OSError. Measured against CPython 3.13.5.
func TestExceptionClassMatchesCPython(t *testing.T) {
	t.Parallel()
	cases := map[syscall.Errno]string{
		syscall.EPERM:        "PermissionError",
		syscall.EACCES:       "PermissionError",
		syscall.ENOENT:       "FileNotFoundError",
		syscall.ESRCH:        "ProcessLookupError",
		syscall.EINTR:        "InterruptedError",
		syscall.ECHILD:       "ChildProcessError",
		syscall.EAGAIN:       "BlockingIOError",
		syscall.EALREADY:     "BlockingIOError",
		syscall.EINPROGRESS:  "BlockingIOError",
		syscall.EEXIST:       "FileExistsError",
		syscall.ENOTDIR:      "NotADirectoryError",
		syscall.EISDIR:       "IsADirectoryError",
		syscall.EPIPE:        "BrokenPipeError",
		syscall.ESHUTDOWN:    "BrokenPipeError",
		syscall.ECONNABORTED: "ConnectionAbortedError",
		syscall.ECONNREFUSED: "ConnectionRefusedError",
		syscall.ECONNRESET:   "ConnectionResetError",
		syscall.ETIMEDOUT:    "TimeoutError",
		syscall.EIO:          "OSError",
		syscall.ENOSPC:       "OSError",
		syscall.ENAMETOOLONG: "OSError",
		syscall.ELOOP:        "OSError",
	}
	for errno, want := range cases {
		if got := ExceptionLine(errno, ""); got != want+": [Errno "+strconv.Itoa(int(errno))+"] "+strerror(errno) {
			t.Fatalf("exception line for %v = %q, want class %s", errno, got, want)
		}
	}
}

// TestCheckPathReproducesNullByteValueError pins the class CPython raises before
// the syscall, which is a ValueError and so not an OSError line at all.
func TestCheckPathReproducesNullByteValueError(t *testing.T) {
	t.Parallel()
	if err := CheckPath("lstat", "/bi\x00n"); err == nil || err.Error() != "lstat: embedded null character in path" {
		t.Fatalf("NUL path check = %v", err)
	}
	if err := CheckPath("readlink", "/bin/sh"); err != nil {
		t.Fatalf("a NUL-free path was rejected: %v", err)
	}
}

func TestErrnoOfWrappedAndJoinedErrors(t *testing.T) {
	err := errors.Join(errors.New("other"), fmt.Errorf("wrapped: %w", syscall.EIO))
	if got, ok := ErrnoOf(err); !ok || got != syscall.EIO {
		t.Fatalf("errno = %v, %v", got, ok)
	}
}
