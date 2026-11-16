#!/usr/bin/env bash
# Vendor drift gate for the repository Go module.
#
# The committed vendor tree is the build's whole dependency closure, and neither
# `go build` nor `go mod verify` authenticates its bytes (`go build` happily
# compiles an edited vendored file; `go mod verify` only reads the module cache).
# Regenerating vendor from the pinned go.sum in a fresh cache and diffing it
# against the committed tree is therefore the only gate that detects a local
# change, and the checksum/proxy policy is stated explicitly instead of inheriting
# the caller's environment.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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

expected_sum='github.com/alexflint/go-arg v1.6.1 h1:uZogJ6VDBjcuosydKgvYYRhh9sRCusjOvoOLZopBlnA=
github.com/alexflint/go-arg v1.6.1/go.mod h1:nQ0LFYftLJ6njcaee0sU+G0iS2+2XJQfA8I062D0LGc=
github.com/alexflint/go-scalar v1.2.0 h1:WR7JPKkeNpnYIOfHRa7ivM21aWAdHD0gEWHCx+WQBRw=
github.com/alexflint/go-scalar v1.2.0/go.mod h1:LoFvNMqS1CPrMVltza4LvnGKhaSpc3oyLEBUZVhhS2o=
github.com/bits-and-blooms/bitset v1.24.4 h1:95H15Og1clikBrKr/DuzMXkQzECs1M6hhoGXLwLQOZE=
github.com/bits-and-blooms/bitset v1.24.4/go.mod h1:7hO7Gc7Pp1vODcmWvKMRA9BNmbv6a/7QIWpPxHddWR8=
github.com/davecgh/go-spew v1.1.1 h1:vj9j/u1bqnvCEfJOwUhtlOARqs3+rkHYY13jYWTU97c=
github.com/davecgh/go-spew v1.1.1/go.mod h1:J7Y8YcW2NihsgmVo/mv3lAwl/skON4iLHjSsI+c5H38=
github.com/failsafe-go/failsafe-go v0.9.8 h1:NzahTEc+vg6FqCJV0Sy9mgwcaCemB81c8iSHWTF/urU=
github.com/failsafe-go/failsafe-go v0.9.8/go.mod h1:wMKUFRbxxjWwvwhJttSiim9BaQssrlZV4lk0IwPkwUc=
github.com/influxdata/tdigest v0.0.1 h1:XpFptwYmnEKUqmkcDjrzffswZ3nvNeevbUSLPP/ZzIY=
github.com/influxdata/tdigest v0.0.1/go.mod h1:Z0kXnxzbTC2qrx4NaIzYkE1k66+6oEDQTvL95hQFh5Y=
github.com/pmezard/go-difflib v1.0.0 h1:4DBwDE0NGyQoBHbLQYPwSUPoCMWR5BEzIk/f1lZbAQM=
github.com/pmezard/go-difflib v1.0.0/go.mod h1:iKH77koFhYxTK1pcRnkKkqfTogsbg7gZNVY4sRDYZ/4=
github.com/stretchr/testify v1.2.2/go.mod h1:a8OnRcib4nhh0OaRAV+Yts87kKdq0PP7pXfy6kDkUVs=
github.com/stretchr/testify v1.11.1 h1:7s2iGBzp5EwR7/aIZr8ao5+dra3wiQyKjjFuvgVKu7U=
github.com/stretchr/testify v1.11.1/go.mod h1:wZwfW3scLgRK+23gO65QZefKpKQRnfz6sD981Nm4B6U=
github.com/ulikunitz/xz v0.5.17 h1:flR0y/x1hgM8EGV1AW3Xll6T413G0glV8UfBwR617V4=
github.com/ulikunitz/xz v0.5.17/go.mod h1:H9Rt/W6/Qj27PGauhQc6nfCDy7vHpzsOThBSaYDoEhw=
go.yaml.in/yaml/v3 v3.0.5 h1:N6y/pJk8buWs9NY5ERU2HSMfm+IuD/OtfdAnq6kESPw=
go.yaml.in/yaml/v3 v3.0.5/go.mod h1:HVTZu1O7/Vkt2N+BFy8Zza+lnLsABggaTM2ZpNIGuKg=
golang.org/x/net v0.28.0 h1:a9JDOJc5GMUJ0+UDqmLT86WiEy7iWyIhz8gz8E4e5hE=
golang.org/x/net v0.28.0/go.mod h1:yqtgsTWOOnlGLG9GFRrK3++bGOUEkNBoHZc8MEDWPNg=
golang.org/x/sys v0.24.0 h1:Twjiwq9dn6R1fQcyiK+wQyHWfaz/BJB+YIpzU/Cv3Xg=
golang.org/x/sys v0.24.0/go.mod h1:/VUhepiaJMQUp4+oa/7Zr1D23ma6VTLIYjOOTFZPUcA=
golang.org/x/text v0.17.0 h1:XtiM5bkSOt+ewxlOE/aE/AKEHibwj/6gvWMl9Rsh0Qc=
golang.org/x/text v0.17.0/go.mod h1:BuEKDfySbSR4drPmRPG/7iBdf8hvFMuRexcpahXilzY=
google.golang.org/genproto/googleapis/rpc v0.0.0-20240814211410-ddb44dafa142 h1:e7S5W7MGGLaSu8j3YjdezkZ+m1/Nm0uRVRMEMGk26Xs=
google.golang.org/genproto/googleapis/rpc v0.0.0-20240814211410-ddb44dafa142/go.mod h1:UqMtugtsSgubUsoxbuAoiCXvqvErP7Gf0so0mK9tHxU=
google.golang.org/grpc v1.67.1 h1:zWnc1Vrcno+lHZCOofnIMvycFcc0QRGIzm9dhnDX68E=
google.golang.org/grpc v1.67.1/go.mod h1:1gLDyUQU7CTLJI90u3nXZ9ekeghjeM7pTDZlqFNg2AA=
google.golang.org/protobuf v1.36.4 h1:6A3ZDJHn/eNqc1i+IdefRzy/9PokBTPvcqMySR7NNIM=
google.golang.org/protobuf v1.36.4/go.mod h1:9fA7Ob0pmnwhb644+1+CVWFRbNajQ6iRojtC/QF5bRE=
gopkg.in/yaml.v3 v3.0.1 h1:fxVm/GzAzEWqLHuvctI91KS9hhNmmWOoWu0XTYJS7CA=
gopkg.in/yaml.v3 v3.0.1/go.mod h1:K4uyk7z7BCEPqu6E+C64Yfv1cQ7kz7rIZviUmN+EgEM='

