#!/usr/bin/env bash
# Private graph framing only. Fixtures are never certified standard products.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
layout='' arch='' out=''
while [ "$#" -gt 0 ]; do
    [ "$#" -ge 2 ] || exit 2
    case "$1" in
        --layout) layout=$2 ;;
        --arch) arch=$2 ;;
        --out) out=$2 ;;
        *) echo "usage: $0 --layout DIR --arch ARCH --out NEW_DIR" >&2; exit 2 ;;
    esac
    shift 2
done
[ -n "$layout" ] && [ -n "$out" ] && [ -n "$arch" ] || exit 2
mkdir "$out"
# Caller-provided layouts are read-only here, including their wrapping metadata.
python3 "$ROOT/script/release/content.py" inventory --layout "$layout" --arch "$arch" --output "$out/graph.json"
# macOS otherwise synthesizes AppleDouble members forbidden by the OCI contract.
COPYFILE_DISABLE=1 tar -C "$layout" -cf "$out/archive.oci.tar" oci-layout index.json blobs
python3 "$ROOT/script/release/content.py" verify-archive --archive "$out/archive.oci.tar" --graph "$out/graph.json"
