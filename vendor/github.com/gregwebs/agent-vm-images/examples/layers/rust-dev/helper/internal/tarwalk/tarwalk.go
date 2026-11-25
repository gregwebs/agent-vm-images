// Package tarwalk reads a component archive: a single xz stream containing one
// tar, bounded before anything is allocated.
//
// It is a port of the source reader's decoded-archive walk (slice 3 of #187, the
// Python deleted in #245). The two rules that
// are Go's own, and not Python's, are stated where the next reader looks:
//
//   - PAX records are merged latest-wins, matching Go's `archive/tar` (the second
//     pass). Python's `tarfile` gives the earliest pending set precedence; this
//     package must describe the parser that will actually read the archive.
//   - the xz reader is `SingleStream: true`, so a second stream or trailing bytes
//     after the first stream's EOF are refused. Python's `LZMADecompressor` stops
//     at the first EOF and ignores the rest. The strictness cannot be reached
//     through an authenticated archive: the archive's bytes are compared against
//     the reviewed checksum *before* this decoder sees them, so trailing garbage
//     would change the checksum and be refused earlier with a different
//     diagnostic. It is defence in depth, not a behaviour difference users can
//     reach (Q3, #231).
package tarwalk

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/ulikunitz/xz"
)

// Reviewed bounds (ADR 0017 D2). A measured review changes them.
const (
	MaxDecodedBytes       int64 = 2 << 30
	MaxExtendedBytes      int64 = 64 << 10
	MaxExtendedTotalBytes int64 = 256 << 10
)

// Limits are the streaming bounds Scan applies. The zero value means the
// reviewed defaults; tests lower them rather than building 2 GiB fixtures.
type Limits struct {
	DecodedBytes  int64
	ExtendedBytes int64
	ExtendedTotal int64
}

func (l Limits) withDefaults() Limits {
	if l.DecodedBytes <= 0 {
		l.DecodedBytes = MaxDecodedBytes
	}
	if l.ExtendedBytes <= 0 {
		l.ExtendedBytes = MaxExtendedBytes
	}
	if l.ExtendedTotal <= 0 {
		l.ExtendedTotal = MaxExtendedTotalBytes
	}
	return l
}

// Refusal is one archive-policy rule. The message is the contract: the shell
// suites match its fragments.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

func refuse(format string, arguments ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, arguments...)}
}

// Scan runs the extended-header pre-pass and then the member walk.
//
// The pre-pass exists because `archive/tar` consumes GNU `L`/`K` and PAX
// `x`/`X`/`g` headers itself, before yielding a member: the per-file metadata
// caps can never see them, and a longname bomb would be allocated before any
// member rule ran. The pre-pass walks the same 512-byte blocks and applies the
// same effective-size rules, then the member walk reads through `archive/tar`.
func Scan(path, what string, limits Limits,
	visit func(*tar.Header, *tar.Reader, bool) error) error {
	limits = limits.withDefaults()
	v7, err := boundExtendedHeaders(path, what, limits)
	if err != nil {
		return err
	}
	reader, closeStream, err := decoded(path, what, limits.DecodedBytes)
	if err != nil {
		return err
	}
	defer closeStream()
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return wrap(what, err)
		}
		if err := visit(header, archive, v7[header.Name]); err != nil {
			return err
		}
	}
	// Drain the decoded stream: only then does the xz reader report a second
	// stream or trailing bytes, and only then is the whole decoded size counted.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return wrap(what, err)
	}
	return nil
}

// wrap names the archive in a decoder refusal unless the message already does.
func wrap(what string, err error) error {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return err
	}
	return refuse("%s: cannot read the xz archive: %v", what, err)
}

// decoded opens one single-stream xz decoder and wraps it in the decoded-byte
// counter.
func decoded(path, what string, limit int64) (io.Reader, func(), error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, nil, refuse("%s: cannot read the xz archive: %v", what, err)
	}
	stream, err := (xz.ReaderConfig{SingleStream: true}).NewReader(handle)
	if err != nil {
		_ = handle.Close()
		return nil, nil, refuse("%s: cannot decode the xz stream: %v", what, err)
	}
	return &countingReader{reader: stream, limit: limit, what: what}, func() {
		_ = handle.Close()
	}, nil
}

// countingReader counts every decompressed byte before the tar parser can
// consume it: headers, extended headers, padding and payloads alike.
type countingReader struct {
	reader io.Reader
	count  int64
	limit  int64
	what   string
}

