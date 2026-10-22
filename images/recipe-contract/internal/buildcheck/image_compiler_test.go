//go:build recipe_contract_image_compiler

package buildcheck

import (
	"runtime"
	"strings"
	"testing"
)

// TestImageCompiler compares runtime.Version with this module's toolchain
// directive. The builder runs it with GOTOOLCHAIN=local, native GOOS/GOARCH and
// cleared target overrides, so auto-download cannot mask stale image contents.
func TestImageCompiler(t *testing.T) {
	directives := readGoMod(t)
	toolchain := strings.TrimPrefix(directives["toolchain"], "go")
	if toolchain == "" {
		t.Fatalf("go.mod has no toolchain directive: %v", directives)
	}
	language := directives["go"]
	if runtime.Version() != "go"+toolchain {
		// CI runs this with GOTOOLCHAIN=auto to assert the toolchain go.mod
		// selects. Auto upgrades a host older than the toolchain and cannot
		// downgrade a newer one, so this line is also how a runner whose Go is
		// newer than go.mod's toolchain reports itself.
		t.Fatalf("compiler is %s, want go%s (go.mod language minimum %s, toolchain %s). "+
			"Under GOTOOLCHAIN=auto a host Go newer than the toolchain cannot downgrade: "+
			"install the pinned toolchain on this runner, or bump go.mod's toolchain directive.",
			runtime.Version(), toolchain, language, directives["toolchain"])
	}
}
