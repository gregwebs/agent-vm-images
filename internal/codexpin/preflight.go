package codexpin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/evidence"
	"github.com/gregwebs/agent-vm-images/internal/strictjson"
)

// The tar header typeflags this policy names, spelled out rather than imported so
// the archive walk and the inventory read the same bytes the same way.
const (
	typeV7            = 0x00
	typeRegular       = '0'
	typeLink          = '1'
	typeSymlink       = '2'
	typeChar          = '3'
	typeBlock         = '4'
	typeDirectory     = '5'
	typeFIFO          = '6'
	typeContiguous    = '7'
	typeGNUSparse     = 'S'
	typeGNULongName   = 'L'
	typeGNULongLink   = 'K'
	typePAX           = 'x'
	typePAXGlobal     = 'g'
	typePAXExtended   = 'X'
	tarBlockBytes     = 512
	tarSizeFieldFrom  = 124
	tarSizeFieldTo    = 136
	tarTypeflagOffset = 156
	tarNameLimit      = 100
)

// requireELF is the first 20 bytes of the reviewed entrypoint, which must be usable
// on `target`.
//
// Checked without extraction and without execution: a wrong-machine ELF is refused
// even when a synthetic pin and checksum manifest consistently hash that archive, so
// "the checksums agree" can never stand in for "this is a Linux/<<arch>> binary".
func requireELF(header []byte, target Target) error {
	if len(header) < elfHeaderBytes {
		return Refuse("bin/codex is shorter than an ELF header")
	}
	if !bytes.Equal(header[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return Refuse("bin/codex is not an ELF object")
	}
	if header[4] != 2 {
		return Refuse("bin/codex is not a 64-bit ELF object")
	}
	if header[5] != 1 {
		return Refuse("bin/codex is not a little-endian ELF object")
	}
	if header[6] != 1 {
		return Refuse("bin/codex is not a current-version ELF object")
	}
	machine := uint16(header[18]) | uint16(header[19])<<8
	expected := ELFMachines[target]
	if machine != expected {
		return Refuse("bin/codex targets ELF machine %d, not %d (%s)", machine, expected, target)
	}
	return nil
}

// Preflight proves the downloaded archive is the reviewed one, before anything uses
// it.
//
// Order matters: the committed digest is re-computed first, then a raw-header pass
// bounds every GNU/PAX metadata body, then a bounded streaming inventory checks the
// layout, the package metadata and the entrypoint's ELF machine. Nothing is
// extracted, chmodded or executed, so every failure here happens before `tar` and
// before the native probe.
func Preflight(pin Pin, selected PlatformPin, archive string) error {
	what := selected.Asset
	info, err := os.Lstat(archive)
	if err != nil {
		if os.IsNotExist(err) {
			return Refuse("%s: no downloaded archive at %s", what, archive)
		}
		return Refuse("%s: cannot read %s: %s", what, archive, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Refuse("%s: %s is not a regular file", what, archive)
	}
	if info.Size() > MaxCompressedBytes {
		return Refuse("%s: compressed archive is %d bytes, exceeding %d",
			what, info.Size(), MaxCompressedBytes)
	}
	actual, _, err := evidence.FileDigest(archive, MaxCompressedBytes, what+" compressed archive")
	if err != nil {
		return err
	}
	if actual != selected.SHA256 {
		return Refuse("%s: archive sha256 %s does not match the committed %s",
			what, actual, selected.SHA256)
	}
	stream, err := openDecoded(archive, what, MaxDecodedBytes)
	if err != nil {
		return err
	}
	boundErr := boundExtendedHeaders(stream, what)
	drainErr := stream.drain()
	closeErr := stream.close()
	if boundErr != nil {
		return boundErr
	}
	if drainErr != nil {
		return drainErr
	}
	if closeErr != nil {
		return closeErr
	}
	stream, err = openDecoded(archive, what, MaxDecodedBytes)
	if err != nil {
		return err
	}
	elfHeader, inventoryErr := inventory(stream, pin, selected.Target, what)
	drainErr = stream.drain()
	closeErr = stream.close()
	if inventoryErr != nil {
		return inventoryErr
	}
	if drainErr != nil {
		return drainErr
	}
	if closeErr != nil {
		return closeErr
	}
	// The ELF check runs last, on the bytes the inventory read, so a package whose
	// metadata and archive are internally consistent but built for the other machine
	// is still refused here rather than at the native probe.
	return requireELF(elfHeader, selected.Target)
}

// decodedStream is the decompressed archive, counted before anything can consume it.
//
// tar.Reader in streaming mode reads through this reader, so a limit applied to
// yielded members never sees GNU/PAX extended headers, 512-byte padding, or the
// bytes the parser discards while skipping. Counting the decoded stream bounds all
// of it: a small compressed archive cannot expand past the budget before the
// parser can allocate anything.
//
// Every read is capped at one chunk and at the remaining allowance plus one byte,
// including a caller that asks for gigabytes, so a giant declared size can never
// turn into a giant allocation here.
type decodedStream struct {
	file    *os.File
	reader  *gzip.Reader
	what    string
	limit   int64
	decoded int64
}

func openDecoded(path, what string, limit int64) (*decodedStream, error) {
	file, err := evidence.OpenNoFollow(path)
	if err != nil {
		return nil, Refuse("%s: cannot open the archive: %s", what, err)
	}
	reader, err := gzip.NewReader(file)
	if err != nil {
		_ = file.Close()
		return nil, Refuse("%s: cannot open the archive: %s", what, err)
	}
	return &decodedStream{file: file, reader: reader, what: what, limit: limit}, nil
}

func (s *decodedStream) Read(p []byte) (int, error) {
	allowance := s.limit - s.decoded
	size := int64(len(p))
	if size > evidence.ChunkBytes {
		size = evidence.ChunkBytes
	}
	if size > allowance+1 {
		size = allowance + 1
	}
	if size <= 0 {
		return 0, Refuse("%s: decoded stream exceeds %d bytes", s.what, s.limit)
	}
	read, err := s.reader.Read(p[:size])
	s.decoded += int64(read)
	if s.decoded > s.limit {
		return read, Refuse("%s: decoded stream exceeds %d bytes", s.what, s.limit)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return read, Refuse("%s: cannot decode the gzip stream: %s", s.what, err)
	}
	return read, err
}

// drain reads to EOF so gzip integrity (CRC/length) and any trailing member is seen.
func (s *decodedStream) drain() error {
	buffer := make([]byte, evidence.ChunkBytes)
	for {
		read, err := s.Read(buffer)
		if read == 0 || err != nil {
			if err == nil || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *decodedStream) close() error {
	readerErr := s.reader.Close()
	fileErr := s.file.Close()
	if readerErr != nil {
		return Refuse("%s: cannot decode the gzip stream: %s", s.what, readerErr)
	}
	if fileErr != nil {
		return Refuse("%s: cannot close the archive: %s", s.what, fileErr)
	}
	return nil
}

// discard consumes exactly size decoded bytes, or refuses a truncated archive.
func discard(stream *decodedStream, size int64, what string) error {
	remaining := size
	buffer := make([]byte, evidence.ChunkBytes)
	for remaining > 0 {
		want := remaining
		if want > evidence.ChunkBytes {
			want = evidence.ChunkBytes
		}
		read, err := stream.Read(buffer[:want])
		remaining -= int64(read)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if read == 0 {
			return Refuse("%s: truncated archive header", what)
		}
	}
	return nil
}

func readBounded(stream *decodedStream, size int64, what, truncated string) ([]byte, error) {
	payload := make([]byte, 0, size)
	buffer := make([]byte, evidence.ChunkBytes)
	for int64(len(payload)) < size {
		want := size - int64(len(payload))
		if want > evidence.ChunkBytes {
			want = evidence.ChunkBytes
		}
		read, err := stream.Read(buffer[:want])
		payload = append(payload, buffer[:read]...)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if read == 0 {
			return nil, Refuse("%s: %s", what, truncated)
		}
	}
	return payload, nil
}

// tarHeaderSize is the size field of a tar header, octal or GNU base-256.
func tarHeaderSize(field []byte, what string) (int64, error) {
	if len(field) > 0 && field[0]&0x80 != 0 {
		var size int64
		for _, byteValue := range field[1:] {
			size = size<<8 | int64(byteValue)
		}
		return size, nil
	}
	text := strings.TrimRight(string(field), "\x00")
	text = strings.Trim(text, " ")
	if text == "" {
		return 0, nil
	}
	var size int64
	for _, digit := range text {
		if digit < '0' || digit > '7' {
			return 0, Refuse("%s: unreadable tar header size field", what)
		}
		size = size*8 + int64(digit-'0')
	}
	return size, nil
}

func isExtendedHeader(typeflag byte) (string, bool) {
	switch typeflag {
	case typeGNULongName, typeGNULongLink:
		return "gnu", true
	case typePAX, typePAXExtended:
		return "pax", true
	case typePAXGlobal:
		return "global", true
	}
	return "", false
}

// paxRecords decodes one PAX payload's `length key=value\n` records.
func paxRecords(payload []byte, what string) (map[string][]byte, error) {
	records := make(map[string][]byte, 4)
	position := 0
	for position < len(payload) && payload[position] != 0 {
		space := bytes.IndexByte(payload[position:], ' ')
		if space < 0 {
			return nil, Refuse("%s: unreadable PAX record at %d", what, position)
		}
		space += position
		length, err := parseDecimal(payload[position:space])
		if err != nil {
			return nil, Refuse("%s: unreadable PAX record length", what)
		}
		if length < 5 || int64(position)+length > int64(len(payload)) {
			return nil, Refuse("%s: PAX record length outside its header", what)
		}
		end := position + int(length)
		if space+1 >= end {
			// The declared length ends inside the length field itself: Python's slice
			// was empty and refused, and a Go slice of the same span would panic.
			return nil, Refuse("%s: PAX record length outside its header", what)
		}
		record := payload[space+1 : end]
		if len(record) == 0 || record[len(record)-1] != '\n' {
			return nil, Refuse("%s: PAX record is not newline-terminated", what)
		}
		key, value, found := strings.Cut(string(record[:len(record)-1]), "=")
		if !found || key == "" {
			return nil, Refuse("%s: unreadable PAX record", what)
		}
		records[key] = []byte(value)
		position = end
	}
	return records, nil
}

func parseDecimal(raw []byte) (int64, error) {
	if len(raw) == 0 {
		return 0, errors.New("empty")
	}
	var value int64
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return 0, errors.New("not a decimal number")
		}
		if value > (1<<62)/10 {
			return 0, errors.New("too large")
		}
		value = value*10 + int64(digit-'0')
	}
	return value, nil
}

func rejectSparseMetadata(records map[string][]byte, what string) error {
	for key := range records {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return Refuse("%s: sparse PAX metadata is outside the archive policy", what)
		}
	}
	return nil
}

// paxSize is the size the pending PAX record sets override the header field with.
//
// The parser this walk has to agree with is Go's archive/tar, which merges every
// pending set in the order the headers appear, so a *later* set wins a key an earlier
// one also names. (Python's tarfile applied the earliest, which is why the Python walk
// iterated forwards; a bound that keeps that order would skip a region the Go parser
// then reads as headers.) A value the parser cannot read is refused here rather than
// guessed: the parser refuses it too.
func paxSize(pending []map[string][]byte, what string) (int64, bool, error) {
	for index := len(pending) - 1; index >= 0; index-- {
		raw, ok := pending[index]["size"]
		if !ok {
			continue
		}
		value, err := parseDecimal(raw)
		if err != nil {
			return 0, false, Refuse("%s: unreadable PAX size record", what)
		}
		return value, true, nil
	}
	return 0, false, nil
}

// boundExtendedHeaders bounds GNU/PAX extended headers before the tar parser can
// allocate them.
//
// tar.Reader reads `L`/`K`/`x`/`g`/`X` pseudo-members itself and allocates their
// contents before yielding anything, so the member-level caps never see them. This
// pass walks the same 512-byte blocks first: each extended header must fit its own
// ceiling, all of them together theirs, and the whole file the physical-header
// budget.
//
// It reproduces tar's own offset arithmetic, because a bound that walks the blocks
// differently from the parser is not a bound: a PAX `size` record replaces the
// header's size field for the member that follows. Sparse metadata and a V7
// directory header that claims data are refused: their layouts are not a raw size
// field, so the two walks could disagree.
func boundExtendedHeaders(stream *decodedStream, what string) error {
	var total int64
	headers := 0
	globalRecords := map[string][]byte{}
	// tar applies a later extended header first and an earlier one last, so the
	// earliest pending record set has the final word on `size`.
	var pending []map[string][]byte
	block := make([]byte, tarBlockBytes)
	for {
		read, err := io.ReadFull(stream, block)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		if read < tarBlockBytes {
			return Refuse("%s: truncated archive header", what)
		}
		if bytes.Equal(block, make([]byte, tarBlockBytes)) {
			return nil
		}
		headers++
		if headers > MaxPhysicalHeaders {
			return Refuse("%s: more than %d archive headers", what, MaxPhysicalHeaders)
		}
		typeflag := block[tarTypeflagOffset]
		size, err := tarHeaderSize(block[tarSizeFieldFrom:tarSizeFieldTo], what)
		if err != nil {
			return err
		}
		if size < 0 {
			return Refuse("%s: negative tar header size", what)
		}
		if kind, extended := isExtendedHeader(typeflag); extended {
			if size > MaxExtendedHeaderBytes {
				return Refuse("%s: extended header of %d bytes exceeds %d",
					what, size, MaxExtendedHeaderBytes)
			}
			total += size
			if total > MaxExtendedTotalBytes {
				return Refuse("%s: extended headers total %d bytes, exceeding %d",
					what, total, MaxExtendedTotalBytes)
			}
			var payload []byte
			if kind == "gnu" {
				// The long name itself is never allocated: it is bounded already.
				err = discard(stream, size, what)
			} else {
				payload, err = readBounded(stream, size, what, "truncated archive header")
			}
			if err != nil {
				return err
			}
			if err := discard(stream, (tarBlockBytes-size%tarBlockBytes)%tarBlockBytes, what); err != nil {
				return err
			}
			if kind == "pax" || kind == "global" {
				records, err := paxRecords(payload, what)
				if err != nil {
					return err
				}
				if err := rejectSparseMetadata(records, what); err != nil {
					return err
				}
				if kind == "global" {
					for key, value := range records {
						globalRecords[key] = value
					}
					continue
				}
				merged := make(map[string][]byte, len(globalRecords)+len(records))
				for key, value := range globalRecords {
					merged[key] = value
				}
				for key, value := range records {
					merged[key] = value
				}
				pending = append(pending, merged)
			}
			continue
		}
		if typeflag == typeGNUSparse {
			return Refuse("%s: GNU sparse members are outside the archive policy", what)
		}
		if len(pending) > 0 {
			if override, ok, err := paxSize(pending, what); err != nil {
				return err
			} else if ok {
				size = override
			}
			pending = nil
		}
		if isV7Directory(block, typeflag) {
			if size != 0 {
				return Refuse("%s: V7 directory header declares %d bytes of data", what, size)
			}
			continue
		}
		if typeflag == typeRegular || typeflag == typeV7 || typeflag == typeContiguous ||
			!supportedType(typeflag) {
			if err := discard(stream, size, what); err != nil {
				return err
			}
			if err := discard(stream, (tarBlockBytes-size%tarBlockBytes)%tarBlockBytes, what); err != nil {
				return err
			}
		}
	}
}

func isV7Directory(block []byte, typeflag byte) bool {
	if typeflag != typeV7 {
		return false
	}
	name := block[:tarNameLimit]
	if cut := bytes.IndexByte(name, 0); cut >= 0 {
		name = name[:cut]
	}
	return bytes.HasSuffix(name, []byte("/"))
}

func supportedType(typeflag byte) bool {
	switch typeflag {
	case typeRegular, typeV7, typeLink, typeSymlink, typeChar, typeBlock,
		typeDirectory, typeFIFO, typeContiguous:
		return true
	}
	return false
}

// canonicalName is the member name this policy accepts, with a directory's
// trailing `/` gone.
func canonicalName(name string, isDir bool, what string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", Refuse("%s: absolute member name %q", what, name)
	}
	stripped := name
	if isDir && strings.HasSuffix(name, "/") {
		stripped = name[:len(name)-1]
	}
	if stripped == "" || stripped != strings.TrimSpace(stripped) {
		return "", Refuse("%s: unusable member name %q", what, name)
	}
	for _, part := range strings.Split(stripped, "/") {
		if part == "" || part == "." || part == ".." {
			return "", Refuse("%s: non-canonical member name %q", what, name)
		}
	}
	return stripped, nil
}

