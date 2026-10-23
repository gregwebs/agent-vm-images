#!/usr/bin/env bash
# Vendor drift gate for images/recipe-contract.
#
# The committed vendor tree is the build's whole dependency closure, and neither
# `go build` nor `go mod verify` authenticates its bytes (`go build` happily
# compiles an edited vendored file; `go mod verify` only reads the module cache).
# Regenerating vendor from the pinned go.sum in a fresh cache and diffing it
# against the committed tree is therefore the only gate that detects a local
# change, and the checksum/proxy policy is stated explicitly instead of inheriting
# the caller's environment.
set -euo pipefail

root="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
module="$root/images/recipe-contract"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/recipe-contract-vendor.XXXXXX")

cleanup() {
    status=$?
    # A module cache holds read-only directories. Make the scratch writable so the
    # removal cannot fail on permissions, and never hide a real failure: report the
    # scratch path that survived so the evidence is not lost.
    if [ -d "$scratch" ]; then
        chmod -R u+w "$scratch" 2>/dev/null || true
        if ! rm -rf "$scratch"; then
            echo "recipe-contract-vendor: cannot remove scratch $scratch" >&2
            [ "$status" -ne 0 ] || status=1
        fi
    fi
    exit "$status"
}
trap cleanup EXIT

expected_sum='go.yaml.in/yaml/v3 v3.0.5 h1:N6y/pJk8buWs9NY5ERU2HSMfm+IuD/OtfdAnq6kESPw=
go.yaml.in/yaml/v3 v3.0.5/go.mod h1:HVTZu1O7/Vkt2N+BFy8Zza+lnLsABggaTM2ZpNIGuKg='

cd "$module"

recorded_sum=$(cat go.sum) || { echo 'recipe-contract-vendor: cannot read go.sum' >&2; exit 1; }
[ "$recorded_sum" = "$expected_sum" ] || {
    echo 'recipe-contract-vendor: go.sum does not name the reviewed pin:' >&2
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
    echo 'recipe-contract-vendor: go.sum changed during download' >&2
    exit 1
}

vendor_go mod vendor -o "$scratch/vendor"

# diff_vendor is the gate. The mutation control below calls it with a tree that
# must be rejected, so a diff that cannot fail is a gate that proves nothing.
diff_vendor() {
    for tree in "$1" "$module/vendor"; do
        unexpected=$(find "$tree" ! -type f ! -type d -print) || return 1
        executable=$(find "$tree" -type f \( -perm -100 -o -perm -010 -o -perm -001 \) -print) || return 1
        if [ -n "$unexpected$executable" ]; then
            printf 'recipe-contract-vendor: non-regular or executable vendor entry: %s%s\n' "$unexpected" "$executable" >&2
            return 1
        fi
    done
    diff -r "$1" "$module/vendor"
}
if ! diff_vendor "$scratch/vendor"; then
    echo 'recipe-contract-vendor: the committed vendor tree differs from the regenerated one' >&2
    exit 1
fi

# A future `go get`/manual go.mod edit changes the closure; `go mod tidy -diff`
# reports it without writing.
if ! vendor_go mod tidy -diff; then
    echo 'recipe-contract-vendor: go.mod/go.sum are not tidy' >&2
    exit 1
fi

cp -R "$module/vendor" "$scratch/mutant"
mutant_file="$(find "$scratch/mutant" -type f -name '*.go' | head -n 1)"
[ -n "$mutant_file" ] || { echo 'recipe-contract-vendor: no vendored Go file to mutate' >&2; exit 1; }
printf '// drift\n' >>"$mutant_file"
if diff_vendor "$scratch/mutant" >"$scratch/mutant.log" 2>&1; then
    echo 'recipe-contract-vendor: drift gate accepted a mutant' >&2
    exit 1
fi

cp "$module/vendor/${mutant_file#"$scratch/mutant/"}" "$mutant_file"
chmod u+x "$mutant_file"
if diff_vendor "$scratch/mutant" >"$scratch/mode.log" 2>&1; then
    echo 'recipe-contract-vendor: drift gate accepted an executable file' >&2
    exit 1
fi
rm "$mutant_file"
ln -s "$module/vendor/${mutant_file#"$scratch/mutant/"}" "$mutant_file"
if diff_vendor "$scratch/mutant" >"$scratch/symlink.log" 2>&1; then
    echo 'recipe-contract-vendor: drift gate accepted a symlink' >&2
    exit 1
fi

echo 'recipe-contract-vendor: go.sum verified, vendor identical, tidy clean, mutant rejected'
