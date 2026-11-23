package toolchain

// Component-archive preflight. Nothing is extracted: the archive is read twice and
// every member rule is applied before rustup ever sees the file. The two passes
// live in internal/tarwalk (extended-header bounding, then the member walk); this
// file owns the payload policy.
//
// The summed expanded-size rule the source reader also carried
// (`expanded += member.size > MAX_EXPANDED_BYTES`) is deliberately NOT here: the
// decoded-stream cap counts every payload, header and padding byte, and sparse
// members are refused above, so no archive can exceed 2 GiB of regular-file
// payload without already tripping the stream cap. It was an unreachable branch,
// and a rule whose deletion leaves the suite green is not covered (#216, Q3).

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/tarwalk"
)

type memberKind int

const (
	regularMember memberKind = iota
	directoryMember
	symlinkMember
	hardlinkMember
)

type archiveMember struct {
	name     string
	mode     int64
	size     int64
	kind     memberKind
	linkname string
}

// PreflightArchive applies the archive policy to one downloaded component.
func PreflightArchive(archivePath string, archive ComponentArchive, events *Events) error {
	what := archive.Content.URL.Filename()
	prefix := string(archive.MemberPrefix)
	root := strings.TrimSuffix(prefix, "/")
	component := PackageComponentName(archive.Package, archive.Target)
	componentRoot := prefix + component + "/"
	metadataNames := map[string]bool{
		prefix + "rust-installer-version": true,
		prefix + "components":             true,
		componentRoot + "manifest.in":     true,
	}
	executableNames := map[string]bool{}
	for _, rel := range ComponentExecutables[archive.Package] {
		executableNames[componentRoot+rel] = true
	}

	members := map[string]archiveMember{}
	bodies := map[string][]byte{}
	headers := map[string][]byte{}
	metadataTotal := int64(0)
	index := 0

	err := tarwalk.Scan(archivePath, what, tarwalk.Limits{}, func(header *tar.Header,
		reader *tar.Reader, v7 bool) error {
		if index >= MaxMembers {
			return refuse("%s: more than %d members", what, MaxMembers)
		}
		index++
		member, err := describeMember(header, v7, what)
		if err != nil {
			return err
		}
		if err := requireCanonicalMember(member.name, what); err != nil {
			return err
		}
		if member.mode&0o6000 != 0 {
			// Every kind: a setgid directory or a privileged link is just as
			// outside the mode policy as a setuid file.
			return refuse("%s: setuid/setgid archive member %s", what, shownName(member.name))
		}
		if member.name == root {
			if _, present := members[member.name]; present {
				return refuse("%s: duplicate archive member %s", what, shownName(member.name))
			}
			if member.kind != directoryMember {
				return refuse("%s: top-level entry must be a directory", what)
			}
			members[member.name] = member
			return nil
		}
		if !strings.HasPrefix(member.name, prefix) {
			return refuse("%s: member outside the expected prefix: %s", what,
				shownName(member.name))
		}
		rel := member.name[len(prefix):]
		if _, present := members[member.name]; present {
			return refuse("%s: duplicate archive member %s", what, shownName(member.name))
		}
		switch member.kind {
		case symlinkMember:
			target, err := symlinkTarget(rel, member.linkname)
			if err != nil {
				return refuse("%s: archive link escapes its tree: %s", what,
					shownName(member.name))
			}
			memberTarget := prefix + target
			existing, present := members[memberTarget]
			if !present || existing.kind != regularMember {
				return refuse("%s: link target not an earlier regular member: %s", what,
					shownName(member.name))
			}
		case hardlinkMember:
			target, err := hardlinkTarget(prefix, member.linkname)
			if err != nil {
				return refuse("%s: archive link escapes its tree: %s", what,
					shownName(member.name))
			}
			memberTarget := prefix + target
			existing, present := members[memberTarget]
			if !present || existing.kind != regularMember {
				return refuse("%s: link target not an earlier regular member: %s", what,
					shownName(member.name))
			}
		case directoryMember:
		case regularMember:
			// The summed-size rule is not re-checked; see the file comment.
		default:
			return refuse("%s: special archive member %s", what, shownName(member.name))
		}
		members[member.name] = member

		if metadataNames[member.name] {
			body, err := readMetadata(reader, member, &metadataTotal, what)
			if err != nil {
				return err
			}
			bodies[member.name] = body
		} else if executableNames[member.name] {
			header := make([]byte, 20)
			count, err := io.ReadFull(reader, header)
			if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
				return refuse("%s: cannot read %s", what, member.name)
			}
			headers[member.name] = header[:count]
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := requireDirectoryAncestors(members, what); err != nil {
		return err
	}

	version, present := bodies[prefix+"rust-installer-version"]
	if !present {
		return refuse("%s: missing rust-installer-version", what)
	}
	if !bytes.Equal(version, []byte(RustInstallerVersion)) {
		return refuse("%s: rust-installer-version must be 3", what)
	}
	named, present := bodies[prefix+"components"]
	if !present {
		return refuse("%s: missing components", what)
	}
	if !bytes.Equal(named, []byte(component+"\n")) {
		return refuse("%s: component list is %s, expected exactly %s", what,
			pyBytesRepr(named), pyBytesRepr([]byte(component+"\n")))
	}
	manifestRaw, present := bodies[componentRoot+"manifest.in"]
	if !present {
		return refuse("%s: missing %s/manifest.in", what, component)
	}
	directives, err := ParseDirectives(manifestRaw, component, what)
	if err != nil {
		return err
	}
	if err := verifyMetadataMembership(members, prefix, component, directives, what); err != nil {
		return err
	}
	if err := verifyPayloadIdentity(members, headers, componentRoot, archive, what); err != nil {
		return err
	}
	return events.Record("preflight-ok " + what)
}

// describeMember normalizes the one header shape Python's tarfile normalizes: a
// directory's trailing slash is removed. A V7 AREGTYPE header whose name ends in
// "/" is a directory there (and its data was refused by the pre-pass), so it is
// one here too.
func describeMember(header *tar.Header, v7 bool, what string) (archiveMember, error) {
	member := archiveMember{name: header.Name, mode: header.Mode, size: header.Size,
		linkname: header.Linkname}
	name := header.Name
	directory := header.Typeflag == tar.TypeDir ||
		(v7 && header.Typeflag == tar.TypeReg && strings.HasSuffix(name, "/"))
	if directory {
		member.name = strings.TrimRight(name, "/")
		if member.name == "" {
			return member, errors.New("empty directory member")
		}
		member.kind = directoryMember
		return member, nil
	}
	switch header.Typeflag {
	case tar.TypeReg:
		member.kind = regularMember
	case tar.TypeSymlink:
		member.kind = symlinkMember
	case tar.TypeLink:
		member.kind = hardlinkMember
	default:
		member.kind = regularMember
		return member, refuse("%s: special archive member %s", what, shownName(header.Name))
	}
	return member, nil
}

// requireCanonicalMember is the source reader's canonical-name rule, applied to
// the normalized name: no leading slash, backslash or NUL, no trailing slash, and
// no empty, "." or ".." segment.
func requireCanonicalMember(name, what string) error {
	if strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\\x00") {
		return refuse("%s: non-canonical archive member %q", what, name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return refuse("%s: non-canonical archive member %q", what, name)
		}
	}
	return nil
}

// symlinkTarget resolves a symlink from its parent directory exactly as
// `PurePosixPath(parent) / target` does, then refuses any escape.
func symlinkTarget(rel, target string) (string, error) {
	resolved := resolvePosix(path.Dir(rel), target)
	if strings.HasPrefix(resolved, "/") || resolved == "" {
		return "", errors.New("escapes")
	}
	for _, segment := range strings.Split(resolved, "/") {
		if segment == "" || segment == ".." {
			return "", errors.New("escapes")
		}
	}
	return resolved, nil
}

// resolvePosix is `PurePosixPath(parent) / target`: "." and empty segments are
// collapsed, ".." is kept (so the caller can refuse it), and an absolute target
// replaces the parent.
func resolvePosix(parent, target string) string {
	if strings.HasPrefix(target, "/") {
		return normalizePosix(target)
	}
	return normalizePosix(parent + "/" + target)
}

func normalizePosix(value string) string {
	absolute := strings.HasPrefix(value, "/")
	parts := make([]string, 0, 8)
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." {
			continue
		}
		parts = append(parts, segment)
	}
	result := strings.Join(parts, "/")
	if absolute {
		return "/" + result
	}
	return result
}