// checkMember is the member policy: canonical, no privilege bits, no links, no
// special files.
func checkMember(header *tar.Header, what string) (string, error) {
	name, err := canonicalName(header.Name, header.Typeflag == tar.TypeDir, what)
	if err != nil {
		return "", err
	}
	if header.Mode&0o6000 != 0 {
		return "", Refuse("%s: setuid/setgid member %q", what, name)
	}
	switch header.Typeflag {
	case tar.TypeSymlink, tar.TypeLink:
		// A prefixless package has no reviewed link resolver: a link is a policy
		// change (whether to resolve it, and how) rather than an archive quirk.
		return "", Refuse("%s: link member %q is outside the archive policy", what, name)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return "", Refuse("%s: special member %q is outside the archive policy", what, name)
	case tar.TypeDir, tar.TypeReg:
		return name, nil
	}
	return "", Refuse("%s: unsupported member type for %q", what, name)
}

// inventory validates every member and returns the entrypoint's first 20 bytes.
func inventory(stream *decodedStream, pin Pin, target Target, what string) ([]byte, error) {
	reader := tar.NewReader(stream)
	type member struct {
		name  string
		isDir bool
		mode  int64
	}
	names := make(map[string]member, 64)
	var expanded int64
	var metadata []byte
	var elfHeader []byte
	seenMetadata := false
	for index := 0; ; index++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, Refuse("%s: cannot read the gzip archive: %s", what, err)
		}
		if index >= MaxLogicalMembers {
			return nil, Refuse("%s: more than %d members", what, MaxLogicalMembers)
		}
		normalized := *header
		// tarfile reads a V7 header whose name ends in `/` as a directory, and this
		// policy has to name the same member the parser will.
		if header.Typeflag == typeV7 && strings.HasSuffix(header.Name, "/") && header.Size == 0 {
			normalized.Typeflag = tar.TypeDir
		}
		name, err := checkMember(&normalized, what)
		if err != nil {
			return nil, err
		}
		if _, duplicate := names[name]; duplicate {
			return nil, Refuse("%s: duplicate archive member %q", what, name)
		}
		isDir := normalized.Typeflag == tar.TypeDir
		names[name] = member{name: name, isDir: isDir, mode: normalized.Mode}
		root := name
		if cut := strings.IndexByte(name, '/'); cut >= 0 {
			root = name[:cut]
		}
		if !TopLevelRoots[root] {
			return nil, Refuse("%s: unexpected top-level entry %q", what, root)
		}
		if isDir {
			continue
		}
		expanded += normalized.Size
		if expanded > MaxExpandedRegularBytes {
			return nil, Refuse("%s: expanded regular files total %d bytes, exceeding %d",
				what, expanded, MaxExpandedRegularBytes)
		}
		switch name {
		case packageMetadataName:
			if normalized.Size > MaxPackageMetadataBytes {
				return nil, Refuse("%s: %s is %d bytes, exceeding %d",
					what, packageMetadataName, normalized.Size, MaxPackageMetadataBytes)
			}
			raw, err := readMember(reader, normalized.Size, what, packageMetadataName)
			if err != nil {
				return nil, err
			}
			metadata = raw
			seenMetadata = true
		case PackageEntrypoint:
			headerBytes := make([]byte, elfHeaderBytes)
			read, err := io.ReadFull(reader, headerBytes)
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, Refuse("%s: cannot read the gzip archive: %s", what, err)
			}
			elfHeader = headerBytes[:read]
		}
	}
	for name := range names {
		parts := strings.Split(name, "/")
		for index := 1; index < len(parts); index++ {
			ancestor, present := names[strings.Join(parts[:index], "/")]
			if present && !ancestor.isDir {
				return nil, Refuse("%s: %q has a non-directory ancestor %q",
					what, name, ancestor.name)
			}
		}
	}
	for _, required := range RequiredExecutables {
		found, present := names[required]
		if !present {
			return nil, Refuse("%s: missing required file %s", what, required)
		}
		if found.isDir {
			return nil, Refuse("%s: %s is not a regular file", what, required)
		}
		if found.mode&0o111 == 0 {
			return nil, Refuse("%s: %s is not executable", what, required)
		}
	}
	if !seenMetadata {
		return nil, Refuse("%s: missing %s", what, packageMetadataName)
	}
	if err := checkPackageMetadata(metadata, pin, target); err != nil {
		return nil, err
	}
	if elfHeader == nil {
		return nil, Refuse("%s: cannot read %s", what, PackageEntrypoint)
	}
	return elfHeader, nil
}

