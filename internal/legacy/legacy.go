// Package legacy reproduces the byte-level rendering rules the retired Python
// recipe-contract helpers exposed on their streams.
//
// The helpers ran under the maintained image's UTF-8 environment: stdout was
// encoded with the surrogateescape error handler, stderr with backslashreplace.
// Both matter for non-UTF8 path bytes. Go strings hold raw bytes here, and the
// renderers decode them the way Python's filesystem decoding did, so a raw 0xff
// byte stays a raw byte on stdout and becomes `\udcff` on stderr.
// See docs/adr/0004-go-build-helpers.md.
package legacy

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

// RawIdentity is the stdout encoding: Python's surrogateescape round trip is the
// identity on the bytes this program carries.
func RawIdentity(text string) []byte { return []byte(text) }

// BackslashReplace mirrors str.encode("utf-8", "backslashreplace") over a string
// that was decoded with surrogateescape: valid UTF-8 passes through, and every
// invalid byte becomes its `\udcXX` escape.
func BackslashReplace(text string) []byte {
	encoded := make([]byte, 0, len(text))
	for index := 0; index < len(text); {
		r, size := utf8.DecodeRuneInString(text[index:])
		if r == utf8.RuneError && size == 1 {
			encoded = append(encoded, fmt.Sprintf("\\u%04x", surrogateBase+uint32(text[index]))...)
			index++
			continue
		}
		encoded = append(encoded, text[index:index+size]...)
		index += size
	}
	return encoded
}

// Repr mirrors Python's repr(str), including its quote choice, its
// non-printable escaping and its `\udcXX` escapes for invalid bytes.
func Repr(text string) string {
	quote := byte('\'')
	if strings.ContainsRune(text, '\'') && !strings.ContainsRune(text, '"') {
		quote = '"'
	}
	var out strings.Builder
	out.WriteByte(quote)
	for index := 0; index < len(text); {
		r, size := utf8.DecodeRuneInString(text[index:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&out, `\u%04x`, surrogateBase+uint32(text[index]))
			index++
			continue
		}
		out.WriteString(escapeRune(r, quote))
		index += size
	}
	out.WriteByte(quote)
	return out.String()
}

// ReprReplacing mirrors `repr(token.decode("utf-8", "replace"))`: the invalid
// input becomes U+FFFD before the repr sees it.
func ReprReplacing(text string) string {
	return Repr(ReplaceInvalid(text))
}

// ReplaceInvalid mirrors `bytes.decode("utf-8", "replace")`: exactly one U+FFFD
// per maximal ill-formed subpart, which is not one per byte and not one per run.
// CPython's UTF-8 decoder consumes the longest prefix of a well-formed sequence
// (so a truncated `\xe2\x82` is one replacement) and stops at the first byte that
// breaks the sequence's range (so the encoded surrogate `\xed\xa0\x80` is three)
// — strings.ToValidUTF8 collapses a run instead, which loses bytes on the wire.
func ReplaceInvalid(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	for index := 0; index < len(text); {
		r, size := utf8.DecodeRuneInString(text[index:])
		if r != utf8.RuneError || size > 1 {
			out.WriteString(text[index : index+size])
			index += size
			continue
		}
		size = maximalSubpart(text[index:])
		out.WriteString("\uFFFD")
		index += size
	}
	return out.String()
}

// maximalSubpart returns the length of the longest well-formed prefix of the
// UTF-8 sequence at the start of text, and always consumes at least one byte so
// the replacement loop terminates. The continuation ranges are checked per lead
// byte, which is what rejects surrogates (`\xed`), over-long forms (`\xe0`) and
// code points above U+10FFFF (`\xf4`).
func maximalSubpart(text string) int {
	lead := text[0]
	continuations := 0
	low, high := byte(0x80), byte(0xBF)
	switch {
	case lead >= 0xC2 && lead <= 0xDF:
		continuations = 1
	case lead == 0xE0:
		continuations = 2
		low = 0xA0
	case lead >= 0xE1 && lead <= 0xEC:
		continuations = 2
	case lead == 0xED:
		continuations = 2
		high = 0x9F
	case lead >= 0xEE && lead <= 0xEF:
		continuations = 2
	case lead == 0xF0:
		continuations = 3
		low = 0x90
	case lead >= 0xF1 && lead <= 0xF3:
		continuations = 3
	case lead == 0xF4:
		continuations = 3
		high = 0x8F
	default:
		return 1
	}
	length := 1
	for index := 0; index < continuations; index++ {
		if length >= len(text) || text[length] < low || text[length] > high {
			break
		}
		length++
		low, high = 0x80, 0xBF
	}
	return length
}

// NullByteError is the ValueError CPython raises for a path argument holding a
// NUL byte: the check runs before the syscall, so the failure is not an OSError
// and its report line is not an errno line. The operation name is the C function
// CPython would have called (`lstat`, `readlink`).
type NullByteError struct{ Operation string }

func (e *NullByteError) Error() string {
	return e.Operation + ": embedded null character in path"
}

