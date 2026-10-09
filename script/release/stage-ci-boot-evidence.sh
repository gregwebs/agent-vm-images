#!/usr/bin/env bash
# Retain all available small diagnostics; any failed copy must still fail the step.
set -euo pipefail
[ "$#" -eq 4 ] || { echo "usage: ${0##*/} ASSETS RUNTIME_LOGS MSB_DIR EVIDENCE_DIR" >&2; exit 2; }
assets=$1 runtime_logs=$2 msb_dir=$3 evidence=$4
shopt -s nullglob
failed=0
if ! mkdir -p "$evidence/verification" "$evidence/release" "$evidence/runtime"; then failed=1; fi
copy() {
    if ! cp -p "$1" "$2"; then
        echo "evidence copy failed: $1 -> $2" >&2
        failed=1
    fi
}
# Flat small files only. Multiple success records are rejected by validation.
for file in "$assets"/verify-amd64-*/*.log "$assets"/verify-amd64-*/*.json; do
    copy "$file" "$evidence/verification/"
done
# Literal required metadata names are attempted even if absent. Bundles are globbed once.
for file in "$assets"/release.json "$assets"/SHA256SUMS "$assets"/platform-amd64.json "$assets"/*.sigstore.json; do
    copy "$file" "$evidence/release/"
done
for file in "$runtime_logs"/*.log ${msb_dir:+"$msb_dir"/*.log}; do
    copy "$file" "$evidence/runtime/"
done
if ! ls -lR "$evidence"; then failed=1; fi
exit "$failed"