func readMember(reader io.Reader, size int64, what, name string) ([]byte, error) {
	payload := make([]byte, 0, size)
	buffer := make([]byte, evidence.ChunkBytes)
	for int64(len(payload)) < size {
		want := size - int64(len(payload))
		if want > evidence.ChunkBytes {
			want = evidence.ChunkBytes
		}
		read, err := reader.Read(buffer[:want])
		payload = append(payload, buffer[:read]...)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, Refuse("%s: cannot read the gzip archive: %s", what, err)
		}
		if read == 0 {
			return nil, Refuse("%s: truncated archive member data", what)
		}
	}
	return payload, nil
}

// checkPackageMetadata is the package's own record of what it is.
func checkPackageMetadata(raw []byte, pin Pin, target Target) error {
	document, err := strictjson.Decode(raw, packageMetadataName)
	if err != nil {
		return err
	}
	members, err := strictjson.FieldsExactly(document, packageMetadataKeys, packageMetadataName)
	if err != nil {
		return err
	}
	if layout, err := strictjson.Integer(members["layoutVersion"], "package layoutVersion"); err != nil {
		return err
	} else if layout != PackageLayoutVersion {
		return Refuse("package layoutVersion must be %d", PackageLayoutVersion)
	}
	if version, err := strictjson.Text(members["version"], "package version"); err != nil {
		return err
	} else if version != pin.Version {
		return Refuse("package version is not the pinned %s", pin.Version)
	}
	if found, err := strictjson.Text(members["target"], "package target"); err != nil {
		return err
	} else if found != string(target) {
		return Refuse("package target is not %s", target)
	}
	if variant, err := strictjson.Text(members["variant"], "package variant"); err != nil {
		return err
	} else if variant != PackageVariant {
		return Refuse("package variant must be %s", PackageVariant)
	}
	if entrypoint, err := strictjson.Text(members["entrypoint"], "package entrypoint"); err != nil {
		return err
	} else if entrypoint != PackageEntrypoint {
		return Refuse("package entrypoint must be %s", PackageEntrypoint)
	}
	return nil
}
