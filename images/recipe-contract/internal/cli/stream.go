package cli

import (
	"bytes"
	"io"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/legacy"
)

// pythonBufferSize is io.DEFAULT_BUFFER_SIZE on the maintained Linux image. The
// legacy helpers' deferred-versus-synchronous write failure split depends on this
// exact boundary, so it is a constant here and is covered by process tests.
const pythonBufferSize = 8192

// streamFailureStatus is the legacy interpreter's exit status when a final
// buffered flush fails (or when stderr itself is unusable).
const streamFailureStatus = 120

// synchronousFailureStatus is the status the legacy interpreter produced when a
// write failed while the program was still running.
const synchronousFailureStatus = 1

type encoder func(string) []byte

// pythonStream reproduces a Python text stream over one file descriptor:
// block-buffered stdout with surrogateescape encoding, line-buffered stderr with
// backslashreplace encoding, and the legacy split between a synchronous write
// failure and a deferred final-flush failure.
type pythonStream struct {
	dest         io.Writer
	encode       encoder
	buffer       []byte
	lineBuffered bool
	// broken records a failure already reported to the caller. The exit-time
	// flush then stays silent: the legacy helper did not re-report a synchronous
	// write failure as a flush failure.
	broken bool
}

func newStdout(dest io.Writer) *pythonStream {
	return &pythonStream{dest: dest, encode: legacy.RawIdentity}
}

func newStderr(dest io.Writer) *pythonStream {
	return &pythonStream{dest: dest, encode: legacy.BackslashReplace, lineBuffered: true}
}

func (s *pythonStream) writeString(text string) error {
	if s.broken {
		return nil
	}
	// Measured against the maintained image's CPython 3.13.5: the pending text
	// is flushed before new text is appended when the two would not fit, and the
	// append itself is flushed once the buffer reaches the buffer size. A total of
	// 8192 bytes is therefore still the first synchronous write failure and 8191
	// the last deferred one, and chunk size does not move that boundary. The order
	// is observable when both streams share one descriptor (`2>&1`): Python's
	// diagnostic lands after the last flush that fit, not after the append that
	// overflowed it.
	data := s.encode(text)
	if len(s.buffer) > 0 && len(s.buffer)+len(data) > pythonBufferSize {
		if err := s.flushBuffer(); err != nil {
			return err
		}
	}
	s.buffer = append(s.buffer, data...)
	if s.lineBuffered && bytes.IndexByte(data, '\n') >= 0 {
		if err := s.flushBuffer(); err != nil {
			return err
		}
		return nil
	}
	if len(s.buffer) >= pythonBufferSize {
		if err := s.flushBuffer(); err != nil {
			return err
		}
	}
	return nil
}

// flush is the exit-time flush. A failure here is the legacy deferred failure,
// reported by the caller as status 120.
func (s *pythonStream) flush() error {
	if s.broken {
		return nil
	}
	return s.flushBuffer()
}

func (s *pythonStream) flushBuffer() error {
	if len(s.buffer) == 0 {
		return nil
	}
	data := s.buffer
	// The legacy buffer dropped its bytes when a flush failed, so a later flush
	// could not report the same failure twice.
	s.buffer = s.buffer[:0]
	if err := s.rawWrite(data); err != nil {
		s.broken = true
		return err
	}
	return nil
}

func (s *pythonStream) rawWrite(data []byte) error {
	for len(data) > 0 {
		written, err := s.dest.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
