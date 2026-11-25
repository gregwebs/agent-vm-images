package toolchain

// The frozen `--events` log. Each line is written at the operation site, never
// reconstructed afterwards, so an interrupted run records exactly what had
// happened when it stopped. The frozen `install` rows assert the whole sequence;
// nothing compares it against a second implementation any more (#245).

import (
	"os"
)

// Events appends what happened. A nil *Events is a no-op recorder for unit tests.
type Events struct {
	path string
}

// NewEvents records to path. An empty path keeps the lines in memory only.
func NewEvents(path string) *Events { return &Events{path: path} }

// Record appends one line. The source reader opens, appends and closes per line;
// doing the same keeps a crash's last completed line on disk.
func (e *Events) Record(message string) error {
	if e == nil || e.path == "" {
		return nil
	}
	handle, err := os.OpenFile(e.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return adopt(err)
	}
	if _, err := handle.WriteString(message + "\n"); err != nil {
		_ = handle.Close()
		return adopt(err)
	}
	return adopt(handle.Close())
}
