package claudepin

import (
	"encoding/binary"
	evidencepkg "github.com/gregwebs/agent-vm-images/internal/evidence"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Verify proves everything the committed directory proves about the requested
// version: the evidence digests, the key's single primary, the detached signature,
// and the signed manifest's own selection for both maintained platforms.
func (p Pin) Verify(version string) (VerifiedPin, error) { return p.VerifyWith(version, nil) }

// VerifyWith is Verify with a warning sink: a keyring this run could not clean up
// is a diagnostic, not a refusal, but it must still be visible.
func (p Pin) VerifyWith(version string, warn func(format string, args ...any)) (VerifiedPin, error) {
	requested, err := RequireVersion(version, "requested version")
	if err != nil {
		return VerifiedPin{}, err
	}
	if requested != p.Version {
		return VerifiedPin{}, Refuse("%s is not the pinned Claude Code version %s; "+
			"an exact but unreviewed version needs a reviewed pin", requested, p.Version)
	}
	manifestRaw, err := p.EvidenceBytes(p.Manifest.Path, p.Manifest.SHA256)
	if err != nil {
		return VerifiedPin{}, err
	}
	signatureRaw, err := p.EvidenceBytes(p.Signature.Path, p.Signature.SHA256)
	if err != nil {
		return VerifiedPin{}, err
	}
	keyRaw, err := p.EvidenceBytes(p.Key.Path, p.Key.SHA256)
	if err != nil {
		return VerifiedPin{}, err
	}
	signature, err := VerifySignature(keyRaw, p.Key.Fingerprint, manifestRaw, signatureRaw, warn)
	if err != nil {
		return VerifiedPin{}, err
	}
	manifest, err := ParseManifest(manifestRaw)
	if err != nil {
		return VerifiedPin{}, err
	}
	if manifest.Version != p.Version {
		return VerifiedPin{}, Refuse("the signed manifest names %s, but the pin names %s",
			manifest.Version, p.Version)
	}
	for token, pinned := range p.Executables {
		signed, ok := manifest.Executables[token]
		if !ok || signed.SHA256 != pinned.SHA256 || signed.Size != pinned.Size {
			return VerifiedPin{}, Refuse("the signed manifest does not carry the pinned %s "+
				"executable: manifest %s, pin (%s, %d)", token, describeSigned(signed),
				pinned.SHA256, pinned.Size)
		}
	}
	return VerifiedPin{Pin: p, Signature: signature, Manifest: manifest}, nil
}

func describeSigned(signed SignatureID) string {
	if signed.SHA256 == "" && signed.Size == 0 {
		return "nil"
	}
	return signed.SHA256 + " " + itoa(signed.Size) + " bytes"
}

// ParseManifest is the security projection of the signed manifest; other content is
// ignored.
func ParseManifest(raw []byte) (Manifest, error) {
	document, err := strictjsonDecode(raw, "the signed Claude manifest")
	if err != nil {
		return Manifest{}, err
	}
	members, err := strictjsonObject(document, "the signed Claude manifest")
	if err != nil {
		return Manifest{}, err
	}
	version, err := strictjsonText(members["version"], "the signed Claude manifest version")
	if err != nil || !versionPattern.MatchString(version) {
		return Manifest{}, Refuse("the signed Claude manifest names no exact version: %q",
			members["version"])
	}
	platforms, err := strictjsonObject(members["platforms"],
		"the signed Claude manifest platforms")
	if err != nil {
		return Manifest{}, err
	}
	executables := map[string]SignatureID{}
	for _, token := range tokens() {
		entry, err := strictjsonObject(platforms[token], "the signed Claude manifest "+token)
		if err != nil {
			return Manifest{}, Refuse("the signed Claude manifest has no %s platform", token)
		}
		if binary, ok := entry["binary"].(string); !ok || binary != "claude" {
			return Manifest{}, Refuse("the signed Claude manifest %s binary is %v, not claude",
				token, entry["binary"])
		}
		checksum, ok := entry["checksum"].(string)
		if !ok || !sha256Pattern.MatchString(checksum) {
			return Manifest{}, Refuse("the signed Claude manifest %s checksum is not 64 "+
				"lower-case hex characters", token)
		}
		size, ok := entry["size"].(int64)
		if !ok || size <= 0 {
			return Manifest{}, Refuse("the signed Claude manifest %s size is not a "+
				"positive integer", token)
		}
		executables[token] = SignatureID{SHA256: checksum, Size: size}
	}
	manifest := Manifest{Version: version, Executables: executables}
	if commit, present := members["commit"]; present && commit != nil {
		text, ok := commit.(string)
		if !ok {
			return Manifest{}, Refuse("the signed Claude manifest commit is not a string")
		}
		manifest.Commit = text
	}
	if buildDate, present := members["buildDate"]; present && buildDate != nil {
		text, ok := buildDate.(string)
		if !ok {
			return Manifest{}, Refuse("the signed Claude manifest buildDate is not a string")
		}
		manifest.BuildDate = text
	}
	return manifest, nil
}

// CheckELF proves the first 20 bytes are a 64-bit little-endian ELF for `token`.
//
// Checked without executing anything: a shell script that hashes correctly is not
// this release's executable, and the native installer must never run it.
func CheckELF(path, token string) error {
	machine, ok := ELFMachines[token]
	if !ok {
		return Refuse("%s is not a maintained Claude Code platform", token)
	}
	file, err := evidencepkg.OpenNoFollow(path)
	if err != nil {
		return Refuse("%s cannot be read: %s", path, err)
	}
	defer func() { _ = file.Close() }()
	header := make([]byte, ELFHeaderBytes)
	read, err := file.Read(header)
	if err != nil && read < ELFHeaderBytes {
		return Refuse("%s cannot be read: %s", path, err)
	}
	if read < ELFHeaderBytes {
		return Refuse("%s is shorter than an ELF header", path)
	}
	if string(header[:4]) != "\x7fELF" {
		return Refuse("%s is not an ELF object", path)
	}
	if header[4] != 2 {
		return Refuse("%s is not a 64-bit ELF object", path)
	}
	if header[5] != 1 {
		return Refuse("%s is not a little-endian ELF object", path)
	}
	if found := int(binary.LittleEndian.Uint16(header[18:20])); found != machine {
		return Refuse("%s targets ELF machine %d, not %d (%s)", path, found, machine, token)
	}
	return nil
}

// Preflight proves a downloaded file is the pinned bytes and an executable for
// this token. It runs before anything is placed.
func (v VerifiedPin) Preflight(token, path string) error {
	pinned, err := v.Pin.Executable(token)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Refuse("no downloaded executable at %s", path)
		}
		return Refuse("cannot stat %s: %s", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Refuse("%s is not a regular file", path)
	}
	digest, size, err := evidencepkg.FileDigest(path, MaxExecutableBytes, "the downloaded executable")
	if err != nil {
		return err
	}
	if digest != pinned.SHA256 {
		return Refuse("the downloaded executable sha256 %s does not match the pinned %s",
			digest, pinned.SHA256)
	}
	if size != pinned.Size {
		return Refuse("the downloaded executable is %d bytes, but the pin records %d",
			size, pinned.Size)
	}
	return CheckELF(path, token)
}

