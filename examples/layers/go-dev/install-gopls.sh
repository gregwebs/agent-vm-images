#!/usr/bin/env bash
# Install the pinned gopls (the Go language server) from the reviewed module
# closure committed under gopls-closure/.
#
# Why it is this thin: the selected module graph, both checksum kinds for every
# selected module, the reviewed notice files and the record binding them live in
# gopls-closure/, and gopls_closure.py is the only program that verifies and
# installs them. This adapter exists so the Dockerfile keeps one plain RUN line
# and a standalone host build behaves the same way. There is no alternate
# install path: no `go install pkg@version`, no proxy-side version resolution and
# no "latest" here.
#
# What the helper proves before anything is published: the committed bytes match
# their recorded digests, the wrapper module is in its reviewed canonical form,
# go.sum carries both checksum kinds for every selected module, the root module's
# own go.mod bytes match their committed h1, and the selected graph obeys the
# reviewed current-pin adoption restriction. It then compiles with
# `-mod=readonly` in an environment it builds from scratch (GOTOOLCHAIN=local,
# proxy without `,direct`, explicit sumdb key, empty GOFLAGS, private caches,
# CGO_ENABLED=0 and this image's Linux target), and finally accepts the result
# only if the binary's own build info reports the pinned compiler, exactly the
# reviewed modules and exactly their sums. The third-party licence file is
# published from the same verified bytes.
#
# Environment:
#   GOPLS_VERSION  required; the module version the closure must select
#   GO_VERSION     required; the Go toolchain the closure must have been built by
#   GO_PREFIX      fixed /opt/go       (a different value cannot work: the image
#   GO_TOOLS_BIN   fixed /opt/go-tools/bin  ENV names these, so they are refused)
set -euo pipefail

: "${GOPLS_VERSION:?GOPLS_VERSION must be set to the pinned module version (e.g. v0.23.0)}"
: "${GO_VERSION:?GO_VERSION must be set to the pinned toolchain (e.g. 1.27.3)}"
GO_PREFIX="${GO_PREFIX:-/opt/go}"
GO_TOOLS_BIN="${GO_TOOLS_BIN:-/opt/go-tools/bin}"
[ "$GO_PREFIX" = /opt/go ] || {
    echo "install-gopls: GO_PREFIX must be the fixed /opt/go (got $GO_PREFIX)" >&2
    exit 1
}
[ "$GO_TOOLS_BIN" = /opt/go-tools/bin ] || {
    echo "install-gopls: GO_TOOLS_BIN must be the fixed /opt/go-tools/bin (got $GO_TOOLS_BIN)" >&2
    exit 1
}
HERE=$(cd "${BASH_SOURCE[0]%/*}" && pwd)
export PYTHONDONTWRITEBYTECODE=1

arch=$(dpkg --print-architecture)
case "$arch" in
    amd64 | arm64) ;;
    *)
        echo "install-gopls: unexpected architecture: $arch" >&2
        exit 1
        ;;
esac

echo "==> install-gopls: gopls ${GOPLS_VERSION} ($arch) from the committed closure"
/usr/bin/python3 "$HERE/gopls_closure.py" install "$HERE/gopls-closure" \
    --expect-gopls "$GOPLS_VERSION" --expect-go "$GO_VERSION" \
    --go-root "$GO_PREFIX" --arch "$arch" \
    --bin-dir "$GO_TOOLS_BIN" --share-dir /opt/go-tools/share/gopls

# The guest runs as an arbitrary uid (ADR-0001): the shared tree is readable and
# searchable by everyone and writable by no one else.
chmod -R a+rX /opt/go-tools
echo "==> install-gopls: done"