// hardlinkTarget applies the prefix rule before resolution; a hard link names a
// member path, not a path relative to itself.
func hardlinkTarget(prefix, target string) (string, error) {
	if !strings.HasPrefix(target, prefix) {
		return "", errors.New("outside prefix")
	}
	resolved := normalizePosix(target[len(prefix):])
	if resolved == "" || strings.HasPrefix(resolved, "/") {
		return "", errors.New("escapes")
	}
	for _, segment := range strings.Split(resolved, "/") {
		if segment == "" || segment == ".." {
			return "", errors.New("escapes")
		}
	}
	return resolved, nil
}

func requireDirectoryAncestors(members map[string]archiveMember, what string) error {
	for name := range members {
		parts := strings.Split(name, "/")
		for index := 1; index < len(parts); index++ {
			ancestor, present := members[strings.Join(parts[:index], "/")]
			if present && ancestor.kind != directoryMember {
				return refuse("%s: %s has a non-directory ancestor %s", what,
					shownName(name), shownName(ancestor.name))
			}
		}
	}
	return nil
}

func readMetadata(reader io.Reader, member archiveMember, total *int64, what string) ([]byte, error) {
	if member.kind != regularMember || member.size > MaxMetadataFileBytes {
		return nil, refuse("%s: %s is not a bounded regular file", what,
			shownName(member.name))
	}
	*total += member.size
	if *total > MaxMetadataTotalBytes {
		return nil, refuse("%s: rust-installer metadata exceeds %d bytes", what,
			MaxMetadataTotalBytes)
	}
	body := make([]byte, member.size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, refuse("%s: cannot read %s", what, member.name)
	}
	return body, nil
}