// CheckInstalled proves the pinned payload is exactly where and what the native
// layout says.
//
// The ELF check is not repeated: it ran in the install RUN over these bytes, and the
// digest match is what binds a test fixture's shell script to this layout.
func (v VerifiedPin) CheckInstalled(token, prefix string, uid, gid int) error {
	pinned, err := v.Pin.Executable(token)
	if err != nil {
		return err
	}
	version := v.Pin.Version
	share := filepath.Join(prefix, ".local/share/claude")
	versions := filepath.Join(share, "versions")
	installed := filepath.Join(versions, version)
	for _, directory := range []struct{ path, what string }{
		{filepath.Join(prefix, ".local/bin"), ".local/bin"},
		{share, ".local/share/claude"},
		{versions, ".local/share/claude/versions"},
	} {
		if err := notWritableByOthers(directory.path, directory.what); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(versions)
	if err != nil {
		return Refuse("cannot list %s: %s", versions, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	if len(names) != 1 || names[0] != version {
		return Refuse("%s holds [%s], not exactly [%s]", versions,
			strings.Join(names, " "), version)
	}
	info, err := os.Lstat(installed)
	if err != nil {
		return Refuse("the installed payload %s cannot be read: %s", installed, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Refuse("the installed payload %s is not a regular file", installed)
	}
	// The whole permission word, not just the nine rwx bits: a setuid or setgid
	// payload runs with privileges the review never granted.
	if mode := permissionBits(info); mode != 0o755 {
		return Refuse("the installed payload %s has mode %04o, not 0755", installed, mode)
	}
	if owner, ok := ownerOf(info); ok && owner != [2]int{uid, gid} {
		return Refuse("the installed payload %s is owned %d:%d, not %d:%d", installed,
			owner[0], owner[1], uid, gid)
	}
	digest, size, err := evidencepkg.FileDigest(installed, MaxExecutableBytes, "the installed payload")
	if err != nil {
		return err
	}
	if digest != pinned.SHA256 {
		return Refuse("the installed payload sha256 %s does not match the pinned %s",
			digest, pinned.SHA256)
	}
	if size != pinned.Size {
		return Refuse("the installed payload is %d bytes, but the pin records %d",
			size, pinned.Size)
	}
	launcher := filepath.Join(prefix, ".local/bin/claude")
	launcherInfo, err := os.Lstat(launcher)
	if err != nil {
		return Refuse("the launcher %s cannot be read: %s", launcher, err)
	}
	if launcherInfo.Mode()&os.ModeSymlink == 0 {
		return Refuse("the launcher %s is not a symlink", launcher)
	}
	target, err := os.Readlink(launcher)
	if err != nil {
		return Refuse("the launcher %s cannot be read: %s", launcher, err)
	}
	if target != installed {
		return Refuse("the launcher %s points at %q, not %q", launcher, target, installed)
	}
	return nil
}

// CheckNativeState proves the offline native step's own state, as the image needs
// it to be.
func CheckNativeState(prefix string) error {
	path := filepath.Join(prefix, ".claude.json")
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Refuse("%s is missing", path)
		}
		return Refuse("%s cannot be read: %s", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Refuse("%s is not a regular file", path)
	}
	raw, err := evidencepkg.ReadBounded(path, MaxStateBytes, path)
	if err != nil {
		return err
	}
	document, err := strictjsonDecode(raw, path)
	if err != nil {
		return err
	}
	members, ok := document.(map[string]any)
	if !ok {
		return Refuse("%s is not a JSON object", path)
	}
	if method, ok := members["installMethod"].(string); !ok || method != "native" {
		return Refuse("%s records installMethod %v, not \"native\"", path, members["installMethod"])
	}
	// A JSON boolean false, not `"false"`, not 0 and not a missing key: the native
	// installer writes a boolean, and every other shape is a different state.
	if updates, ok := members["autoUpdates"].(bool); !ok || updates {
		return Refuse("%s records autoUpdates %v, not false", path, members["autoUpdates"])
	}
	for _, directory := range []struct{ path, what string }{
		{filepath.Join(prefix, ".claude/downloads"), ".claude/downloads"},
		{filepath.Join(prefix, ".cache/claude/staging"), ".cache/claude/staging"},
	} {
		if err := emptyOrAbsent(directory.path, directory.what); err != nil {
			return err
		}
	}
	return nil
}

func notWritableByOthers(path, what string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Refuse("%s is missing at %s", what, path)
		}
		return Refuse("%s cannot be read at %s: %s", what, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Refuse("%s at %s is not a real directory", what, path)
	}
	if mode := permissionBits(info); mode&0o022 != 0 {
		return Refuse("%s %s is group- or world-writable (mode %04o)", what, path, mode)
	}
	return nil
}

func emptyOrAbsent(path, what string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return Refuse("%s cannot be read at %s: %s", what, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Refuse("%s at %s is not a real directory", what, path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return Refuse("cannot list %s at %s: %s", what, path, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		sort.Strings(names)
		if len(names) > 5 {
			names = names[:5]
		}
		return Refuse("%s at %s is not empty: [%s]", what, path, strings.Join(names, " "))
	}
	return nil
}

// CheckReport is the `check` verb's transcript: what was proven, without claiming a
// fresh fetch.
func CheckReport(verified VerifiedPin) string {
	pin := verified.Pin
	lines := []string{
		"claude " + pin.Version + ": committed closure verified offline -- signature by " +
			verified.Signature.PrimaryFingerprint + " created " + verified.Signature.Created +
			", manifest commit " + verified.Manifest.Commit +
			", both platform executables pinned",
	}
	return strings.Join(lines, "\n") + "\n"
}
