package artifactinstall

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
)

// MaxLinkDepth bounds a symlink chain. Every chain is resolved through the
// archive's own links, so a bound is the cycle detector: a real payload nests a
// handful of links, never this many.
const MaxLinkDepth = 16

// ELFHeaderBytes is enough for every field the architecture check reads.
const ELFHeaderBytes = 20

// Sum512 is the lower-case SHA-512 of raw.
func Sum512(raw []byte) artifactpin.SHA512 {
	sum := sha512.Sum512(raw)
	return artifactpin.SHA512(hex.EncodeToString(sum[:]))
}

// Events records what the installer did. Without it a regression that extracts
// before hashing keeps a suite green, so the ordering is proved from the log
// instead of inferred from the source.
type Events struct{ log io.Writer }

// NewEvents appends every event to log.
func NewEvents(log io.Writer) *Events { return &Events{log: log} }

// Fetch records one request, before the first attempt.
func (e *Events) Fetch(url string) { e.write("fetch", url) }

// Verify records one evidence check.
func (e *Events) Verify(kind string) { e.write("verify", kind) }

// Publish records how many files publication created.
func (e *Events) Publish(count int) { e.write("publish", itoa(count)) }

func (e *Events) extract(count int) { e.write("extract", itoa(count)) }

func (e *Events) write(kind, detail string) {
	if e == nil || e.log == nil {
		return
	}
	_, _ = io.WriteString(e.log, kind+" "+detail+"\n")
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// openArchive returns the uncompressed tar stream over one archive file. Every
// caller opens its own, so preflight and extraction are two passes instead of a
// buffer that would hold a decoded 234 MB payload in memory.
type openArchive func() (io.ReadCloser, error)

func archiveOpener(archiveFile string, format artifactpin.ArchiveFormat, compress string) openArchive {
	return func() (io.ReadCloser, error) {
		handle, err := os.Open(archiveFile)
		if err != nil {
			return nil, err
		}
		if format == artifactpin.TarGz {
			reader, err := gzip.NewReader(handle)
			if err != nil {
				_ = handle.Close()
				return nil, err
			}
			return &compoundCloser{Reader: reader, closers: []io.Closer{reader, handle}}, nil
		}
		// xz decodes with the base image's own xz-utils, which the recipe already
		// installs for exactly this archive: adding a second decoder would either
		// widen the vendor closure or duplicate a tool the image ships.
		if compress == "" {
			compress = "xz"
		}
		command := exec.Command(compress, "-dc", archiveFile) //nolint:gosec // the path is the installer's own scratch file.
		command.Stderr = os.Stderr
		stdout, err := command.StdoutPipe()
		if err != nil {
			_ = handle.Close()
			return nil, err
		}
		if err := command.Start(); err != nil {
			_ = handle.Close()
			return nil, err
		}
		return &processReader{Reader: stdout, command: command, file: handle}, nil
	}
}

type compoundCloser struct {
	io.Reader
	closers []io.Closer
}

func (c *compoundCloser) Close() error {
	var err error
	for _, closer := range c.closers {
		if closeErr := closer.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

type processReader struct {
	io.Reader
	command *exec.Cmd
	file    *os.File
}

func (p *processReader) Close() error {
	_ = p.command.Wait()
	return p.file.Close()
}

// canonicalMember is the member's canonical path, or a refusal.
//
// A name that differs from its own canonical form is a *second spelling* of a
// path the archive may already carry: `prefix/bin/node` followed by
// `prefix/bin/./node` used to pass preflight and silently overwrite the first
// file's contents.
func canonicalMember(name string, isDir bool) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\n") {
		return "", refuse("the archive carries an unusable member name: %q", name)
	}
	parts := make([]string, 0, 8)
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." {
			continue
		}
		parts = append(parts, part)
	}
	canonical := strings.Join(parts, "/")
	if isDir && name == canonical+"/" {
		return canonical, nil
	}
	if name != canonical {
		return "", refuse("the archive member is not a canonical path: %q", name)
	}
	return canonical, nil
}

// member is one archive entry, reduced to what the checks act on.
type member struct {
	name     string
	isDir    bool
	isFile   bool
	isSym    bool
	isHard   bool
	mode     int64
	size     int64
	linkname string
}

