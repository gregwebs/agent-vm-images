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
for tool in bash git gpg jq node npm python3 timeout shellcheck actionlint cargo; do
    if ! command -v "$tool" >/dev/null; then echo "missing prerequisite: $tool" >&2; missing=true; fi
done
[ "$missing" = false ] || exit 1
# A tool that cannot report a version still gets the "required, got ..." message.
version=$(actionlint -version | head -n 1) || version=unknown
[ "$version" = v1.7.7 ] || { echo "actionlint v1.7.7 required, got ${version:-nothing}" >&2; exit 1; }
version=$(shellcheck --version | sed -n 's/^version: //p') || version=unknown
[ "$version" = 0.11.0 ] || { echo "shellcheck 0.11.0 required, got ${version:-nothing} (script/dev/install-shellcheck.sh)" >&2; exit 1; }
timeout --version | grep -q 'GNU coreutils' || { echo 'GNU timeout required (macOS: coreutils gnubin on PATH)' >&2; exit 1; }
# Go owns the recipe-contract build helper. GOTOOLCHAIN=auto lets go.mod select the
# patch release; a stale local linter must fail rather than silently accept.
command -v go >/dev/null || { echo 'missing prerequisite: go (install Go; go.mod selects the toolchain via GOTOOLCHAIN=auto)' >&2; exit 1; }
# The module owns the linter pin, so this gate and the review notes cannot drift
# from one another; installing the same version is CONTRIBUTING's job.
lint_want=$(<"$ROOT/images/recipe-contract/golangci-lint.version")
lint_version=$(golangci-lint --version 2>/dev/null | sed -n 's/.*has version \([^ ]*\) .*/\1/p') || lint_version=unknown
[ "${lint_version:-}" = "$lint_want" ] || {
    echo "golangci-lint $lint_want required, got ${lint_version:-nothing} (bash script/dev/install-golangci-lint.sh)" >&2
    exit 1
}
contract_module="$ROOT/images/recipe-contract"
# The formatter must come from the toolchain the module selects, not from PATH:
# an older gofmt there would silently reformat to a different style. Every step is
# status-checked separately so a missing or failing formatter cannot pass on empty
# output.
if selected_goroot=$(cd "$contract_module" && GOTOOLCHAIN=auto go env GOROOT); then :; else
    status=$?; echo 'cannot select Go formatter toolchain' >&2; exit "$status"
fi
formatter="$selected_goroot/bin/gofmt"
[ -x "$formatter" ] || { echo "missing formatter: $formatter" >&2; exit 1; }
# Upstream vendor bytes are never reformatted: the vendor tree is a verbatim
# module copy whose only authenticator is the drift gate, so a formatter rewrite
# would look like a local (reviewed) change. The file list is passed explicitly
# because `-l .` walks vendor/.
format_files=()
while IFS= read -r -d '' format_file; do format_files+=("$format_file"); done \
    < <(cd "$contract_module" && find . -path ./vendor -prune -o -name '*.go' -print0)
if format_output=$(cd "$contract_module" && "$formatter" -l ${format_files+"${format_files[@]}"}); then :; else
    status=$?; echo "formatter failed: $formatter (status $status)" >&2; exit "$status"
fi
[ -z "$format_output" ] || { printf '%s\n' "$format_output" >&2; exit 1; }
(cd "$contract_module" && GOTOOLCHAIN=auto go test ./internal/buildcheck)
# Vendor mode: dependencies are committed, so the gates never reach a module
# proxy. A missing vendor tree fails here instead of downloading.
(cd "$contract_module" && GOFLAGS=-mod=vendor GOTOOLCHAIN=auto go vet ./...)
(cd "$contract_module" && golangci-lint run)
# The composed helper is shared by every suite below and by the Rust graph guard,
# so it is built once here: after the lint gates, because the formatter suite
# replaces `go` and must stop at vet.
contract_context=$(bash "$ROOT/script/build/recipe-contract.sh" --native) ||
    { echo 'cannot build the native recipe-contract helper' >&2; exit 1; }
