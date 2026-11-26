#!/usr/bin/env bash
# Vendor drift gate for an example layer's own Go module (ADR 0017).
#
# An example layer is built by its own pinned `golang:` stage, which compiles the
# layer's module with `GOFLAGS=-mod=vendor` and `--network=none`. The committed
# vendor tree is therefore the whole dependency closure of that build, and neither
# `go build` nor `go mod verify` authenticates its bytes: `go build` happily
# compiles an edited vendored file, and `go mod verify` only reads the module
# cache. Regenerating vendor from the pinned go.sum in a fresh cache and diffing it
# against the committed tree is the only gate that detects a local change.
#
# One gate for both layer modules (#187 slice 2, #249), and the module is required:
# a default would quietly gate one layer and report for both. The reviewed go.sum
# text is stated per module below, so a dependency change is a diff in a gate as
# well as in the module. The two modules are separate (ADR 0017); which of them the
# root module links is the root go.mod's decision, not this script's.
#
# Usage: script/test/example-helper-vendor.sh MODULE_DIR
set -euo pipefail

[ "$#" -eq 1 ] && [ -n "$1" ] || { echo "usage: $0 MODULE_DIR" >&2; exit 2; }
module_argument=$1
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/example-helper-vendor.XXXXXX")

cleanup() {
    status=$?
    # A module cache holds read-only directories. Make the scratch writable so the
    # removal cannot fail on permissions, and never hide a real failure: report the
    # scratch path that survived so the evidence is not lost.
    if [ -d "$scratch" ]; then
        chmod -R u+w "$scratch" 2>/dev/null || true
        if ! rm -rf "$scratch"; then
            echo "example-helper-vendor: cannot remove scratch $scratch" >&2
            [ "$status" -ne 0 ] || status=1
        fi
    fi
    exit "$status"
}
trap cleanup EXIT

# The reviewed closures, stated here so a dependency change is a diff in a gate as
# well as in the module. Both modules take the go-arg grammar ADR 0017 requires for
# every command plus the scalar decoder go-arg uses. The rust-dev module adds the
# TOML decoder its pin reader was measured against (docs/adr/0017-go-programs.md:
# go-toml/v2 v2.2.4, the newest release that matches TOML 1.0 with strict tables on
# eight of the nine grammar vectors -- the ninth, the TOML 1.1 `\e` escape, is a
# declared acceptance with a test) and ulikunitz/xz for its single-stream archive
# preflight. The go-dev module reads committed JSON and text only, so it takes
# nothing beyond the two (#249).
#
# Both go.sum files also carry lines for the two modules' *test* dependencies
# (testify, go-spew, go-difflib, yaml.v3): `go mod tidy` records them, they are not
# vendored and not compiled.
expected_sum_for() { # MODULE_DIR
    case "$1" in
        examples/layers/rust-dev/helper)
            cat <<'RUST_SUM'