func membersOf(open openArchive) ([]member, error) {
	stream, err := open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	reader := tar.NewReader(stream)
	var members []member
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, refuse("the archive is unreadable: %s", err)
		}
		members = append(members, member{
			name:     header.Name,
			isDir:    header.Typeflag == tar.TypeDir,
			isFile:   header.Typeflag == tar.TypeReg,
			isSym:    header.Typeflag == tar.TypeSymlink,
			isHard:   header.Typeflag == tar.TypeLink,
			mode:     header.Mode,
			size:     header.Size,
			linkname: header.Linkname,
		})
	}
	return members, nil
}

// preflight reads every member before a byte is written. The archive is a
// reviewed upstream payload, not a trusted one.
func preflight(members []member, prefix string) (map[string]member, error) {
	if len(members) == 0 {
		return nil, refuse("the archive is empty")
	}
	root := strings.TrimSuffix(prefix, "/")
	byName := make(map[string]member, len(members))
	for _, entry := range members {
		name, err := canonicalMember(entry.name, entry.isDir)
		if err != nil {
			return nil, err
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return nil, refuse("the archive member escapes its prefix: %q", entry.name)
			}
		}
		if prefix != "" && name != root && !strings.HasPrefix(name, prefix) {
			return nil, refuse("the archive member is outside %s: %q", prefix, entry.name)
		}
		if !entry.isDir && !entry.isFile && !entry.isSym && !entry.isHard {
			return nil, refuse("the archive member is not a file, directory or link: %q", name)
		}
		if entry.mode&0o6000 != 0 {
			return nil, refuse("the archive member carries a setuid/setgid bit: %q", name)
		}
		if _, duplicate := byName[name]; duplicate {
			return nil, refuse("the archive repeats a member: %q", name)
		}
		byName[name] = entry
	}
	if err := checkConflicts(byName); err != nil {
		return nil, err
	}
	if err := checkLinks(byName, root); err != nil {
		return nil, err
	}
	return byName, nil
}

// checkConflicts refuses a member that sits under another member that is not a
// directory. That is the file/directory conflict, and it is also how a write
// through a declared link happens.
func checkConflicts(byName map[string]member) error {
	for name := range byName {
		for parent := path.Dir(name); parent != "." && parent != "/" && parent != ""; parent = path.Dir(parent) {
			ancestor, ok := byName[parent]
			if !ok {
				continue
			}
			if ancestor.isSym || ancestor.isHard {
				return refuse("the archive would write through the link %s to create %s",
					ancestor.name, name)
			}
			if !ancestor.isDir {
				return refuse("the archive carries %s under the file %s", name, ancestor.name)
			}
		}
	}
	return nil
}

// resolveThrough resolves one archive path through the archive's own links, so a
// link target like `a/../..` is judged the way an extractor would see it instead
// of lexically. A cycle and a resolution that leaves the archive are refusals.
func resolveThrough(name string, links map[string]string) ([]string, error) {
	var resolved []string
	pending := strings.Split(name, "/")
	hops := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return nil, refuse("%s links outside the archive", name)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}
		candidate := strings.Join(append(append([]string{}, resolved...), part), "/")
		link, ok := links[candidate]
		if !ok {
			resolved = append(resolved, part)
			continue
		}
		if link == "" || strings.HasPrefix(link, "/") || strings.Contains(link, "\\") {
			return nil, refuse("%s links outside the archive: %q", candidate, link)
		}
		hops++
		if hops > MaxLinkDepth {
			return nil, refuse("%s resolves through a cycle of links", name)
		}
		pending = append(strings.Split(link, "/"), pending...)
	}
	return resolved, nil
}