# The module root is the repository root, which also holds ignored build scratch
# that may contain stand-alone Go files. A module-wide command (`go mod tidy`,
# `go mod vendor`) walks all of it, so this gate runs over the tracked tree — the
# files a checkout owns, with the working tree's content — instead of the
# directory.
module="$scratch/tree"
mkdir -p "$module"
(cd "$root" && git ls-files -z | tar --null -T - -cf -) | tar -xf - -C "$module" ||
    { echo 'recipe-contract-vendor: cannot extract the tracked module tree' >&2; exit 1; }

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
    for tree in "$1" "$root/vendor"; do
        unexpected=$(find "$tree" ! -type f ! -type d -print) || return 1
        executable=$(find "$tree" -type f \( -perm -100 -o -perm -010 -o -perm -001 \) -print) || return 1
        if [ -n "$unexpected$executable" ]; then
            printf 'recipe-contract-vendor: non-regular or executable vendor entry: %s%s\n' "$unexpected" "$executable" >&2
            return 1
        fi
    done
    diff -r "$1" "$root/vendor"
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

cp -R "$root/vendor" "$scratch/mutant"
# `-print -quit` rather than `| head -1`: under `pipefail` the pipeline reports
# find's SIGPIPE when head exits first, which made this line fail on a bigger
# vendor tree.
mutant_file="$(find "$scratch/mutant" -type f -name '*.go' -print -quit)"
[ -n "$mutant_file" ] || { echo 'recipe-contract-vendor: no vendored Go file to mutate' >&2; exit 1; }
printf '// drift\n' >>"$mutant_file"
if diff_vendor "$scratch/mutant" >"$scratch/mutant.log" 2>&1; then
    echo 'recipe-contract-vendor: drift gate accepted a mutant' >&2
    exit 1
fi

cp "$root/vendor/${mutant_file#"$scratch/mutant/"}" "$mutant_file"
chmod u+x "$mutant_file"
if diff_vendor "$scratch/mutant" >"$scratch/mode.log" 2>&1; then
    echo 'recipe-contract-vendor: drift gate accepted an executable file' >&2
    exit 1
fi
rm "$mutant_file"
ln -s "$root/vendor/${mutant_file#"$scratch/mutant/"}" "$mutant_file"
if diff_vendor "$scratch/mutant" >"$scratch/symlink.log" 2>&1; then
    echo 'recipe-contract-vendor: drift gate accepted a symlink' >&2
    exit 1
fi

echo 'recipe-contract-vendor: go.sum verified, vendor identical, tidy clean, mutant rejected'