export RECIPE_CONTRACT_CONTEXT="$contract_context"
# The committed repository base identity is derived from tracked bytes, so a
# stale manifest or extension default is a source defect every mode can see
# without Docker. It runs after the helper export so a formatter failure above
# still stops the gate first.
bash script/build/base-inputs.sh --check
# The images run #!/bin/sh under dash; macOS sh is bash 3.2 and parses bashisms.
if command -v dash >/dev/null; then posix_sh='dash'
elif [ "${GITHUB_ACTIONS:-}" = true ]; then echo 'dash required on CI for #!/bin/sh syntax checks' >&2; exit 1
else posix_sh='sh'; echo 'note: dash not on PATH; #!/bin/sh syntax checked with sh -n' >&2
fi
if [ "$guard_only" = false ]; then
    (cd "$contract_module" && GOFLAGS=-mod=vendor GOTOOLCHAIN=auto go test ./...)
    # Regeneration-plus-diff is the vendor drift gate: `go mod vendor` does not
    # authenticate edits to the committed tree and `go mod verify` never reads it.
    # It downloads through GOPROXY/GOSUMDB, so it is default-mode only.
    bash script/test/recipe-contract-vendor.sh
    python3 script/test/recipe-contract-oracle.py
    node script/test/recipe-contract-oracle.mjs
    python3 script/test/recipe-contract-adapters.py
    # Pinned third-party release artifacts (#96): the offline case table for the
    # installer and the fault-injection table for the maintenance writer, named
    # here because this file is the list of what runs.
    bash script/test/artifact-pins.sh
    python3 script/test/pin-artifact-maintenance.py
    # The committed Codex payload closure (#97): the offline pin/evidence schema,
    # the archive/preflight budgets and the owner writer's staging rules. Real
    # cosign and the two native architecture rows are the evidence phase's work,
    # never this aggregate.
    python3 script/test/codex-pins.py
    # The restricted-egress gate's stream assertions and its addon policy are the
    # parts a native run cannot check cheaply: the allow-list log shape, the `-`
    # placeholder row, and whether a learned redirect target keeps the HTTPS/port/
    # credential rules. Both replay the production functions/config offline.
    bash script/test/shipped-installer-network.sh --self-test
    python3 script/test/installer-egress-addon.py
    # The Rust example's pinned toolchain closure (#102): the offline pin/install
    # table and the maintenance writer's staging/publication faults. Both are
    # hermetic; the real updater and the native installer are never run here.
    python3 script/test/rust-toolchain-pins.py
    python3 script/test/rust-toolchain-maintenance.py
    # The Go example's gopls closure (#103): the real-go pin/install table and the
    # maintenance writer's faults. Native Go runs only inside the OS sandbox, and
    # --go-root is the already selected go1.27.3 root, so nothing downloads a
    # compiler; both suites refuse any other toolchain.
    python3 script/test/gopls-closure-pins.py --go-root "$selected_goroot"
    python3 script/test/gopls-closure-maintenance.py --go-root "$selected_goroot"
    for suite in agent-versions apt-sources claude-installer codex-installer copilot-installer copilot-verify \
        opencode-installer opencode-pnpm-prepare-lock opencode-pnpm-install opencode-pnpm-upgrade \
        opencode-plugins-verify run-pnpm dsh-prepare-lock dsh-verify pi-install pi-prepare-lock pi-verify \
        pi-wrapper shipped-installer-contracts tool-access vendored-installers upgrade-scripts \
        install-build-ca install-shellcheck install-zellij install-pnpm host-watchdog transactional-publish \
        standard-certification build-entrypoint contracts-formatter release-content release-operations release-http \
        ci-boot-bind ci-boot-runtime ci-boot-evidence ci-boot-prepare; do
        echo "=== $suite ==="
        bash "script/test/$suite.sh"
    done
    # The ci-boot-bind self-check is only worth its runtime while it still fails, and it is the
    # guard for the maintenance pins, not something these suites exercise (#92).
    python3 script/test/ci-boot-bind-mutants.py
    node script/test/pnpm-lock-inputs.mjs
    # issue #52 Phase 0 selected-shrinkwrap vectors: replay the authenticated
    # 0.87.0 identity/occurrence/SRI/edge/optional/peer comparison and its five
    # offline mutants. Data-only; no manager, network or committed 0.87 lock.
    python3 script/test/pi-shrinkwrap-vectors.py
    # issue #52 C3 additive defences: the converter's synthesis/delta vectors over
    # the Phase 0 evidence, and the installed-byte checker's real-layout mutant
    # table. Both are hermetic; the checker consumes a fixture graph through the
    # contract CLI seam and never a package manager.
    node script/test/pi-shrinkwrap.mjs
    node script/test/pi-installed-bytes.mjs
    bash script/test/standard-node-consumers.sh --self-test
    python3 script/test/standard-node-layout.py --self-test
    bash script/test/standard-node-policy.sh --self-test
    # The dsh filesystem module's authorization/publisher faults are not
    # exercised by the shell suites, so its unit table is its own gate. The
    # shared module carries the same boundary for all three consumers: its table
    # owns the consumer profiles, the bootstrap stage and the four-input
    # transaction's absent-destination and mode behaviour.
    python3 script/test/dsh-project.py
    python3 script/test/pnpm-project.py
    node script/test/pnpm-inputs.mjs
    python3 script/test/opencode-probe.py
    python3 script/test/opencode-audit-tests.py
    python3 script/test/opencode-egress.py
    python3 script/test/render-released-agent-probe.py --self-test
    bash script/test/pnpm-defaults.sh --hermetic
    bash script/test/standard-image.sh --self-test
    bash script/test/example-layers.sh --self-test
    # The native Rust harness's own controls (unavailable daemon, interrupted
    # run, parent argument on every build) need no daemon, so they run here; its
    # native phases stay the evidence phase's work.
    bash script/test/rust-example.sh --self-test
    # The Go example's native harness controls (unavailable/non-native daemon,
    # interrupted run, inspected parent on every build, LSP client faults): no
    # daemon, so they belong here; its native phases are the evidence run's work.
    bash script/test/go-example.sh --self-test
    # Example-layer CI (#33): the event/diff selector against a scratch repository
    # and the cold-build life cycle against a fake docker, including the three
    # installers' URL diagnostics. Neither touches a daemon or the network.
    python3 script/ci/layer-changes.py --self-test
    bash script/test/layer-build.sh --self-test
    # Updater transaction and signal table (#41): the host-only writer's statuses.
    python3 script/test/chrome-mcp-upgrade.py
    # Parent-pin resolver/publisher table (#95): authenticated resolution,
    # mutation refusals and publication containment, all hermetic.
    python3 script/test/parent-pins.py
    # Repository base identity (#95): the committed manifest, the generator, the
    # in-build verifier and the copied extension default, all hermetic.
    python3 script/test/base-inputs.py
