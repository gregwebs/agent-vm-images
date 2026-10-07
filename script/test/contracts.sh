#!/usr/bin/env bash
# Image-only fast gates. Explicit list prevents accidentally running installers.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
cd "$ROOT"
guard_only=false
case "${1:-}" in
    '') [ "$#" -eq 0 ] || exit 2 ;;
    --guard-only) [ "$#" -eq 1 ] || exit 2; guard_only=true ;;
    *) echo "usage: $0 [--guard-only]" >&2; exit 2 ;;
esac
missing=false
for tool in bash git jq node npm python3 timeout shellcheck actionlint cargo; do
    if ! command -v "$tool" >/dev/null; then echo "missing prerequisite: $tool" >&2; missing=true; fi
done
[ "$missing" = false ] || exit 1
version=$(actionlint -version | head -n 1)
[ "$version" = v1.7.7 ] || { echo "actionlint v1.7.7 required, got $version" >&2; exit 1; }
timeout --version | grep -q 'GNU coreutils' || { echo 'GNU timeout required (macOS: coreutils gnubin on PATH)' >&2; exit 1; }
if [ "$guard_only" = false ]; then
    for suite in agent-versions claude-installer codex-installer copilot-installer copilot-verify \
        opencode-installer dsh-prepare-lock dsh-verify pi-install pi-prepare-lock pi-verify \
        pi-wrapper shipped-installer-contracts tool-access vendored-installers upgrade-scripts \
        install-zellij host-watchdog transactional-publish standard-certification build-entrypoint \
        release-content release-operations release-http; do
        echo "=== $suite ==="
        bash "script/test/$suite.sh"
    done
    bash script/test/standard-image.sh --self-test
fi
# Syntax follows the script's interpreter; upstream snapshots are provenance.
while IFS= read -r -d '' script; do
    case "$(head -n 1 "$script")" in
        *bash*) bash -n "$script" ;;
        *) sh -n "$script" ;;
    esac
    # SC2016 in the byte-preserved Claude seed JS is inherited informational
    # literal interpolation, not a warning; don't rewrite reviewed payloads.
    shellcheck --severity=warning "$script"
done < <(find images script examples -type f \( -name '*.sh' -o -name hello-image \) ! -name '*.upstream.sh' ! -path '*/target/*' -print0)
python3 - <<'PY'
import ast
from pathlib import Path
for root in ('images', 'script'):
    for p in Path(root).rglob('*.py'):
        if 'target' not in p.parts:
            ast.parse(p.read_text(), filename=str(p))
assert not list(Path('images/tools').glob('*/contract')), 'tool-local helpers must not reappear'
assert not list(Path('images/tools').glob('*/Dockerfile')), 'no per-tool products'
PY
node --check images/tools/dsh/check-lock-update.js
(cd script/test/image-sources && cargo test --locked && cargo fmt --check && cargo clippy --locked --all-targets -- -D warnings)
actionlint .github/workflows/contracts.yml .github/workflows/build-local.yml .github/workflows/installer-network.yml .github/workflows/release-standard.yml .github/workflows/release-rehearsal.yml .github/workflows/release-transports.yml
echo 'image contracts passed'