// checkLinks resolves the whole link graph: no cycle, every target inside the
// archive, and a hard link to a regular member that really is a file.
func checkLinks(byName map[string]member, root string) error {
	local := make(map[string]string)
	for name, entry := range byName {
		if entry.isSym {
			local[name] = entry.linkname
		}
	}
	prefix := root + "/"
	for name, entry := range byName {
		if entry.isHard {
			target := entry.linkname
			if canonicalTarget(target) != target {
				return refuse("%s is a hard link to a non-canonical path: %q", name, target)
			}
			linked, ok := byName[target]
			if !ok {
				return refuse("%s is a hard link to an absent member: %q", name, target)
			}
			if !linked.isFile {
				return refuse("%s is a hard link to %s, which is not a file", name, target)
			}
		}
		if !entry.isSym && !entry.isHard {
			continue
		}
		target := entry.linkname
		if target == "" || strings.HasPrefix(target, "/") || strings.Contains(target, "\\") {
			return refuse("%s links outside the archive: %q", name, target)
		}
		if entry.isHard {
			continue
		}
		start := strings.TrimSuffix(path.Dir(name), ".") + "/" + target
		if path.Dir(name) == "." || path.Dir(name) == "" {
			start = target
		}
		resolved, err := resolveThrough(start, local)
		if err != nil {
			return err
		}
		final := strings.Join(resolved, "/")
		if final != root && !strings.HasPrefix(final, prefix) {
			return refuse("%s links outside the archive: %q", name, target)
		}
	}
	return nil
}

func canonicalTarget(name string) string {
	parts := make([]string, 0, 8)
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." {
			continue
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "/")
}

// checkMembers is the archive shape an artifact's identity includes. It runs on
// the member metadata between preflight and extraction, so a second member, a
// directory where the reviewed binary should be, a different permission mode or
// a payload past its ceiling is refused before a byte reaches staging.
func checkMembers(pin artifactpin.Pin, byName map[string]member) error {
	expected, mode, ceiling, reviewed := artifactpin.ExactShape(pin.Name)
	if !reviewed {
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sortStrings(names)
	if len(names) != len(expected) {
		return refuse("%s: the archive carries %s, the reviewed archive carries %s",
			pin.Name, describeList(names), describeList(expected))
	}
	for index, name := range names {
		if name != expected[index] {
			return refuse("%s: the archive carries %s, the reviewed archive carries %s",
				pin.Name, describeList(names), describeList(expected))
		}
	}
	for _, name := range names {
		entry := byName[name]
		if !entry.isFile {
			return refuse("%s: %s is not a regular file", pin.Name, entry.name)
		}
		// Extraction publishes the member's own bits, so the reviewed mode is
		// refused here rather than narrowed later: a group- or world-writable
		// member would ship a binary every guest UID can replace.
		if entry.mode&0o7777 != mode {
			return refuse("%s: %s is mode %04o, the reviewed archive carries %04o",
				pin.Name, entry.name, entry.mode&0o7777, mode)
		}
		if entry.size > ceiling {
			return refuse("%s: %s decodes to %d bytes, past the %d byte ceiling",
				pin.Name, entry.name, entry.size, ceiling)
		}
	}
	return nil
}

func describeList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "'"+value+"'")
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// checkELF reads the architecture of one payload binary from its own header. A
// release asset whose digest and recorded corroboration are mutually consistent
// can still be the other architecture's binary: e_machine is the only thing that
// says otherwise.
func checkELF(pinName artifactpin.Name, published string, header []byte, machine uint16,
	platform artifactpin.Platform) error {
	if len(header) != ELFHeaderBytes || string(header[:4]) != "\x7fELF" {
		return refuse("%s: %s is not an ELF binary", pinName, published)
	}
	if header[4] != 2 || header[5] != 1 {
		return refuse("%s: %s is not a 64-bit little-endian ELF binary", pinName, published)
	}
	found := uint16(header[18]) | uint16(header[19])<<8
	if found != machine {
		return refuse("%s: %s is ELF machine %d, the pin selects %d for %s",
			pinName, published, found, machine, platform)
	}
	return nil
}

func checkExecutable(pin artifactpin.Pin, platform artifactpin.Platform, staging string) error {
	machine, ok := artifactpin.ELFMachines[pin.Name][platform]
	if !ok {
		return nil
	}
	for _, entry := range pin.Install.Payload {
		file, err := os.Open(filepath.Join(staging, entry.Archive.String()))
		if err != nil {
			return err
		}
		header := make([]byte, ELFHeaderBytes)
		read, err := io.ReadFull(file, header)
		if closeErr := file.Close(); closeErr != nil && err == nil {
			return closeErr
		}
		if err != nil && read != ELFHeaderBytes {
			return refuse("%s: %s is not an ELF binary", pin.Name, entry.Published)
		}
		if err := checkELF(pin.Name, entry.Published.String(), header, machine, platform); err != nil {
			return err
		}
	}
	return nil
}

