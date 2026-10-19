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
# A tool that cannot report a version still gets the "required, got ..." message.
version=$(actionlint -version | head -n 1) || version=unknown
[ "$version" = v1.7.7 ] || { echo "actionlint v1.7.7 required, got ${version:-nothing}" >&2; exit 1; }
version=$(shellcheck --version | sed -n 's/^version: //p') || version=unknown
[ "$version" = 0.11.0 ] || { echo "shellcheck 0.11.0 required, got ${version:-nothing} (script/dev/install-shellcheck.sh)" >&2; exit 1; }
timeout --version | grep -q 'GNU coreutils' || { echo 'GNU timeout required (macOS: coreutils gnubin on PATH)' >&2; exit 1; }
# The images run #!/bin/sh under dash; macOS sh is bash 3.2 and parses bashisms.
if command -v dash >/dev/null; then posix_sh='dash'
elif [ "${GITHUB_ACTIONS:-}" = true ]; then echo 'dash required on CI for #!/bin/sh syntax checks' >&2; exit 1
else posix_sh='sh'; echo 'note: dash not on PATH; #!/bin/sh syntax checked with sh -n' >&2
fi
if [ "$guard_only" = false ]; then
    for suite in agent-versions claude-installer codex-installer copilot-installer copilot-verify \
        opencode-installer opencode-pnpm-prepare-lock opencode-pnpm-install opencode-pnpm-upgrade \
        opencode-plugins-verify run-pnpm dsh-prepare-lock dsh-verify pi-install pi-prepare-lock pi-verify \
        pi-wrapper shipped-installer-contracts tool-access vendored-installers upgrade-scripts \
        install-shellcheck install-zellij install-pnpm host-watchdog transactional-publish \
        standard-certification build-entrypoint release-content release-operations release-http \
        ci-boot-bind ci-boot-runtime ci-boot-evidence ci-boot-prepare; do
        echo "=== $suite ==="
        bash "script/test/$suite.sh"
    done
    node script/test/pnpm-vendor.mjs
    node script/test/pnpm-lock-inputs.mjs
    bash script/test/standard-node-consumers.sh --self-test
    python3 script/test/standard-node-layout.py --self-test
    bash script/test/standard-node-policy.sh --self-test
    # The dsh filesystem module's authorization/publisher faults are not
    # exercised by the shell suites, so its unit table is its own gate.
    python3 script/test/dsh-project.py
    node script/test/pnpm-inputs.mjs
    python3 script/test/opencode-probe.py
    python3 script/test/opencode-audit-tests.py
    python3 script/test/opencode-egress.py
    python3 script/test/render-released-agent-probe.py --self-test
    bash script/test/pnpm-defaults.sh --hermetic
    bash script/test/standard-image.sh --self-test
    bash script/test/example-layers.sh --self-test
fi
# Syntax follows the script's interpreter; upstream snapshots are provenance.
while IFS= read -r -d '' script; do
    case "$(head -n 1 "$script")" in
        *bash*) bash -n "$script" ;;
        *) "$posix_sh" -n "$script" ;;
    esac
    # SC2016 in the byte-preserved Claude seed JS is inherited informational
    # literal interpolation, not a warning; don't rewrite reviewed payloads.
    shellcheck --severity=warning "$script"
done < <(find images script examples -type f \( -name '*.sh' -o -name hello-image \) ! -name '*.upstream.sh' ! -path '*/target/*' -print0)
bash script/test/example-layers.sh
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
# The migration ledger is a generated record: regenerate it in memory and fail on
# any drift. Fast and offline, but it reads the baseline commit object, so CI needs
# the full history (contracts.yml checks out with fetch-depth: 0).
node script/test/pnpm-migration-ledger.mjs --check
for source in images/recipe-contract/pnpm-lock.mjs script/test/pnpm-vendor.mjs script/test/pnpm-lock-inputs.mjs \
    script/test/pnpm-migration-ledger.mjs \
    images/recipe-contract/check-pnpm-environment.mjs \
    images/tools/dsh/check-lock-update.js images/tools/dsh/check-runtime.mjs \
    images/tools/dsh/derive-cordis-overrides.mjs \
    images/tools/dsh/upgrade-project.mjs script/test/fixtures/dsh/oracle.mjs \
    script/test/fixtures/dsh/bin.js script/test/fixtures/dsh/read-workspace.mjs \
    script/test/fixtures/dsh/strip-cordis-overrides.mjs \
    images/tools/opencode/check-claude-runtime.mjs images/tools/opencode/check-claude-payload.mjs images/tools/opencode/plugins/check-lock.mjs \
    images/tools/opencode/plugins/pin-pnpm-inputs.mjs \
    images/tools/opencode/plugins/index.js \
    images/tools/pi/extensions/agent-vm-codex.js \
    images/tools/pi/extensions/guest-credential-warning.js \
    script/test/fixtures/pi-codex/oracle.mjs script/test/pi-codex-read.mjs \
    script/test/fixtures/pi-codex/registration-probe.js; do
    node --check "$source"
done
(cd script/test/image-sources && cargo test --locked && cargo fmt --check && cargo clippy --locked --all-targets -- -D warnings)
actionlint .github/workflows/contracts.yml .github/workflows/build-local.yml .github/workflows/installer-network.yml .github/workflows/release-standard.yml .github/workflows/release-rehearsal.yml .github/workflows/release-transports.yml .github/workflows/verify-release-boot.yml
echo 'image contracts passed'
