#!/usr/bin/env bash
# Post-install sanity for the go-dev image. Runs from the Dockerfile as
# `RUN --network=none` with the verifier bundle mounted at /run/go-verify, and
# directly from a checkout whose /opt layout matches the image's.
#
# The first thing it does is verify the *installed* gopls against the committed
# closure (gopls-closure/): the binary's own build info must pair the pinned
# compiler with this image's architecture and report exactly the reviewed
# modules and their sums, the installed third-party licence file must carry the
# reviewed bytes, and the mode must be the world-readable one. A gopls built
# from a different selection, or one whose licence evidence drifted, fails here.
# The bundle always sits beside this script -- the read-only mount in the image,
# the checkout when it is run standalone -- so there is no mode that skips it.
#
# It then invokes every tool as the guest PATH resolves it (bare name, not an
# absolute path), so a tool missing from the Dockerfile's ENV PATH fails the
# build here rather than surfacing as command-not-found in the guest. Beyond
# the versions, each tool analyses a real throwaway module: `go` builds, vets
# and race-tests it (the race run is also what proves the C toolchain is
# usable, since `-race` needs cgo), `gofmt` must report it formatted,
# `golangci-lint` lints it, and `gopls check` reports its diagnostics. A binary
# that prints a version but cannot analyse code -- a truncated download, a
# missing shared library, a module-cache misconfiguration -- fails the build.
#
# HOME/GOCACHE/GOPATH point at a temporary tree so the smoke module's build
# cache is not baked into the image (the guest gets its own, under its own
# persistent home).
#
# Environment:
#   GO_VERSION              required; the exact channel the layer installed
#   GOLANGCI_LINT_VERSION   required; the release the layer installed
#   GOPLS_VERSION           required; the module version the layer installed
set -euo pipefail

: "${GO_VERSION:?GO_VERSION must be set to the pinned toolchain}"
: "${GOLANGCI_LINT_VERSION:?GOLANGCI_LINT_VERSION must be set to the pinned release}"
: "${GOPLS_VERSION:?GOPLS_VERSION must be set to the pinned module version}"
VERIFY_DIR=$(cd "${BASH_SOURCE[0]%/*}" && pwd)
export PYTHONDONTWRITEBYTECODE=1

# Validated by the helper: it refuses anything but the two Linux target names the
# closure is cross-built for, rather than treating `dpkg` output as a value it can
# pass through.
arch=$(dpkg --print-architecture)
/usr/bin/python3 "$VERIFY_DIR/gopls_closure.py" verify-installed "$VERIFY_DIR/gopls-closure" \
    --expect-gopls "$GOPLS_VERSION" --expect-go "$GO_VERSION" \
    --go-root /opt/go --arch "$arch" \
    --binary /opt/go-tools/bin/gopls --share-dir /opt/go-tools/share/gopls

test "$(go version | cut -d' ' -f3)" = "go${GO_VERSION}"

# Each tool's output is captured before it is matched, never piped into
# `grep -q`: grep exits at the first match, the tool's remaining writes get
# SIGPIPE, and `pipefail` would turn that into a failed build -- a false
# failure whose rate depends only on how much the tool prints (gofmt's help is
# long enough to hit it reliably).
gofmt_help="$(gofmt -h 2>&1 || true)"
grep -q 'usage: gofmt' <<<"$gofmt_help"
# GOLANGCI_LINT_VERSION is the guarded Dockerfile mirror of the repository pin,
# so verification follows the same bump rather than carrying another default.
golangci_lint_version="$(golangci-lint version)"
grep -q "golangci-lint has version ${GOLANGCI_LINT_VERSION} " <<<"$golangci_lint_version"
gopls_version="$(gopls version)"
grep -q "gopls ${GOPLS_VERSION}" <<<"$gopls_version"

# A clean module must analyse to an empty diagnostic set. gopls v0.23.0's `check`
# exits 0 whether or not it reports diagnostics (measured: a clean file and a
# seeded type error both exit 0), so the observation is the diagnostics -- but the
# status is still checked separately, because a `check` that could not run at all
# prints nothing either and would otherwise satisfy "no diagnostics". The output is
# captured in a file, not in a command substitution, because a substitution strips
# trailing newlines: `printf '\n'` would then look like empty output. Only an
# output file of size zero is "no diagnostics". Both directions are exercised
# against a fake gopls by `script/test/go-example.sh --self-test`, which reads this
# function from this file.
require_clean_diagnostics() { # FILE
    local file=$1 output status=0
    output=$(mktemp)
    gopls check "$file" >"$output" 2>&1 || status=$?
    if [ "$status" -ne 0 ]; then
        printf 'gopls check %s exited %s:\n' "$file" "$status" >&2
        cat "$output" >&2
        rm -f "$output"
        return 1
    fi
    if [ -s "$output" ]; then
        printf 'gopls check %s reported diagnostics for clean code:\n' "$file" >&2
        cat "$output" >&2
        rm -f "$output"
        return 1
    fi
    rm -f "$output"
    return 0
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

export HOME="$work/home"
export GOCACHE="$work/go-build"
export GOPATH="$work/gopath"
mkdir -p "$HOME"

cat > "$work/go.mod" <<EOF
module agent-vm-go-dev-smoke

go ${GO_VERSION}
EOF

cat > "$work/main.go" <<'GO'
package main

import "fmt"

func greeting() string { return "go-dev layer smoke OK" }

func main() {
	fmt.Println(greeting())
}
GO

cat > "$work/main_test.go" <<'GO'
package main

import "testing"

func TestGreeting(t *testing.T) {
	if got := greeting(); got != "go-dev layer smoke OK" {
		t.Fatalf("greeting() = %q", got)
	}
}
GO

(
    cd "$work"
    go vet ./...
    go run .
    # `-race` needs cgo, so this also proves the C toolchain is present and
    # usable rather than silently defaulted off with CGO_ENABLED=0.
    go test -race ./...
    test -z "$(gofmt -l .)"
    golangci-lint run
    # Success *and* an empty diagnostic set: see require_clean_diagnostics above.
    require_clean_diagnostics ./main.go
)

echo "== go-dev layer sanity OK =="
