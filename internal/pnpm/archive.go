package pnpm

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The three ceilings are different bounds and each one stops a different attack:
//
//   - MaxArchiveBytes bounds the *compressed* archive, so a response or a file
//     that is not the reviewed tarball is never read into memory;
//   - MaxExpandedBytes bounds the sum of the member sizes the archive claims, so
//     a compression bomb cannot fill the disk the build runs on;
//   - MaxMembers bounds how many members an archive may carry, so a flood of tiny
//     entries cannot exhaust the inode table.
//
// The reviewed tarball (pnpm 11.11.0) is far inside all three: the numbers leave a
// wide margin for an upstream such as a future release growing, and a pin that
// outgrew them would refuse rather than silently change the contract.
const (
	MaxArchiveBytes  = 64 << 20
	MaxExpandedBytes = 512 << 20
	MaxMembers       = 20000
)

// upstreamFiles are the entries the distribution must carry as regular non-empty
// files: the two entrypoints the launcher execs, the bundle whose digest the pin
// records, and the upstream license.
var upstreamFiles = []string{"bin/pnpm.mjs", "bin/pnpx.mjs", "dist/pnpm.mjs", "LICENSE"}

// extract authenticates nothing: the caller has already compared the archive
// bytes with the pin. It validates the whole member inventory before writing
// anything, then unpacks it under destination and returns the `package/` tree.
//
// The member rules are the security property, not an artifact of one library:
// every member is under a single `package/` prefix, there is no `..` and no
// absolute name, no name appears twice, and only regular files and directories
// are allowed. Links and device nodes are refused outright, which is what makes
// the published tree immutable without an ownership audit of every member.
func extract(archivePath, destination string, pin Pin) (string, error) {
	members, err := archiveMembers(archivePath)
	if err != nil {
		return "", err
	}
	if err := unpack(archivePath, destination, members); err != nil {
		return "", err
	}
	packageDir := filepath.Join(destination, "package")
	if err := checkPackage(packageDir, pin); err != nil {
		return "", err
	}
	return packageDir, nil
}

// archiveMembers is the validation pass. Nothing is written until the whole
// inventory has been accepted, so a hostile member cannot leave a partial tree
// behind for a later step to publish, and the two resource ceilings are enforced
// here -- before the first byte reaches the disk.
func archiveMembers(archivePath string) ([]string, error) {
	stream, closeArchive, err := openArchive(archivePath)
	if err != nil {
		return nil, err
	}
	defer closeArchive()
	reader := tar.NewReader(stream)
	seen := make(map[string]bool)
	var members []string
	var expanded int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return members, nil
		}
		if err != nil {
			return nil, refuse("the archive is unreadable: %s", err)
		}
		// The reader already spells the legacy AREGTYPE as TypeReg or TypeDir, so
		// the allowed set here is the archive's own closed set.
		if !safeMember(header.Name) || seen[header.Name] ||
			(header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir) {
			return nil, refuseText("unsafe archive member: " + header.Name)
		}
		seen[header.Name] = true
		members = append(members, header.Name)
		if len(members) > MaxMembers {
			return nil, refuse("the archive carries more than %d members", MaxMembers)
		}
		expanded += header.Size
		if expanded > MaxExpandedBytes {
			return nil, refuse("the archive expands past the %d byte ceiling", MaxExpandedBytes)
		}
	}
}

// unpack writes the validated members. It reads the archive a second time, so a
// member this pass names is exactly the member the first pass accepted.
func unpack(archivePath, scratch string, members []string) error {
	wanted := make(map[string]bool, len(members))
	for _, name := range members {
		wanted[name] = true
	}
	stream, closeArchive, err := openArchive(archivePath)
	if err != nil {
		return err
	}
	defer closeArchive()
	reader := tar.NewReader(stream)
	written := 0
	for written < len(wanted) {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return refuse("the archive is unreadable: %s", err)
		}
		if !wanted[header.Name] {
			continue
		}
		written++
		destination := filepath.Join(scratch, filepath.FromSlash(header.Name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o755); err != nil {
				return refuseCause(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return refuseCause(err)
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			if err := writeMember(destination, mode, reader); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeMember(destination string, mode os.FileMode, reader io.Reader) error {
	handle, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return refuseCause(err)
	}
	if _, err := io.Copy(handle, reader); err != nil {
		_ = handle.Close()
		return refuseCause(err)
	}
	if err := handle.Close(); err != nil {
		return refuseCause(err)
	}
	return nil
}

func openArchive(archivePath string) (io.Reader, func(), error) {
	handle, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, refuseCause(err)
	}
	uncompressed, err := gzip.NewReader(handle)
	if err != nil {
		_ = handle.Close()
		return nil, nil, refuse("the archive is unreadable: %s", err)
	}
	return uncompressed, func() {
		_ = uncompressed.Close()
		_ = handle.Close()
	}, nil
}

// safeMember is the name rule. memberParts reproduces what the reviewed rule was
// measured against, Path(member).parts: repeated separators and `.` components
// are dropped, so `package//bin` and `package/./bin` are the same member name and
// `..` survives as a component.
func safeMember(name string) bool {
	parts := memberParts(name)
	if len(parts) == 0 || parts[0] != "package" {
		return false
	}
	if strings.HasPrefix(name, "/") || containsString(parts, "..") {
		return false
	}
	return true
}

func memberParts(name string) []string {
	var parts []string
	for _, element := range strings.Split(name, "/") {
		if element == "" || element == "." {
			continue
		}
		parts = append(parts, element)
	}
	if strings.HasPrefix(name, "/") {
		return append([]string{"/"}, parts...)
	}
	return parts
}

// checkPackage proves the extracted tree is the reviewed package and not merely
// a well-formed tarball: the selection, the four upstream files, the MIT license
// and the pristine bundle digest.
func checkPackage(packageDir string, pin Pin) error {
	raw, err := os.ReadFile(filepath.Join(packageDir, "package.json"))
	if err != nil {
		return refuseCause(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return refuseText("package selection mismatch")
	}
	name, _ := metadata["name"].(string)
	version, _ := metadata["version"].(string)
	if name != "pnpm" || version != pin.Version {
		return refuseText("package selection mismatch")
	}
	for _, name := range upstreamFiles {
		info, err := os.Lstat(filepath.Join(packageDir, filepath.FromSlash(name)))
		// Lstat rather than Stat: a link to a regular file is not the upstream
		// file, and the member rules already refuse links.
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return refuseText("missing regular upstream file: " + name)
		}
	}
	license, err := os.ReadFile(filepath.Join(packageDir, "LICENSE"))
	if err != nil {
		return refuseCause(err)
	}
	if !strings.Contains(string(license), "MIT") {
		return refuseText("upstream MIT license missing")
	}
	bundle, err := os.ReadFile(filepath.Join(packageDir, "dist/pnpm.mjs"))
	if err != nil {
		return refuseCause(err)
	}
	if Sum256(bundle) != pin.BundleSHA256 {
		return refuseText("pristine bundle SHA-256 mismatch")
	}
	return nil
}
