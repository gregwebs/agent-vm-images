package claudepin

import (
	"strconv"
	"syscall"

	"github.com/gregwebs/agent-vm-images/internal/strictjson"
)

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