func (c *countingReader) Read(buffer []byte) (int, error) {
	count, err := c.reader.Read(buffer)
	c.count += int64(count)
	if c.count > c.limit {
		return 0, refuse("%s: decoded stream exceeds %d bytes, padding and extended "+
			"headers included", c.what, c.limit)
	}
	return count, err
}

// --- extended-header pre-pass -------------------------------------------------

var extendedTypes = map[byte]string{
	'L': "gnu", 'K': "gnu",
	'x': "pax", 'X': "pax",
	'g': "global",
}

func boundExtendedHeaders(path, what string, limits Limits) (map[string]bool, error) {
	reader, closeStream, err := decoded(path, what, limits.DecodedBytes)
	if err != nil {
		return nil, err
	}
	defer closeStream()
	v7 := map[string]bool{}
	var total int64
	global := map[string]string{}
	pending := []map[string]string{}
	for {
		block := make([]byte, 512)
		if _, err := io.ReadFull(reader, block); err != nil {
			var refusal *Refusal
			if errors.As(err, &refusal) {
				return nil, err
			}
			return nil, refuse("%s: truncated archive header", what)
		}
		if isZero(block) {
			return v7, nil
		}
		typeflag := block[156]
		size, err := headerSize(block[124:136], what)
		if err != nil {
			return nil, err
		}
		if size < 0 {
			return nil, refuse("%s: negative tar header size", what)
		}
		kind, extended := extendedTypes[typeflag]
		if extended {
			if size > limits.ExtendedBytes {
				return nil, refuse("%s: extended header of %d bytes exceeds %d", what, size,
					limits.ExtendedBytes)
			}
			total += size
			if total > limits.ExtendedTotal {
				return nil, refuse("%s: extended headers total %d bytes, exceeding %d", what,
					total, limits.ExtendedTotal)
			}
			var payload []byte
			if kind == "gnu" {
				if err := discard(reader, size, what); err != nil {
					return nil, err
				}
			} else {
				payload = make([]byte, size)
				if _, err := io.ReadFull(reader, payload); err != nil {
					var refusal *Refusal
					if errors.As(err, &refusal) {
						return nil, err
					}
					return nil, refuse("%s: truncated archive member data", what)
				}
			}
			if err := discard(reader, padding(size), what); err != nil {
				return nil, err
			}
			if kind == "pax" || kind == "global" {
				records, err := paxRecords(payload, what)
				if err != nil {
					return nil, err
				}
				if err := rejectSparseMetadata(records, what); err != nil {
					return nil, err
				}
				if kind == "global" {
					for key, value := range records {
						global[key] = value
					}
				} else {
					pending = append(pending, records)
				}
			}
			continue
		}
		if typeflag == 'S' {
			return nil, refuse("%s: GNU sparse members are outside the archive policy", what)
		}
		if len(pending) > 0 || len(global) > 0 {
			override, present, err := paxSize(global, pending, what)
			if err != nil {
				return nil, err
			}
			if present {
				size = override
			}
			pending = nil
		}
		// V7 normalization: an AREGTYPE (NUL) header whose *header* name ends in
		// "/" is a directory to Python's tarfile. Its size field is then not
		// data. A follow-up header of this shape is refused when it claims data,
		// rather than walked in a version-specific way.
		if typeflag == 0 && endsWithSlash(block[:100]) {
			if size != 0 {
				return nil, refuse("%s: V7 directory header declares %d bytes of data", what, size)
			}
			v7[headerName(block[:100])] = true
			continue
		}
		if skipsData(typeflag) {
			if err := discard(reader, size, what); err != nil {
				return nil, err
			}
			if err := discard(reader, padding(size), what); err != nil {
				return nil, err
			}
		}
	}
}

func isZero(block []byte) bool {
	for _, b := range block {
		if b != 0 {
			return false
		}
	}
	return true
}

func headerName(field []byte) string {
	for index, b := range field {
		if b == 0 {
			return string(field[:index])
		}
	}
	return string(field)
}

func endsWithSlash(field []byte) bool {
	for index, b := range field {
		if b == 0 {
			return index > 0 && field[index-1] == '/'
		}
	}
	return len(field) > 0 && field[len(field)-1] == '/'
}

func padding(size int64) int64 { return (512 - size%512) % 512 }