// Directive is one `file:` or `dir:` line of manifest.in.
type Directive struct {
	Kind string
	Path string
}

// ParseDirectives applies the strict manifest.in grammar.
func ParseDirectives(raw []byte, component, what string) ([]Directive, error) {
	if bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		return nil, refuse("%s: leading BOM in %s/manifest.in", what, component)
	}
	if !utf8.Valid(raw) {
		return nil, refuse("%s: %s/manifest.in is not valid UTF-8", what, component)
	}
	text := string(raw)
	if !strings.HasSuffix(text, "\n") || strings.ContainsAny(text, "\r\x00") {
		return nil, refuse("%s: %s/manifest.in must be LF-terminated lines", what, component)
	}
	lines := strings.Split(text, "\n")
	lines = lines[:len(lines)-1]
	if len(lines) == 0 || len(lines) > MaxMetadataLines {
		return nil, refuse("%s: %s/manifest.in line count out of bounds", what, component)
	}
	directives := make([]Directive, 0, len(lines))
	seen := map[[2]string]bool{}
	for _, line := range lines {
		if len([]byte(line)) > MaxLineBytes {
			return nil, refuse("%s: %s/manifest.in line too long", what, component)
		}
		if err := RejectControlCharacters(line, what+"."+component+"/manifest.in"); err != nil {
			return nil, err
		}
		kind := ""
		for _, prefix := range []string{"file:", "dir:"} {
			if strings.HasPrefix(line, prefix) {
				kind = prefix
				break
			}
		}
		if kind == "" {
			return nil, refuse("%s: unknown directive in %s/manifest.in: %q", what,
				component, firstRunes(line, 32))
		}
		rel := line[len(kind):]
		if rel == "" || strings.HasPrefix(rel, "/") || strings.ContainsAny(rel, "\\:") ||
			hasBadSegment(rel) {
			return nil, refuse("%s: non-canonical directive path in %s/manifest.in: %q",
				what, component, rel)
		}
		key := [2]string{kind, rel}
		if seen[key] {
			return nil, refuse("%s: duplicate directive in %s/manifest.in: %s", what,
				component, line)
		}
		seen[key] = true
		directives = append(directives, Directive{Kind: kind, Path: rel})
	}
	files := map[string]bool{}
	dirs := map[string]bool{}
	for _, directive := range directives {
		if directive.Kind == "file:" {
			files[directive.Path] = true
		} else {
			dirs[directive.Path] = true
		}
	}
	for rel := range files {
		parents := strings.Split(rel, "/")
		for index := 1; index < len(parents); index++ {
			if files[strings.Join(parents[:index], "/")] {
				return nil, refuse("%s: overlapping directives for %s", what, rel)
			}
		}
	}
	for outer := range dirs {
		for inner := range dirs {
			if inner != outer && strings.HasPrefix(inner, outer+"/") {
				return nil, refuse("%s: overlapping directory directives %s and %s", what,
					outer, inner)
			}
		}
		if files[outer] {
			return nil, refuse("%s: %s is both file: and dir:", what, outer)
		}
	}
	for rel := range files {
		for outer := range dirs {
			if strings.HasPrefix(rel, outer+"/") {
				return nil, refuse("%s: file %s is covered twice (directory %s)", what, rel,
					outer)
			}
		}
	}
	return directives, nil
}