fi
# --- no embedded Python in shell sources -------------------------------------
# Python belongs in its own file, invoked as `python3 FILE`; see CODING_STANDARDS
# "Avoid embedding code". That includes containers, BuildKit gates and rendered
# guests: those need an explicit mount or a renderer source entry, never inline
# source. The walk is over the filesystem, so a new untracked .sh cannot escape
# it. It is lexical: aliases, computed commands and extensionless bodies still
# need the manual inventory review, so do not widen it into directory exemptions.
embedded_manifest=$(mktemp "${TMPDIR:-/tmp}/embedded-python.XXXXXX") ||
    { echo 'FAIL: cannot create the shell-source manifest for the embedded-Python gate' >&2; exit 1; }
trap 'rm -f "$embedded_manifest"' EXIT
# `target` is generated build scratch, not a maintained-source allowance. The
# Codex vendor snapshot used to carry an inline metadata parser; the committed pin
# replaced it, so there is no longer an exemption for it here.
if ! find images script examples -type f -name '*.sh' \
    ! -path '*/target/*' \
    -print0 >"$embedded_manifest"; then
    echo 'FAIL: cannot enumerate shell sources for the embedded-Python gate' >&2
    exit 1
fi
# Portable ERE alternatives, assembled as data and never eval'd: an interpreter
# reaching its source mode (-c or stdin) through its own option run, a
# `--entrypoint` interpreter argument, and a Python heredoc label or .py
# destination. Heredocs are spelled `[<][<]` so this definition cannot match
# itself.
#
# The interpreter name is a whole word, so quoting, command substitution and an
# option value (`--entrypoint=…`) are in scope. The option run is what keeps the
# gate honest: between the name and the source-mode token only option-shaped
# tokens, plus the options that take a separate value (-X dev, -W ignore), may
# appear. Any other token is an operand and ends the run, so a helper's own -c or
# stdin dash after `name FILE` is not inline source, and neither is a stray -c
# further along the same line. -c may carry attached program text (-c'pass',
# -cpass) and stdin may be fed by a redirect (-<program.py). Kept on purpose as
# residual gaps of a lexical gate, not a parser: a quoted option token, a
# combined short option (-Bc), a versioned name (python3.11), a bare `python`,
# a long option whose separate value is not -X/-W (--check-hash-based-pycs
# always), and a body assembled across lines all still pass.
embedded_option_run='([[:space:]]+(-[XW][[:space:]]+[^[:space:];|&]+|-[^[:space:];|&]*))*'
embedded_source_mode='[[:space:]]+(-c|-($|[[:space:]]|\\|<))'
embedded_pattern='(^|[^[:alnum:]_.-])(/usr/bin/|/usr/local/bin/)?python3'
embedded_pattern="$embedded_pattern$embedded_option_run$embedded_source_mode"
# docker runs the interpreter directly with the image reference as its operand,
# so the source-mode token sits after one argument here; the `--entrypoint`
# prefix makes that unambiguous, and a helper-script operand cannot hide behind
# it because the token has to be the image reference.
embedded_pattern="$embedded_pattern"'|(^|[[:space:]])--entrypoint(=|[[:space:]]+)(/usr/bin/|/usr/local/bin/)?python3([[:space:]]+[^[:space:];|&]+)?'
embedded_pattern="$embedded_pattern$embedded_source_mode"
embedded_pattern="$embedded_pattern"'|[<][<]-?[[:space:]]*['"'"'"]?[Pp][Yy][[:alnum:]_]*['"'"'"]?([[:space:]]|$)'
embedded_pattern="$embedded_pattern"'|[>][^<]*[.]py[^<]*[<][<]|[<][<][^>]*[>][^;]*[.]py'
embedded_found=false
while IFS= read -r -d '' script; do
    embedded_hits=
    if embedded_hits=$(grep -nE -e "$embedded_pattern" -- "$script"); then
        :
    else
        status=$?
        [ "$status" -eq 1 ] ||
            { echo "FAIL: embedded-Python search failed for $script (grep status $status)" >&2; exit 1; }
        embedded_hits=
    fi
    [ -n "$embedded_hits" ] || continue
    # A full-line shell comment cannot execute a body; drop those lines only.
    embedded_code=
    if embedded_code=$(printf '%s\n' "$embedded_hits" | grep -vE -e '^[0-9]+:[[:space:]]*#'); then
        :
    else
        status=$?
        [ "$status" -eq 1 ] ||
            { echo "FAIL: embedded-Python comment filter failed for $script (grep status $status)" >&2; exit 1; }
        embedded_code=
    fi
    [ -n "$embedded_code" ] || continue
    printf '%s\n' "$embedded_code" | sed -e "s|^|$script:|"
    embedded_found=true