// CheckPath reproduces CPython's NUL rejection before a path syscall.
func CheckPath(operation, path string) error {
	if strings.IndexByte(path, 0) >= 0 {
		return &NullByteError{Operation: operation}
	}
	return nil
}

func escapeRune(r rune, quote byte) string {
	switch {
	case r == '\\':
		return `\\`
	case byte(r) == quote && r < utf8.RuneSelf:
		return `\` + string(quote)
	case r == '\t':
		return `\t`
	case r == '\n':
		return `\n`
	case r == '\r':
		return `\r`
	case unicode.IsPrint(r):
		return string(r)
	case r <= 0xFF:
		return fmt.Sprintf(`\x%02x`, r)
	case r <= 0xFFFF:
		return fmt.Sprintf(`\u%04x`, r)
	default:
		return fmt.Sprintf(`\U%08x`, r)
	}
}

const surrogateBase = 0xDC00

// cStrerror holds the glibc strerror(3) strings for the errno values these
// helpers can surface. Python's `str(OSError)` prints strerror, not Go's
// lower-case text, so a Go-only rendering would change deterministic bytes.
//
//nolint:exhaustive // syscall.Errno is an open platform set; strerror's fallback covers the rest.
var cStrerror = map[syscall.Errno]string{
	syscall.EPERM:        "Operation not permitted",
	syscall.ENOENT:       "No such file or directory",
	syscall.EIO:          "Input/output error",
	syscall.EACCES:       "Permission denied",
	syscall.EEXIST:       "File exists",
	syscall.ENOTDIR:      "Not a directory",
	syscall.EISDIR:       "Is a directory",
	syscall.EINVAL:       "Invalid argument",
	syscall.ENAMETOOLONG: "File name too long",
	syscall.EFBIG:        "File too large",
	syscall.ENOSPC:       "No space left on device",
	syscall.EROFS:        "Read-only file system",
	syscall.ELOOP:        "Too many levels of symbolic links",
	syscall.ENOTEMPTY:    "Directory not empty",
	syscall.EPIPE:        "Broken pipe",
	syscall.EAGAIN:       "Resource temporarily unavailable",
	syscall.ENOMEM:       "Cannot allocate memory",
	syscall.EBUSY:        "Device or resource busy",
	syscall.ENODEV:       "No such device",
	syscall.EOVERFLOW:    "Value too large for defined data type",
	syscall.ECONNRESET:   "Connection reset by peer",
	syscall.ETIMEDOUT:    "Connection timed out",
}

func strerror(errno syscall.Errno) string {
	if text, ok := cStrerror[errno]; ok {
		return text
	}
	text := errno.Error()
	if text == "" {
		return "Unknown error"
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// ErrnoOf reports the Unix errno a Go error carries, if any.
func ErrnoOf(err error) (syscall.Errno, bool) {
	var errno syscall.Errno
	ok := errors.As(err, &errno)
	return errno, ok
}

// exceptionClass mirrors the OSError subclass Python selects from an errno, so
// the reported exception line keeps its legacy spelling. The table is CPython's
// (Objects/exceptions.c, OSError_new) as measured on 3.13.5 for every errno it
// defines; an errno outside it stays a plain OSError, exactly as in CPython.
func exceptionClass(errno syscall.Errno) string {
	//nolint:exhaustive // syscall.Errno is an open platform set; OSError is the fallback.
	switch errno {
	case syscall.EPERM, syscall.EACCES:
		return "PermissionError"
	case syscall.ENOENT:
		return "FileNotFoundError"
	case syscall.ESRCH:
		return "ProcessLookupError"
	case syscall.EINTR:
		return "InterruptedError"
	case syscall.ECHILD:
		return "ChildProcessError"
	case syscall.EAGAIN, syscall.EALREADY, syscall.EINPROGRESS:
		return "BlockingIOError"
	case syscall.EEXIST:
		return "FileExistsError"
	case syscall.ENOTDIR:
		return "NotADirectoryError"
	case syscall.EISDIR:
		return "IsADirectoryError"
	case syscall.EPIPE, syscall.ESHUTDOWN:
		return "BrokenPipeError"
	case syscall.ECONNABORTED:
		return "ConnectionAbortedError"
	case syscall.ECONNREFUSED:
		return "ConnectionRefusedError"
	case syscall.ECONNRESET:
		return "ConnectionResetError"
	case syscall.ETIMEDOUT:
		return "TimeoutError"
	default:
		return "OSError"
	}
}

// OSErrorText mirrors `str(OSError(errno, strerror, filename))`; the filename
// part is omitted when Python would not have set one.
func OSErrorText(errno syscall.Errno, filename string) string {
	if filename == "" {
		return fmt.Sprintf("[Errno %d] %s", int(errno), strerror(errno))
	}
	return fmt.Sprintf("[Errno %d] %s: %s", int(errno), strerror(errno), Repr(filename))
}

// ExceptionLine mirrors the final line of a Python traceback for the error, and
// the exception line of the "Exception ignored" flush diagnostic.
func ExceptionLine(errno syscall.Errno, filename string) string {
	return exceptionClass(errno) + ": " + OSErrorText(errno, filename)
}