github.com/alexflint/go-arg v1.6.1 h1:uZogJ6VDBjcuosydKgvYYRhh9sRCusjOvoOLZopBlnA=
github.com/alexflint/go-arg v1.6.1/go.mod h1:nQ0LFYftLJ6njcaee0sU+G0iS2+2XJQfA8I062D0LGc=
github.com/alexflint/go-scalar v1.2.0 h1:WR7JPKkeNpnYIOfHRa7ivM21aWAdHD0gEWHCx+WQBRw=
github.com/alexflint/go-scalar v1.2.0/go.mod h1:LoFvNMqS1CPrMVltza4LvnGKhaSpc3oyLEBUZVhhS2o=
github.com/davecgh/go-spew v1.1.1 h1:vj9j/u1bqnvCEfJOwUhtlOARqs3+rkHYY13jYWTU97c=
github.com/davecgh/go-spew v1.1.1/go.mod h1:J7Y8YcW2NihsgmVo/mv3lAwl/skON4iLHjSsI+c5H38=
github.com/pelletier/go-toml/v2 v2.2.4 h1:mye9XuhQ6gvn5h28+VilKrrPoQVanw5PMw/TB0t5Ec4=
github.com/pelletier/go-toml/v2 v2.2.4/go.mod h1:2gIqNv+qfxSVS7cM2xJQKtLSTLUE9V8t9Stt+h56mCY=
github.com/pmezard/go-difflib v1.0.0 h1:4DBwDE0NGyQoBHbLQYPwSUPoCMWR5BEzIk/f1lZbAQM=
github.com/pmezard/go-difflib v1.0.0/go.mod h1:iKH77koFhYxTK1pcRnkKkqfTogsbg7gZNVY4sRDYZ/4=
github.com/stretchr/testify v1.2.2/go.mod h1:a8OnRcib4nhh0OaRAV+Yts87kKdq0PP7pXfy6kDkUVs=
github.com/stretchr/testify v1.7.0 h1:nwc3DEeHmmLAfoZucVR881uASk0Mfjw8xYJ99tb5CcY=
github.com/stretchr/testify v1.7.0/go.mod h1:6Fq8oRcR53rry900zMqJjRRixrwX3KX962/h/Wwjteg=
github.com/ulikunitz/xz v0.5.17 h1:flR0y/x1hgM8EGV1AW3Xll6T413G0glV8UfBwR617V4=
github.com/ulikunitz/xz v0.5.17/go.mod h1:H9Rt/W6/Qj27PGauhQc6nfCDy7vHpzsOThBSaYDoEhw=
gopkg.in/yaml.v3 v3.0.1 h1:fxVm/GzAzEWqLHuvctI91KS9hhNmmWOoWu0XTYJS7CA=
gopkg.in/yaml.v3 v3.0.1/go.mod h1:K4uyk7z7BCEPqu6E+C64Yfv1cQ7kz7rIZviUmN+EgEM=
RUST_SUM
            ;;
        examples/layers/go-dev/helper)
            cat <<'GO_SUM'
github.com/alexflint/go-arg v1.6.1 h1:uZogJ6VDBjcuosydKgvYYRhh9sRCusjOvoOLZopBlnA=
github.com/alexflint/go-arg v1.6.1/go.mod h1:nQ0LFYftLJ6njcaee0sU+G0iS2+2XJQfA8I062D0LGc=
github.com/alexflint/go-scalar v1.2.0 h1:WR7JPKkeNpnYIOfHRa7ivM21aWAdHD0gEWHCx+WQBRw=
github.com/alexflint/go-scalar v1.2.0/go.mod h1:LoFvNMqS1CPrMVltza4LvnGKhaSpc3oyLEBUZVhhS2o=
github.com/davecgh/go-spew v1.1.1 h1:vj9j/u1bqnvCEfJOwUhtlOARqs3+rkHYY13jYWTU97c=
github.com/davecgh/go-spew v1.1.1/go.mod h1:J7Y8YcW2NihsgmVo/mv3lAwl/skON4iLHjSsI+c5H38=
github.com/pmezard/go-difflib v1.0.0 h1:4DBwDE0NGyQoBHbLQYPwSUPoCMWR5BEzIk/f1lZbAQM=
github.com/pmezard/go-difflib v1.0.0/go.mod h1:iKH77koFhYxTK1pcRnkKkqfTogsbg7gZNVY4sRDYZ/4=
github.com/stretchr/testify v1.2.2/go.mod h1:a8OnRcib4nhh0OaRAV+Yts87kKdq0PP7pXfy6kDkUVs=
github.com/stretchr/testify v1.7.0 h1:nwc3DEeHmmLAfoZucVR881uASk0Mfjw8xYJ99tb5CcY=
github.com/stretchr/testify v1.7.0/go.mod h1:6Fq8oRcR53rry900zMqJjRRixrwX3KX962/h/Wwjteg=
gopkg.in/yaml.v3 v3.0.1 h1:fxVm/GzAzEWqLHuvctI91KS9hhNmmWOoWu0XTYJS7CA=
gopkg.in/yaml.v3 v3.0.1/go.mod h1:K4uyk7z7BCEPqu6E+C64Yfv1cQ7kz7rIZviUmN+EgEM=
GO_SUM
            ;;
        *)
            echo "example-helper-vendor: no reviewed go.sum for $1" >&2
            return 2
            ;;
    esac
}

