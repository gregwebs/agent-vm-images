package document

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLargeJSONMappings(t *testing.T) {
	var source strings.Builder
	source.WriteByte('{')
	for index := 0; index < 80000; index++ {
		if index > 0 {
			source.WriteByte(',')
		}
		fmt.Fprintf(&source, `"k%d":1`, index)
	}
	source.WriteByte('}')
	payload := []byte(source.String())
	for _, plain := range []bool{false, true} {
		t.Run(fmt.Sprintf("plain=%t", plain), func(t *testing.T) {
			start := time.Now()
			var parseErr error
			allocations := testing.AllocsPerRun(1, func() { _, parseErr = readJSON(payload, plain) })
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			// Quadratic duplicate scans took several seconds per parse at this size.
			// The indexed/cursor paths have ample headroom even on slower CI hosts.
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("80000 JSON keys took %s (quadratic scan?)", elapsed)
			}
			if allocations > 1000000 {
				t.Errorf("%.0f allocations for 80000 JSON keys", allocations)
			}
		})
	}
}
