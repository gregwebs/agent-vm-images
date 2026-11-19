package dsh

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// TestKillAfterMatchesTheReviewedRunners binds the one bound this package declares to
// the `--kill-after` the shell envelopes actually pass. `cancelGrace` is derived from
// it, so a drift here would let a cancelled hook return while the manager it started
// is still resolving inside `timeout`'s own escalation.
func TestKillAfterMatchesTheReviewedRunners(t *testing.T) {
	for _, name := range []string{"run-pnpm.sh", "run-report.sh"} {
		path := filepath.Join("..", "..", "images", "recipe-contract", name)
		source, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("%s is not beside this package: %v", name, err)
		}
		match := regexp.MustCompile(`--kill-after=(\d+)`).FindStringSubmatch(string(source))
		if match == nil {
			t.Fatalf("%s no longer passes --kill-after", name)
		}
		seconds, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatal(err)
		}
		if time.Duration(seconds)*time.Second != killAfter {
			t.Fatalf("%s allows %ds, this package assumes %s", name, seconds, killAfter)
		}
	}
	if cancelGrace <= killAfter {
		t.Fatalf("cancelGrace %s does not outlast the envelope's %s backstop", cancelGrace, killAfter)
	}
}