func headerSize(field []byte, what string) (int64, error) {
	if len(field) > 0 && field[0]&0x80 != 0 {
		// GNU base-256. The source reader reads field[1:] big-endian unsigned;
		// archives use the positive form, and a negative one is out of bounds.
		var value int64
		for _, b := range field[1:] {
			if value > (1<<55)-1 {
				return 0, refuse("%s: unreadable tar header size field", what)
			}
			value = value<<8 | int64(b)
		}
		return value, nil
	}
	text := field
	for index, b := range field {
		if b == 0 {
			text = field[:index]
			break
		}
	}
	for len(text) > 0 && (text[0] == ' ' || text[len(text)-1] == ' ') {
		if text[0] == ' ' {
			text = text[1:]
		}
		if len(text) > 0 && text[len(text)-1] == ' ' {
			text = text[:len(text)-1]
		}
	}
	if len(text) == 0 {
		return 0, nil
	}
	value, err := strconv.ParseInt(string(text), 8, 64)
	if err != nil {
		return 0, refuse("%s: unreadable tar header size field", what)
	}
	return value, nil
}

func discard(reader io.Reader, size int64, what string) error {
	if size <= 0 {
		return nil
	}
	if _, err := io.CopyN(io.Discard, reader, size); err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			return err
		}
		return refuse("%s: truncated archive member data", what)
	}
	return nil
}

// paxRecords parses an already size-bounded PAX payload. Framing mistakes are
// refusals, never slice panics.
func paxRecords(payload []byte, what string) (map[string]string, error) {
	records := map[string]string{}
	for position := 0; position < len(payload) && payload[position] != 0; {
		space := indexByte(payload, ' ', position)
		if space < 0 {
			return nil, refuse("%s: unreadable PAX record at %d", what, position)
		}
		length, err := strconv.Atoi(string(payload[position:space]))
		if err != nil {
			return nil, refuse("%s: unreadable PAX record length", what)
		}
		if length < 5 || position+length > len(payload) {
			return nil, refuse("%s: PAX record length outside its header", what)
		}
		record := payload[space+1 : position+length]
		if len(record) == 0 || record[len(record)-1] != '\n' {
			return nil, refuse("%s: PAX record is not newline-terminated", what)
		}
		key, value, found := cut(record[:len(record)-1], '=')
		if !found || len(key) == 0 {
			return nil, refuse("%s: unreadable PAX record", what)
		}
		records[string(key)] = string(value)
		position += length
	}
	return records, nil
}

func indexByte(data []byte, b byte, from int) int {
	for index := from; index < len(data); index++ {
		if data[index] == b {
			return index
		}
	}
	return -1
}

func cut(data []byte, b byte) (before, after []byte, found bool) {
	index := indexByte(data, b, 0)
	if index < 0 {
		return data, nil, false
	}
	return data[:index], data[index+1:], true
}

func rejectSparseMetadata(records map[string]string, what string) error {
	for key := range records {
		if len(key) >= len("GNU.sparse.") && key[:len("GNU.sparse.")] == "GNU.sparse." {
			return refuse("%s: sparse PAX metadata %s is outside the archive policy", what,
				shown(key))
		}
	}
	return nil
}

// paxSize applies global records and then the pending per-file sets in order,
// latest wins, which is what `archive/tar` does. An unreadable or negative size
// is refused: a parser that cannot say how long a member is cannot bound it.
func paxSize(global map[string]string, pending []map[string]string, what string) (int64, bool, error) {
	value, present := global["size"]
	for _, records := range pending {
		if candidate, ok := records["size"]; ok {
			value, present = candidate, true
		}
	}
	if !present {
		return 0, false, nil
	}
	size, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false, refuse("%s: unreadable PAX size record", what)
	}
	if size < 0 {
		return 0, false, refuse("%s: negative PAX size record", what)
	}
	return size, true, nil
}

// skipsData reports whether the member's size field counts payload bytes. The
// source reader skips a regular member or a type it does not support; a
// directory, link or device header carries no data.
func skipsData(typeflag byte) bool {
	switch typeflag {
	case '0', 0, '7', 'S', '1', '2', '5', '6', '3', '4', 'L', 'K':
		return typeflag == '0' || typeflag == 0 || typeflag == '7' || typeflag == 'S'
	default:
		return true
	}
}

func shown(name string) string {
	if len(name) <= 80 {
		return name
	}
	return name[:77] + "..."
}