func hasBadSegment(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func firstRunes(text string, count int) string {
	runes := []rune(text)
	if len(runes) <= count {
		return text
	}
	return string(runes[:count])
}

func verifyMetadataMembership(members map[string]archiveMember, prefix, component string,
	directives []Directive, what string) error {
	root := prefix + component + "/"
	files := map[string]bool{}
	for name, member := range members {
		if strings.HasPrefix(name, root) && name != root+"manifest.in" &&
			member.kind != directoryMember {
			files[name] = true
		}
	}
	covered := map[string]bool{}
	for _, directive := range directives {
		name := root + directive.Path
		member, present := members[name]
		if !present {
			return refuse("%s: directive names a missing payload path: %s", what,
				directive.Path)
		}
		if directive.Kind == "file:" {
			if member.kind == directoryMember {
				return refuse("%s: file directive names a directory: %s", what,
					directive.Path)
			}
			covered[name] = true
			continue
		}
		if member.kind != directoryMember {
			return refuse("%s: dir directive names a non-directory: %s", what,
				directive.Path)
		}
		for other := range files {
			if strings.HasPrefix(other, name+"/") {
				covered[other] = true
			}
		}
	}
	for name := range files {
		if !covered[name] {
			return refuse("%s: payload path not covered by manifest.in: %s", what, name)
		}
	}
	// Leaf coverage is the invariant: an unreferenced directory (upstream
	// rust-std carries an empty lib/self-contained) is never installed.
	return nil
}

func verifyPayloadIdentity(members map[string]archiveMember, headers map[string][]byte,
	componentRoot string, archive ComponentArchive, what string) error {
	if archive.Package == "rust-std" {
		stdRoot := componentRoot + "lib/rustlib/" + string(archive.Target) + "/lib/"
		found := false
		for name := range members {
			if strings.HasPrefix(name, stdRoot) && strings.HasSuffix(name, ".rlib") {
				found = true
				break
			}
		}
		if !found {
			return refuse("%s: no %s standard library in the archive", what, archive.Target)
		}
		return nil
	}
	arch := AMD64
	if strings.HasPrefix(string(archive.Target), "aarch64") {
		arch = ARM64
	}
	for _, rel := range ComponentExecutables[archive.Package] {
		header, present := headers[componentRoot+rel]
		if !present {
			return refuse("%s: missing executable %s", what, rel)
		}
		machine, err := CheckELF(header)
		if err != nil {
			if errors.Is(err, errNotELF) {
				return refuse("%s: %s is not an ELF binary", what, rel)
			}
			if errors.Is(err, errNotELF64LE) {
				return refuse("%s: %s is not a 64-bit little-endian ELF binary", what, rel)
			}
			return err
		}
		if machine != arch.ELFMachine() {
			return refuse("%s: %s targets ELF machine %d, expected %d", what, rel, machine,
				arch.ELFMachine())
		}
	}
	return nil
}

var (
	errNotELF     = errors.New("not an ELF binary")
	errNotELF64LE = errors.New("not a 64-bit little-endian ELF binary")
)

// CheckELF reads the first 20 bytes of an ELF image and returns e_machine.
func CheckELF(header []byte) (uint16, error) {
	if len(header) < 20 || !bytes.HasPrefix(header, []byte("\x7fELF")) {
		return 0, errNotELF
	}
	if header[4] != 2 || header[5] != 1 {
		return 0, errNotELF64LE
	}
	return uint16(header[18]) | uint16(header[19])<<8, nil
}

func shownName(name string) string {
	if len(name) <= 80 {
		return name
	}
	return name[:77] + "..."
}

// pyBytesRepr is Python's bytes repr for the short ASCII values these
// diagnostics name.
func pyBytesRepr(raw []byte) string {
	var builder strings.Builder
	builder.WriteString("b'")
	for _, b := range raw {
		switch b {
		case '\\':
			builder.WriteString("\\\\")
		case '\'':
			builder.WriteString("\\'")
		case '\n':
			builder.WriteString("\\n")
		case '\r':
			builder.WriteString("\\r")
		case '\t':
			builder.WriteString("\\t")
		default:
			if b >= 0x20 && b < 0x7f {
				builder.WriteByte(b)
			} else {
				const hex = "0123456789abcdef"
				builder.WriteString("\\x")
				builder.WriteByte(hex[b>>4])
				builder.WriteByte(hex[b&0xf])
			}
		}
	}
	builder.WriteString("'")
	return builder.String()
}
