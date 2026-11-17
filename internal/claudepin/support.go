package claudepin

import (
	"os"
	"strconv"
	"syscall"

	"github.com/gregwebs/agent-vm-images/internal/strictjson"
)

// permissionBits is the file's whole mode word, special bits included: `Perm()`
// alone would hide a setuid bit, which a reviewed payload never carries.
//
// The mask is the numeric 07777 of the underlying stat mode: Go's `ModeSetuid` and
// friends are its own high bits, not the S_ISUID/S_ISGID a kernel reports, so masking
// a syscall mode with them would compare two different numberings.
func permissionBits(info os.FileInfo) os.FileMode {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return os.FileMode(stat.Mode & 0o7777)
	}
	return info.Mode().Perm()
}

// ownerOf is the file's owning uid and gid, where the platform reports them.
func ownerOf(info interface{ Sys() any }) ([2]int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]int{}, false
	}
	return [2]int{int(stat.Uid), int(stat.Gid)}, true
}

// The strict-JSON entry points, so this package has one JSON policy: the shared
// decoder refuses a duplicate key, a trailing document, a fraction and an exponent.
func strictjsonDecode(raw []byte, what string) (any, error) {
	value, err := strictjson.Decode(raw, what)
	return value, err
}

func strictjsonObject(value any, what string) (map[string]any, error) {
	members, ok := value.(map[string]any)
	if !ok {
		return nil, Refuse("%s must be a JSON object", what)
	}
	return members, nil
}

func strictjsonText(value any, what string) (string, error) {
	text, ok := value.(string)
	if !ok || text == "" {
		return "", Refuse("%s must be a non-empty string", what)
	}
	return text, nil
}

// itoa keeps a diagnostic free of a dependency on fmt for one number.
func itoa(value int64) string { return strconv.FormatInt(value, 10) }
