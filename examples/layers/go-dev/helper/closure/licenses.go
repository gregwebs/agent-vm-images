package closure

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LicenseFile is one notice file a module ships.
type LicenseFile struct {
	Name   NoticePath
	SHA256 SHA256
}

// LicenseSection is one framed notice body: its identity, its recorded digest and
// the bytes themselves.
type LicenseSection struct {
	ID     ModuleID
	Name   NoticePath
	SHA256 SHA256
	Raw    []byte
}

// headerVersion reads the release the evidence file's header names.
func headerVersion(text, what string) (ReleaseVersion, error) {
	prefix := "# Third-party license files for " + Gopls + " "
	rest, found := strings.CutPrefix(text, prefix)
	if !found {
		return "", refuse("%s: unexpected header", what)
	}
	version, rest, found := strings.Cut(rest, " and its build closure.\n")
	if !found || version == "" || strings.ContainsAny(version, " \t\n") {
		return "", refuse("%s: unexpected header", what)
	}
	_ = rest
	return ReleaseVersion(version), nil
}

// parseLicenses reads the length-framed notice file.
//
// A duplicate frame used to survive: the loader indexed the sections by identity,
// so two frames for one notice collapsed to one and the set comparison still
// balanced (39 sections accepted against 38 reviewed notices, #188 G3). The
// duplicates are refused here, before anything is indexed.
func parseLicenses(raw []byte, what string) ([]LicenseSection, error) {
	text, err := decodeText(raw, what)
	if err != nil {
		return nil, err
	}
	release, err := headerVersion(text, what)
	if err != nil {
		return nil, err
	}
	header := []byte(fmt.Sprintf(licenseHeader, release))
	if !bytes.HasPrefix(raw, header) {
		return nil, refuse("%s: unexpected header", what)
	}
	sections := []LicenseSection{}
	seen := map[string]struct{}{}
	position := len(header)
	for position < len(raw) {
		end := bytes.IndexByte(raw[position:], '\n')
		if end < 0 {
			end = len(raw)
		} else {
			end += position
		}
		marker := string(raw[position:end])
		position = end + 1
		if marker == "" {
			if position >= len(raw) {
				break
			}
			return nil, refuse("%s: empty section separator", what)
		}
		fields := strings.Split(marker, " ")
		if len(fields) != 6 || fields[0] != "===" ||
			!strings.HasPrefix(fields[4], "bytes=") || !strings.HasPrefix(fields[5], "sha256=") {
			return nil, refuse("%s: malformed section header: %s", what, repr(marker))
		}
		path, err := parseModulePath(fields[1], what+" section")
		if err != nil {
			return nil, err
		}
		version, err := parseModuleVersion(fields[2], what+" section")
		if err != nil {
			return nil, err
		}
		name, err := parseNoticePath(fields[3], what+" section")
		if err != nil {
			return nil, err
		}
		sizeText := strings.TrimPrefix(fields[4], "bytes=")
		if !asciiDigits(sizeText) {
			return nil, refuse("%s: malformed byte count: %s", what, repr(marker))
		}
		// The count is read against the bytes that remain, not accumulated into an
		// int and then used as a slice bound. Two versions of this line were wrong
		// before: the sum and the bound both overflowed, so a 20-digit count panicked
		// with a slice-bounds error (status 2) where the Python reader refuses. A
		// count larger than the remaining bytes is the same truncated section.
		size, fits := parseBoundedSize(sizeText, len(raw)-position)
		digest, err := parseSHA256(strings.TrimPrefix(fields[5], "sha256="), what+" section")
		if err != nil {
			return nil, err
		}
		if !fits {
			return nil, refuse("%s: truncated section for %s %s", what, path, name)
		}
		body := raw[position : position+size]
		position += size
		if position >= len(raw) || raw[position] != '\n' {
			return nil, refuse("%s: section is not newline-terminated", what)
		}
		position++
		actual := sha256.Sum256(body)
		if hex.EncodeToString(actual[:]) != string(digest) {
			return nil, refuse("license evidence differs from the committed digest: %s %s",
				path, name)
		}
		section := LicenseSection{ID: ModuleID{Path: path, Version: version}, Name: name,
			SHA256: digest, Raw: body}
		key := section.ID.String() + "\x00" + string(name)
		if _, duplicate := seen[key]; duplicate {
			return nil, refuse("%s: duplicate notice section for %s %s", what, path, name)
		}
		seen[key] = struct{}{}
		sections = append(sections, section)
	}
	if len(sections) == 0 {
		return nil, refuse("%s: no license sections", what)
	}
	return sections, nil
}

// parseBoundedSize reads a decimal byte count that may not exceed limit. It
// saturates at the first digit that would pass the limit, so no arithmetic here
// can overflow whatever the file says.
func parseBoundedSize(text string, limit int) (int, bool) {
	value := 0
	for _, digit := range text {
		value = value*10 + int(digit-'0')
		if value > limit {
			return 0, false
		}
	}
	return value, true
}

// asciiDigits is Python's isdigit for the ASCII digits a byte count may use. The
// wider Unicode digit set Python's isdigit also accepts would reach int() and
// raise there instead of refusing; this reader refuses.
func asciiDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// reviewedNestedNotices is the reviewed nested layout for one module, if any.
func reviewedNestedNotices(module ModuleID) []string {
	return ReviewedNestedNotices[module]
}

// noticeName is the reviewed notice-file name set as one compiled pattern, read from
// the constant below rather than a second regular expression kept in step by hand.
var noticeName = regexp.MustCompile(NoticeNamePattern)

// CollectNotices reads one module directory's notice files from the module cache,
// keyed by canonical relative path.
//
// One rule for both consumers: the stager transports these bytes into the committed
// evidence (#252), and the installer re-reads the same files to prove the build used
// the bytes it published. Whether the resulting layout is *reviewed* is decided by
// the caller against ReviewedNestedNotices, not here. A module directory that is not
// there is missing evidence, never an empty set; a symlink in the tree is an
// unsupported layout rather than a file to follow.
func CollectNotices(directory string, module ModuleID) (map[NoticePath][]byte, error) {
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return nil, refuse("license evidence missing for %s: the cache has no %s", module,
			directory)
	}
	found := map[NoticePath][]byte{}
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return refuse("%s: unsupported license layout; a notice path is a symlink", module)
		}
		if entry.IsDir() || !noticeName.MatchString(entry.Name()) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		found[NoticePath(filepath.ToSlash(relative))] = raw
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}