// extract writes the archive into staging: files and directories first, links
// last, because a symlink created before the members it points at could redirect
// a later write outside the staging tree.
func extract(open openArchive, byName map[string]member, staging, prefix string) error {
	root := strings.TrimSuffix(prefix, "/")
	stream, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	reader := tar.NewReader(stream)
	written := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return refuse("the archive is unreadable: %s", err)
		}
		entry, ok := byName[header.Name]
		if !ok || entry.name != header.Name {
			// The preflight already refused anything unrecognised; a stream that
			// disagrees with it is not the reviewed archive.
			name, canonicalErr := canonicalMember(header.Name, header.Typeflag == tar.TypeDir)
			if canonicalErr != nil {
				return canonicalErr
			}
			if entry, ok = byName[name]; !ok {
				return refuse("the archive member is outside %s: %q", prefix, header.Name)
			}
		}
		if entry.isDir || entry.isFile {
			if err := writeFile(reader, entry, staging, root); err != nil {
				return err
			}
			written++
		}
	}
	if err := writeLinks(open, byName, staging, root); err != nil {
		return err
	}
	return nil
}

func relativeParts(name, root string) []string {
	if root == "" || name == root {
		return strings.Split(name, "/")
	}
	return strings.Split(strings.TrimPrefix(strings.TrimPrefix(name, root), "/"), "/")
}

func writeFile(reader *tar.Reader, entry member, staging, root string) error {
	target := filepath.Join(append([]string{staging}, relativeParts(entry.name, root)...)...)
	if err := refuseSymlinkAncestors(staging, target); err != nil {
		return err
	}
	if entry.isDir {
		if err := os.MkdirAll(target, 0o750); err != nil {
			return err
		}
		return os.Chmod(target, os.FileMode(entry.mode&0o777))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	handle, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(entry.mode&0o777))
	if err != nil {
		return err
	}
	// Streamed rather than read whole: agy's member decodes to 234 MB and a second
	// copy of it is not worth the peak memory.
	if _, err := io.Copy(handle, reader); err != nil {
		_ = handle.Close()
		return err
	}
	if err := handle.Close(); err != nil {
		return err
	}
	// The member's own bits are what publishes: for an EXACT_MEMBERS artifact
	// checkMembers has already required exactly the reviewed mode.
	return os.Chmod(target, os.FileMode(entry.mode&0o777))
}

func writeLinks(open openArchive, byName map[string]member, staging, root string) error {
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		entry := byName[name]
		if !entry.isSym && !entry.isHard {
			continue
		}
		target := filepath.Join(append([]string{staging}, relativeParts(entry.name, root)...)...)
		if err := refuseSymlinkAncestors(staging, target); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		if entry.isHard {
			linked := filepath.Join(append([]string{staging}, relativeParts(entry.linkname, root)...)...)
			if err := os.Link(linked, target); err != nil {
				return refuse("the archive hard link %s cannot be created: %s", name, err)
			}
			continue
		}
		if err := os.Symlink(entry.linkname, target); err != nil {
			return refuse("the archive symlink %s cannot be created: %s", name, err)
		}
	}
	return nil
}

// refuseSymlinkAncestors checks the filesystem itself, so no write can pass
// through a link created by an earlier member.
func refuseSymlinkAncestors(staging, target string) error {
	relative, err := filepath.Rel(staging, target)
	if err != nil {
		return refuse("%s is outside the staging tree", target)
	}
	parts := strings.Split(relative, string(filepath.Separator))
	current := staging
	for _, part := range parts[:max(len(parts)-1, 0)] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ := filepath.Rel(staging, current)
			return refuse("the archive would write through the link %s", link)
		}
	}
	return nil
}

// bundledNPM is the npm version the reviewed archive ships, read from the staged
// tree.
func bundledNPM(staging string) (string, error) {
	packageFile := filepath.Join(staging, artifactpin.NPMPackage)
	raw, err := os.ReadFile(packageFile)
	if err != nil {
		return "", refuse("the archive carries no readable %s: %s", artifactpin.NPMPackage, err)
	}
	var document struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", refuse("the archive carries no readable %s: %s", artifactpin.NPMPackage, err)
	}
	if document.Version == "" {
		return "", refuse("the archive carries no readable %s: no version", artifactpin.NPMPackage)
	}
	return document.Version, nil
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