done <"$embedded_manifest"
if [ "$embedded_found" = true ]; then
    echo 'FAIL: embedded Python candidate in shell source; move Python to a sibling .py and invoke python3 FILE (preserve container/guest delivery). Codex vendor changes require the reviewed vendor update procedure.' >&2
    exit 1
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
# The Chrome container checks assert subprocess status separately from the text
# they match; these stand-in producers prove that, without Docker.
"$posix_sh" script/test/fixtures/chrome-example/checks.sh self-test
python3 "$ROOT/script/test/contracts-python.py"
# The migration ledger is a generated record: regenerate it in memory and fail on
# any drift. Fast and offline, but it reads the baseline commit object, so CI needs
# the full history (contracts.yml checks out with fetch-depth: 0).
node script/test/pnpm-migration-ledger.mjs --check
for source in script/test/pnpm-lock-inputs.mjs \
    script/test/pnpm-migration-ledger.mjs \
    images/tools/dsh/check-runtime.mjs \
    script/test/fixtures/dsh/oracle.mjs \
    script/test/fixtures/dsh/bin.js \
    script/test/fixtures/dsh/strip-cordis-overrides.mjs \
    images/tools/opencode/check-claude-runtime.mjs images/tools/opencode/check-claude-payload.mjs images/tools/opencode/plugins/check-lock.mjs \
    images/tools/opencode/plugins/pin-pnpm-inputs.mjs \
    images/tools/opencode/plugins/index.js \
    images/tools/pi/extensions/agent-vm-codex.js \
    images/tools/pi/extensions/guest-credential-warning.js \
    script/test/fixtures/pi-codex/oracle.mjs script/test/pi-codex-read.mjs \
    script/test/fixtures/pi-codex/registration-probe.js \
    script/test/fixtures/chrome-example/mcp-probe.mjs; do
    node --check "$source"
done
(cd script/test/image-sources && cargo test --locked && cargo fmt --check && cargo clippy --locked --all-targets -- -D warnings)
actionlint .github/workflows/example-layers.yml .github/workflows/contracts.yml .github/workflows/build-local.yml .github/workflows/installer-network.yml .github/workflows/release-standard.yml .github/workflows/release-rehearsal.yml .github/workflows/release-transports.yml .github/workflows/verify-release-boot.yml
echo 'image contracts passed'