# The module's tracked files only: the extracted tree is what a fresh checkout
# would hand `go mod vendor`, so an untracked file cannot make the gate pass.
(cd "$root" && git ls-files -z -- "$module_argument") | tar --null -T - -cf - |
    tar -xf - -C "$scratch" || { echo 'example-helper-vendor: cannot extract the module tree' >&2; exit 1; }
module="$scratch/$module_argument"
[ -f "$module/go.mod" ] || { echo "example-helper-vendor: $module_argument is not a module" >&2; exit 1; }
cd "$module"

expected_sum=$(expected_sum_for "$module_argument") ||
    { echo 'example-helper-vendor: no reviewed closure for this module' >&2; exit 1; }
recorded_sum=$(cat go.sum) || { echo 'example-helper-vendor: cannot read go.sum' >&2; exit 1; }
[ "$recorded_sum" = "$expected_sum" ] || {
    echo 'example-helper-vendor: go.sum does not name the reviewed pin:' >&2
    printf '%s\n' "$recorded_sum" >&2
    exit 1
}

# An explicit environment: no workspace, no ambient flags, no private-module or
# insecure sources, and a module cache this invocation owns.
vendor_go() {
    env -u GOOS -u GOARCH -u GOEXPERIMENT \
        GOTOOLCHAIN=auto GOENV=off GOWORK=off GOFLAGS=-mod=mod \
        GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org \
        GOPRIVATE= GONOPROXY= GONOSUMDB= GOINSECURE= \
        GOMODCACHE="$scratch/modcache" go "$@"
}

vendor_go mod download
vendor_go mod verify

after_download=$(cat go.sum)
[ "$after_download" = "$expected_sum" ] || {
    echo 'example-helper-vendor: go.sum changed during download' >&2
    exit 1
}

vendor_go mod vendor -o "$scratch/vendor"

# diff_vendor is the gate. The mutation controls below call it with trees that
# must be rejected, so a diff that cannot fail is a gate that proves nothing.
diff_vendor() {
    for tree in "$1" "$module/vendor"; do
        unexpected=$(find "$tree" ! -type f ! -type d -print) || return 1
        executable=$(find "$tree" -type f \( -perm -100 -o -perm -010 -o -perm -001 \) -print) || return 1
        if [ -n "$unexpected$executable" ]; then
            printf 'example-helper-vendor: non-regular or executable vendor entry: %s%s\n' \
                "$unexpected" "$executable" >&2
            return 1
        fi
    done
    diff -r "$1" "$module/vendor"
}
if ! diff_vendor "$scratch/vendor"; then
    echo 'example-helper-vendor: the committed vendor tree differs from the regenerated one' >&2
    exit 1
fi

# A future `go get`/manual go.mod edit changes the closure; `go mod tidy -diff`
# reports it without writing.
if ! vendor_go mod tidy -diff; then
    echo 'example-helper-vendor: go.mod/go.sum are not tidy' >&2
    exit 1
fi

cp -R "$module/vendor" "$scratch/mutant"
mutant_file="$(find "$scratch/mutant" -type f -name '*.go' -print -quit)"
[ -n "$mutant_file" ] || { echo 'example-helper-vendor: no vendored Go file to mutate' >&2; exit 1; }
printf '// drift\n' >>"$mutant_file"
if diff_vendor "$scratch/mutant" >"$scratch/mutant.log" 2>&1; then
    echo 'example-helper-vendor: drift gate accepted a mutant' >&2
    exit 1
fi

cp "$module/vendor/${mutant_file#"$scratch/mutant/"}" "$mutant_file"
chmod u+x "$mutant_file"
if diff_vendor "$scratch/mutant" >"$scratch/mode.log" 2>&1; then
    echo 'example-helper-vendor: drift gate accepted an executable file' >&2
    exit 1
fi
rm "$mutant_file"
ln -s "$module/vendor/${mutant_file#"$scratch/mutant/"}" "$mutant_file"
if diff_vendor "$scratch/mutant" >"$scratch/symlink.log" 2>&1; then
    echo 'example-helper-vendor: drift gate accepted a symlink' >&2
    exit 1
fi

echo 'example-helper-vendor: go.sum verified, vendor identical, tidy clean, mutant rejected'
