#!/usr/bin/env bash
# Static image contracts only; launcher agreement and native builds have other owners.
set -euo pipefail
CHECKER="$(cd "${BASH_SOURCE[0]%/*}" && pwd)/$(basename "${BASH_SOURCE[0]}")"
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
# Image-owned interface: agent-vm's CHROME_MCP_CAPABILITY_PATH and
# CHROME_MCP_WRAPPER_PATH (crates/agent-vm/src/defaults.rs) must name these
# paths; agent-vm checks that side (docs/image-source-ownership.md).
CHROME_MARKER=/etc/agent-vm-capabilities/chrome-devtools-mcp
CHROME_WRAPPER=/usr/local/bin/agent-vm-chrome-mcp
LAYERS=examples/layers
EXAMPLES='chrome-devtools go-dev rust-dev wirenboard-cpp'
# Known space-free paths, explicitly moved from agent-vm's ci-contracts.sh:
# deletion/rename must fail rather than silently leave lint coverage.
GUARDED='chrome-devtools/agent-vm-chrome-mcp chrome-devtools/chrome-devtools-mcp.sh go-dev/install-go.sh go-dev/install-golangci-lint.sh go-dev/install-gopls.sh go-dev/verify-toolchain.sh rust-dev/install-rust.sh rust-dev/install-verus.sh rust-dev/rust_toolchain.py rust-dev/verify-toolchain.sh'
fail() { echo "$*" >&2; exit 1; }
usage() { echo "usage: example-layers.sh [--self-test | --root DIR]" >&2; exit 2; }
checksum_token() {
    awk '!/^[[:space:]]*#/ && /sha256sum/ { found=1 } END { exit !found }' "$1"
}
# Only instruction boundaries for parent checks: no shell/JSON parsing, variable
# expansion or general Dockerfile validation. Heredoc words must be simple;
# quoted/escaped shell context before an operator is unsupported, not guessed.
# Fast drift guard limits: no full quoted heredoc delimiter grammar, distinction
# of heredoc-looking LABEL/ENV values, escaped-backslash boundaries, or Docker
# directive cutoff after an ordinary comment (only after an instruction).
logical_instructions() {
    awk -v example="$2" -v quote="'" '
        function reject(message) {
            print "parent pin: " message ": " example > "/dev/stderr"
            failed=1
            exit 1
        }
        function heredocs(line, rest, prefix, tabs, word, delimiter, end) {
            rest=line
            while (match(rest, /<</)) {
                prefix=substr(rest, 1, RSTART-1)
                if (index(prefix, quote) || index(prefix, "\"") || index(prefix, "\\") || index(prefix, "#"))
                    reject("unsupported heredoc context")
                rest=substr(rest, RSTART+2)
                tabs=(substr(rest, 1, 1) == "-")
                if (tabs) rest=substr(rest, 2)
                sub(/^[ \t]+/, "", rest)
                delimiter=substr(rest, 1, 1)
                if (delimiter == quote || delimiter == "\"") {
                    rest=substr(rest, 2)
                    end=index(rest, delimiter)
                    if (!end) reject("unsupported heredoc word")
                    word=substr(rest, 1, end-1)
                    rest=substr(rest, end+1)
                } else {
                    if (!match(rest, /^[A-Za-z0-9_.-]+/)) reject("unsupported heredoc word")
                    word=substr(rest, 1, RLENGTH)
                    rest=substr(rest, RLENGTH+1)
                }
                if (word !~ /^[A-Za-z0-9_.-]+$/ || rest !~ /^($|[ \t;&|>)])/)
                    reject("unsupported heredoc word")
                terminator[++total]=word
                strip_tabs[total]=tabs
            }
        }
        BEGIN { escape="\\"; leading=1; body=1 }
        {
            sub(/\r$/, "")
            if (body <= total) {
                data=$0
                if (strip_tabs[body]) sub(/^\t+/, "", data)
                if (data == terminator[body]) body++
                next
            }
            if (leading && $0 ~ /^[ \t]*#[ \t]*[Ee][Ss][Cc][Aa][Pp][Ee][ \t]*=/) {
                directive=$0
                sub(/^[^=]*=[ \t]*/, "", directive)
                sub(/[ \t]+$/, "", directive)
                if (directive != "\\" && directive != "`") reject("unsupported escape directive")
                escape=directive
                next
            }
            if ($0 ~ /^[ \t]*(#|$)/) next
            leading=0
            continued=(substr($0, length($0), 1) == escape)
            line=line (continued ? substr($0, 1, length($0)-1) : $0)
            if (continued) next
            print line
            heredocs(line)
            line=""
        }
        END {
            if (failed) exit 1
            if (body <= total) reject("unterminated heredoc")
            if (continued) reject("unsupported unterminated continuation")
        }
    ' "$1"
}
check() {
    [ -d "$ROOT" ] && [ -r "$ROOT" ] && [ -x "$ROOT" ] || fail "root missing: $ROOT"
    cd "$ROOT" || fail "root missing: $ROOT"
    local example dockerfile instructions first refs token wrapper arg file rel
    for example in $EXAMPLES; do
        dockerfile="$LAYERS/$example/Dockerfile"
        [ -f "$dockerfile" ] || fail "parent pin: ARG shape: $example Dockerfile missing"
        instructions=$(logical_instructions "$dockerfile" "$example") || exit 1
        first=$(printf '%s\n' "$instructions" | awk 'NR == 1 { print }')
        # C2a first-ARG shape begin
        [[ "$first" =~ ^ARG\ BASE_IMAGE=ghcr\.io/gregwebs/agent-vm-standard@sha256:[0-9a-f]{64}$ ]] || fail "parent pin: ARG shape: $example"
        # C2a first-ARG shape end
        [ "$(printf '%s\n' "$instructions" | awk '
            toupper($1) == "ARG" {
                for (i=2; i<=NF; i++) if ($i ~ /^BASE_IMAGE(=|$)/) count++
            }
            END { print count+0 }
        ')" -eq 1 ] || fail "parent pin: ARG shape: $example redefinition"
        printf '%s\n' "$instructions" | awk 'toupper($0) ~ /^[[:space:]]*FROM([[:space:]]|$)/ { found=1; if ($0 != "FROM ${BASE_IMAGE}") bad=1 } END { exit (!found || bad) }' || fail "parent pin: FROM shape: $example"
    done
    refs=$(grep -rhoE 'ghcr\.io/gregwebs/agent-vm-standard[^[:space:]`"]*' "$LAYERS" | sort -u)
    [[ "$refs" =~ ^ghcr\.io/gregwebs/agent-vm-standard@sha256:[0-9a-f]{64}$ ]] || fail 'parent pin: references disagree'
    dockerfile="$LAYERS/chrome-devtools/Dockerfile"
    for token in "$CHROME_MARKER" /usr/bin/google-chrome-stable /opt/google/chrome/chrome 'visudo -cf' 'sudo -u chrome -H -- test -w' 'getent group 9999' 'getent passwd 9999'; do
        grep -qF "$token" "$dockerfile" || fail "Chrome example lacks: $token"
    done
    grep -qxF "COPY --chmod=0755 agent-vm-chrome-mcp $CHROME_WRAPPER" "$dockerfile" || fail "Chrome wrapper must be installed at $CHROME_WRAPPER"
    [ "$(tail -n 1 "$dockerfile")" = " && : > $CHROME_MARKER" ] || fail 'Chrome marker must be written last'
    wrapper="$LAYERS/chrome-devtools/agent-vm-chrome-mcp"
    [ -f "$wrapper" ] || fail 'guarded script missing: chrome-devtools/agent-vm-chrome-mcp'
    for token in 'failed to prepare chrome NSS DB' 'sudo -u chrome -H -n' 'cannot cd to HOME'; do
        grep -qF "$token" "$wrapper" || fail "Chrome wrapper lacks: $token"
    done
    if grep -qF '|| true' "$wrapper"; then fail 'Chrome wrapper must not swallow failures'; fi
    # The preinstalled MCP selection is installed by exactly one reviewed helper
    # invocation over the committed inputs; nothing at first use resolves or
    # fetches (issue #41).
    mcpdir="$LAYERS/chrome-devtools/chrome-devtools-mcp"
    helper="$LAYERS/chrome-devtools/chrome-devtools-mcp.sh"
    [ -f "$helper" ] || fail 'guarded script missing: chrome-devtools/chrome-devtools-mcp.sh'
    instructions=$(logical_instructions "$dockerfile" chrome-devtools) || exit 1
    grep -qxF 'COPY chrome-devtools-mcp/ /opt/agent-vm/chrome-devtools-mcp/' "$dockerfile" ||
        fail 'Chrome MCP inputs must be copied to /opt/agent-vm/chrome-devtools-mcp'
    install_line='RUN --mount=type=bind,source=chrome-devtools-mcp.sh,target=/tmp/chrome-devtools-mcp.sh,ro     sh /tmp/chrome-devtools-mcp.sh install'
    [ "$(printf '%s\n' "$instructions" | grep -cF 'chrome-devtools-mcp.sh')" -eq 1 ] ||
        fail 'Chrome MCP install must be exactly the reviewed helper invocation'
    printf '%s\n' "$instructions" | grep -qxF "$install_line" ||
        fail 'Chrome MCP install must be exactly the reviewed helper invocation'
    fetch_ere='(^|[^[:alnum:]_.-])(npx|pnpx|pnx|bunx|yarn)([[:space:]]|$)|(^|[^[:alnum:]_.-])npm[[:space:]]+(exec|install|i|x)([[:space:]]|$)|(^|[^[:alnum:]_.-])pnpm[[:space:]]+(dlx|add|exec|install|i)([[:space:]]|$)|pre-warm|lazy-fetch'
    offending=$(printf '%s\n' "$instructions" | grep -E "$fetch_ere" | head -n 1 || true)
    [ -z "$offending" ] || fail "Chrome example must not fetch at first use or skip install: $offending"
    selected=$(sh "$helper" check "$mcpdir" 2>&1) || fail "Chrome MCP inputs: $selected"
    # Static provenance binding: the committed bundle must name the locked
    # tarball. Phase A of chrome-example.sh verifies it cryptographically.
    bundle="$LAYERS/chrome-devtools/chrome-devtools-mcp.sigstore.json"
    lock="$mcpdir/pnpm-lock.yaml"
    hex=$(sed -n 's/.*integrity: sha512-\([A-Za-z0-9+/=]*\).*/\1/p' "$lock" | base64 -d | od -An -v -tx1 | tr -d ' \n')
    jq -e --arg n "pkg:npm/chrome-devtools-mcp@$selected" --arg h "$hex" '
        (.mediaType == "application/vnd.dev.sigstore.bundle.v0.3+json")
        and (.dsseEnvelope.payloadType == "application/vnd.in-toto+json")
        and ((.dsseEnvelope.payload | @base64d | fromjson) as $statement
             | $statement.predicateType == "https://slsa.dev/provenance/v1"
             and $statement.subject == [{name: $n, digest: {sha512: $h}}])
    ' "$bundle" >/dev/null 2>&1 || fail 'Chrome MCP provenance does not match the lock'
    for token in 'MCP_PREFIX=/opt/agent-vm/chrome-devtools-mcp' 'refusing to run a package manager' \
        "command must not start with '-'"; do
        grep -qF "$token" "$wrapper" || fail "Chrome wrapper lacks: $token"
    done
    installed_token='chrome-devtools-mcp@$installed'
    grep -qF "\"$installed_token\"" "$wrapper" || fail "Chrome wrapper lacks: \"$installed_token\""
    dockerfile="$LAYERS/rust-dev/Dockerfile"
    for token in '^ARG RUST_TOOLCHAIN=[0-9]+\.[0-9]+(\.[0-9]+)?$' '^ARG VERUS_RELEASE=[^[:space:]]+$' '^ARG VERUS_SHA256=[0-9a-f]{64}$'; do
        grep -qE "$token" "$dockerfile" || fail "rust-dev pins: $token"
    done
    file="$LAYERS/rust-dev/install-verus.sh"
    [ -f "$file" ] || fail 'guarded script missing: rust-dev/install-verus.sh'
    checksum_token "$file" || fail 'rust-dev: install-verus.sh lacks non-comment sha256sum token'
    # --- rust-dev: reviewed toolchain closure (#102) -------------------------
    # The committed data directory is the reviewed input set; the helper is the
    # only program that verifies and installs it. Presence is checked before the
    # helper is invoked so a missing file reports itself instead of a shell error.
    rustdir=$LAYERS/rust-dev
    helper=$rustdir/rust_toolchain.py
    datadir=$rustdir/rust-toolchain
    [ -f "$helper" ] || fail 'guarded script missing: rust-dev/rust_toolchain.py'
    [ -d "$datadir" ] || fail 'rust-dev: missing Rust input directory'
    rust_arg=$(sed -nE 's/^ARG RUST_TOOLCHAIN=(.*)$/\1/p' "$rustdir/Dockerfile")
    [ -n "$rust_arg" ] || fail 'rust-dev pins: ARG RUST_TOOLCHAIN'
    selected=$(PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/$helper" check "$ROOT/$datadir" \
        --expect-channel "$rust_arg" 2>&1) || fail "rust-dev pins: $selected"
    [ "$selected" = "$rust_arg" ] || fail "rust-dev pins: channel disagreement"
    instructions=$(logical_instructions "$rustdir/Dockerfile" rust-dev) || exit 1
    # Normalized for comparison: `logical_instructions` keeps a continuation's
# spacing, so runs of blanks are collapsed before the exact shape comparison.
normalize_instruction() { printf '%s' "$1" | tr -s ' \t' ' ' | sed -e 's/^ //' -e 's/ $//'; }
reviewed_installer='RUN --mount=type=bind,source=install-rust.sh,target=/tmp/install-rust.sh,ro --mount=type=bind,source=rust_toolchain.py,target=/tmp/rust_toolchain.py,ro --mount=type=bind,source=rust-toolchain,target=/tmp/rust-toolchain,ro bash /tmp/install-rust.sh'
reviewed_verifier='RUN --network=none --mount=type=bind,source=verify-toolchain.sh,target=/run/rust-verify/verify-toolchain.sh,ro --mount=type=bind,source=rust_toolchain.py,target=/run/rust-verify/rust_toolchain.py,ro --mount=type=bind,source=rust-toolchain,target=/run/rust-verify/rust-toolchain,ro RUST_TOOLCHAIN=$RUST_TOOLCHAIN VERUS_DIR=/opt/verus PYTHONDONTWRITEBYTECODE=1 /bin/bash /run/rust-verify/verify-toolchain.sh'
installer_runs=$(printf '%s\n' "$instructions" | grep -cF 'install-rust.sh' || true)
    [ "$installer_runs" -eq 1 ] || fail 'rust-dev: installer RUN must appear exactly once'
    installer_line=$(printf '%s\n' "$instructions" | grep -F 'install-rust.sh')
    for token in 'source=install-rust.sh' 'source=rust_toolchain.py,target=/tmp/rust_toolchain.py,ro' \
        'source=rust-toolchain,target=/tmp/rust-toolchain,ro'; do
        case "$installer_line" in
            *"$token"*) ;;
            *) fail "rust-dev: installer RUN lacks the reviewed runtime bind: $token" ;;
        esac
    done
    # Exact shape, not a substring: a disabled, wrapped or extended command
    # (`: # ...`, `|| true`, an extra argv) is not the reviewed install.
    [ "$(normalize_instruction "$installer_line")" = "$reviewed_installer" ] ||
        fail "rust-dev: installer RUN must be exactly: $reviewed_installer (got: $(normalize_instruction "$installer_line"))"
    verifier_runs=$(printf '%s\n' "$instructions" | grep -cF 'verify-toolchain.sh' || true)
    [ "$verifier_runs" -eq 1 ] || fail 'rust-dev: verifier RUN must appear exactly once'
    verifier_line=$(printf '%s\n' "$instructions" | grep -F 'verify-toolchain.sh')
    case "$verifier_line" in
        *--network=none*) ;;
        *) fail 'rust-dev: verifier must run with --network=none' ;;
    esac
    for token in 'verify-toolchain.sh,target=/run/rust-verify/verify-toolchain.sh,ro' \
        'rust_toolchain.py,target=/run/rust-verify/rust_toolchain.py,ro' \
        'rust-toolchain,target=/run/rust-verify/rust-toolchain,ro' \
        'RUST_TOOLCHAIN=$RUST_TOOLCHAIN' 'VERUS_DIR=/opt/verus' 'PYTHONDONTWRITEBYTECODE=1'; do
        case "$verifier_line" in
            *"$token"*) ;;
            *) fail "rust-dev: verifier RUN lacks the reviewed delivery: $token" ;;
        esac
    done
    [ "$(normalize_instruction "$verifier_line")" = "$reviewed_verifier" ] ||
        fail "rust-dev: verifier RUN must be exactly: $reviewed_verifier (got: $(normalize_instruction "$verifier_line"))"
    grep -qxF 'ENV RUSTUP_AUTO_INSTALL=0' "$rustdir/Dockerfile" ||
        fail 'rust-dev: missing the no-implicit-install contract (ENV RUSTUP_AUTO_INSTALL=0)'
    grep -qxF 'ENV RUSTUP_HOME=/opt/rustup' "$rustdir/Dockerfile" ||
        fail 'rust-dev: RUSTUP_HOME ENV'
    grep -qxE 'ENV PATH=/opt/cargo/bin:/opt/verus/verus-x86-linux:\$\{PATH\}' "$rustdir/Dockerfile" ||
        fail 'rust-dev: PATH must stay additive'
    if grep -qE '^ENV ([A-Za-z_]+=)*CARGO_HOME=' "$rustdir/Dockerfile"; then
        fail 'rust-dev: CARGO_HOME must not be exported at runtime'
    fi
    if grep -qE '^ENV ([A-Za-z_]+=)*RUSTUP_TOOLCHAIN=' "$rustdir/Dockerfile"; then
        fail 'rust-dev: RUSTUP_TOOLCHAIN must not be forced'
    fi
    for file in "$rustdir/Dockerfile" "$rustdir/install-rust.sh"; do
        if grep -nE 'sh\.rustup\.rs|channel-rust-(stable|beta|nightly)|rustup[[:space:]]+target[[:space:]]+add|(^|[^[:alnum:]_])(curl|wget)[[:space:]]' "$file"; then
            fail "rust-dev: live bootstrap, mutable channel or first-use fetch in ${file##*/}"
        fi
        if grep -nE 'upgrade-rust-toolchain|rust-toolchain-update' "$file"; then
            fail "rust-dev: maintenance writer must not run in an ordinary build: ${file##*/}"
        fi
    done
    verifier=$rustdir/verify-toolchain.sh
    for token in 'if [ "$arch" = amd64 ]; then' '--crate-type=lib' \
        'verification results:: [1-9][0-9]* verified, 0 errors' 'verus! {'; do
        grep -qF -- "$token" "$verifier" || fail "rust-dev: Verus smoke lacks: $token"
    done
    if grep -nE '\|\|[[:space:]]*(true|:)' "$verifier"; then
        fail 'rust-dev: Verus smoke must not swallow failures'
    fi
    for arg in GO_AMD64_SHA256 GO_ARM64_SHA256 GOLANGCI_LINT_AMD64_SHA256 GOLANGCI_LINT_ARM64_SHA256; do
        grep -qE "^ARG $arg=[0-9a-f]{64}$" "$LAYERS/go-dev/Dockerfile" || fail "go-dev: digest ARG shape: $arg"
    done
    for rel in go-dev/install-go.sh go-dev/install-golangci-lint.sh; do
        file="$LAYERS/$rel"
        [ -f "$file" ] || fail "guarded script missing: $rel"
        checksum_token "$file" || fail "go-dev: ${rel##*/} lacks non-comment sha256sum token"
    done
    for file in images/Dockerfile images/standard/Dockerfile; do
        [ -f "$file" ] || fail "recipe text missing: $file"
        if grep -qiE 'chromium|google-chrome|agent-vm-chrome-mcp|agent-vm-capabilities' "$file"; then
            fail "recipe text contains Chrome token: $file"
        fi
    done
    for rel in $GUARDED; do
        [ -f "$LAYERS/$rel" ] || fail "guarded script missing: $rel"
    done
    while IFS= read -r -d '' file; do
        if [ "$(head -c 2 "$file")" = '#!' ]; then
            rel=${file#"$LAYERS/"}
            case " $GUARDED " in *" $rel "*) ;; *) fail "unguarded example script: $rel" ;; esac
        fi
    done < <(find "$LAYERS" -type f -print0)
    # Lint by interpreter: the Python helper's single `check` invocation above is
    # its parser, so there is no bash -n, no ShellCheck and no second `check`
    # masquerading as a syntax lint; contracts-python.py adds independent AST
    # coverage for every repository Python file.
    for rel in $GUARDED; do
        case "$(head -n 1 "$LAYERS/$rel")" in
            '#!'*python*) ;;
            *)
                "$BASH" -n "$LAYERS/$rel" || fail "syntax: $rel"
                shellcheck "$LAYERS/$rel" || fail "shellcheck: $rel"
                ;;
        esac
    done
    echo 'example layers passed'
}
# Rewrites preserve copied file modes on both BSD and GNU hosts.
rewrite() {
    local file=$1 expression=$2
    sed "$expression" "$file" > "$SCRATCH/rewrite"
    cat "$SCRATCH/rewrite" > "$file"
}
# Re-pin a copied tree's inputs: the byte pins are what a mutation must trip.
repin() {
    ( cd "$1/$LAYERS/chrome-devtools/chrome-devtools-mcp" &&
        sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml > pnpm-inputs.sha256 )
}
self_test() {
    SCRATCH=$(mktemp -d)
    trap 'rm -rf "$SCRATCH"' EXIT
    local n tree chrome go rust wrapper expected rc telemetry file pnpmdir bundle lock
    for n in $(seq 1 79); do
        tree="$SCRATCH/case-$n"
        mkdir -p "$tree/examples" "$tree/images/standard"
        cp -R "$ROOT/$LAYERS" "$tree/examples/"
        cp "$ROOT/images/Dockerfile" "$tree/images/"
        cp "$ROOT/images/standard/Dockerfile" "$tree/images/standard/"
        chrome="$tree/$LAYERS/chrome-devtools/Dockerfile"
        wrapper="$tree/$LAYERS/chrome-devtools/agent-vm-chrome-mcp"
        pnpmdir="$tree/$LAYERS/chrome-devtools/chrome-devtools-mcp"
        bundle="$tree/$LAYERS/chrome-devtools/chrome-devtools-mcp.sigstore.json"
        lock="$pnpmdir/pnpm-lock.yaml"
        go="$tree/$LAYERS/go-dev/Dockerfile"
        rust="$tree/$LAYERS/rust-dev/install-verus.sh"
        rusttool="$tree/$LAYERS/rust-dev/rust-toolchain"
        rustdocker="$tree/$LAYERS/rust-dev/Dockerfile"
        rustverify="$tree/$LAYERS/rust-dev/verify-toolchain.sh"
        expected=''
        case "$n" in
            1) ;;
            2) echo '# after marker' >> "$chrome"; expected='Chrome marker must be written last' ;;
            3) rewrite "$chrome" '/getent passwd 9999/d'; expected='Chrome example lacks: getent passwd 9999' ;;
            4) rewrite "$chrome" 's|COPY --chmod=0755 agent-vm-chrome-mcp /usr/local/bin/agent-vm-chrome-mcp|COPY --chmod=0755 agent-vm-chrome-mcp /usr/local/bin/agent-vm-chrome-mcp2|'; expected='Chrome wrapper must be installed at' ;;
            5) echo 'true || true' >> "$wrapper"; expected='Chrome wrapper must not swallow failures' ;;
            6) rewrite "$wrapper" '/cannot cd to HOME/d'; expected='Chrome wrapper lacks: cannot cd to HOME' ;;
            7) rewrite "$go" 's|^ARG BASE_IMAGE=.*|ARG BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard:latest|'; expected='parent pin: ARG shape:' ;;
            8)
                file="$tree/$LAYERS/README.md"
                awk '!changed && /sha256:0/ { sub(/sha256:0/, "sha256:1"); changed=1 } { print }' "$file" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$file"
                expected='parent pin: references disagree' ;;
            9) rewrite "$tree/$LAYERS/wirenboard-cpp/Dockerfile" 's|^FROM .*|FROM debian:13|'; expected='parent pin: FROM shape:' ;;
            10) rewrite "$rust" '/sha256sum -c -/d'; expected='rust-dev: install-verus.sh lacks non-comment sha256sum token' ;;
            11) rewrite "$rust" '/sha256sum -c -/s/^/# /'; expected='rust-dev: install-verus.sh lacks non-comment sha256sum token' ;;
            12) rewrite "$tree/$LAYERS/rust-dev/Dockerfile" '/^ARG VERUS_SHA256=/s/.$//'; expected='rust-dev pins:' ;;
            13) rewrite "$tree/$LAYERS/go-dev/install-golangci-lint.sh" '/sha256sum/d'; expected='go-dev: install-golangci-lint.sh lacks non-comment sha256sum token' ;;
            14) echo 'RUN apt-get install -y chromium' >> "$tree/images/standard/Dockerfile"; expected='recipe text contains Chrome token:' ;;
            15) rm "$tree/$LAYERS/go-dev/install-gopls.sh"; expected='guarded script missing: go-dev/install-gopls.sh' ;;
            16) printf '#!/bin/sh\n' > "$tree/$LAYERS/rust-dev/extra-tool"; expected='unguarded example script: rust-dev/extra-tool' ;;
            17) echo 'if then' >> "$tree/$LAYERS/rust-dev/install-rust.sh"; expected='syntax: rust-dev/install-rust.sh' ;;
            18)
                # Literal child code is the SC2086 negative, not parent expansion.
                # shellcheck disable=SC2016
                printf '\nx="a b"\necho $x\n' >> "$tree/$LAYERS/go-dev/verify-toolchain.sh"
                expected='shellcheck: go-dev/verify-toolchain.sh' ;;
            19) rewrite "$go" '/^FROM /d'; expected='parent pin: FROM shape:' ;;
            20) echo 'ARG BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard:latest' >> "$go"; expected='parent pin: ARG shape:' ;;
            21)
                awk '/^ARG BASE_IMAGE=/ { arg=$0; next } { print } /^FROM / { print arg }' "$go" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$go"
                expected='parent pin: ARG shape:' ;;
            22)
                awk '/^FROM / { print "arg BASE_IMAGE=debian:13" } { print }' "$go" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$go"
                expected='parent pin: ARG shape:' ;;
            23) echo 'from debian:13' >> "$go"; expected='parent pin: FROM shape:' ;;
            24)
                awk '/^FROM / { print "arg \\\n BASE_IMAGE=debian:13" } { print }' "$go" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$go"
                expected='parent pin: ARG shape:' ;;
            25) printf '\nFR\\\nOM debian:13\n' >> "$go"; expected='parent pin: FROM shape:' ;;
            26)
                printf '# escape=`\n' > "$SCRATCH/rewrite"
                sed 's/\\$/`/' "$go" >> "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$go"
                printf '\nFROM`\n debian:13\n' >> "$go"
                expected='parent pin: FROM shape:' ;;
            27) printf '\nFROM\\\r\n debian:13\r\n' >> "$go"; expected='parent pin: FROM shape:' ;;
            28)
                awk '/^FROM / { print "ARG OTHER=unused BASE_IMAGE=debian:13" } { print }' "$go" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$go"
                expected='parent pin: ARG shape:' ;;
            29)
                printf "\nRUN cat > /tmp/example-text <<'EOF'\nfrom debian:13\n" >> "$go"
                expected='parent pin: unterminated heredoc:' ;;
            30)
                printf "\nRUN cat > /tmp/example-text <<'EOF'\nfrom debian:13\narg BASE_IMAGE=debian:13\nEOF\n" >> "$go" ;;
            31) printf '\nRUN true \\\n && true \\\n && true\n' >> "$go" ;;
            32)
                printf '\nRUN cat <<-EOF <<"SECOND"\n\tfrom debian:13\n\tEOF\narg BASE_IMAGE=debian:13\nSECOND\n' >> "$go" ;;
            33)
                printf '# escape = `\n' > "$SCRATCH/rewrite"
                sed 's/\\$/`/' "$go" | awk '/^FROM / { print "ARG `\n BASE_IMAGE=debian:13" } { print }' >> "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$go"
                expected='parent pin: ARG shape:' ;;
            34)
                printf '# escape\t=\t`\n' > "$SCRATCH/rewrite"
                sed 's/\\$/`/' "$go" | awk '/^FROM / { print "ARG `\n BASE_IMAGE=debian:13" } { print }' >> "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$go"
                expected='parent pin: ARG shape:' ;;
            # Cases 35-50: the pinned MCP preinstall (#41). 35-37, 39-41 and
            # 46-49 re-pin, so only the content rule under test can fail.
            35) printf ' ' >> "$lock"; expected='pnpm-lock.yaml: bytes differ from the reviewed pin' ;;
            36) rm "$lock"; expected='missing input: pnpm-lock.yaml' ;;
            37) awk '$2 != "pnpm-lock.yaml"' "$pnpmdir/pnpm-inputs.sha256" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$pnpmdir/pnpm-inputs.sha256"
                expected='pin file must name exactly' ;;
            38) : > "$pnpmdir/.npmrc"; expected='unreviewed input: .npmrc' ;;
            39)
                jq '.dependencies["chrome-devtools-mcp"] = "^1.0.1"' "$pnpmdir/package.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$pnpmdir/package.json"
                repin "$tree"
                expected='selection must be one canonical exact version' ;;
            40) echo 'trustLockfile: true' >> "$pnpmdir/pnpm-workspace.yaml"
                repin "$tree"
                expected='policy escape in pnpm-workspace.yaml: trustLockfile' ;;
            41) awk '/^snapshots:/ && !done { print "  other-pkg@1.0.0:"; done = 1 } { print }' "$lock" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$lock"
                repin "$tree"
                expected='closure shape changed; review required' ;;
            42)
                awk '/^RUN chromium --version/ && !done { print "RUN sudo -u chrome -H npx -y chrome-devtools-mcp@1.0.1 --help"; done = 1 } { print }' "$chrome" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$chrome"
                expected='must not fetch at first use or skip install' ;;
            43) rewrite "$chrome" 's#sh /tmp/chrome-devtools-mcp.sh install#sh /tmp/chrome-devtools-mcp.sh install || echo skipped#'
                expected='Chrome MCP install must be exactly' ;;
            44) awk '/^    npx\)$/ { skip = 1 } skip && /^        ;;$/ { skip = 0; next } !skip { print }' "$wrapper" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$wrapper"
                expected='Chrome wrapper lacks: "chrome-devtools-mcp@$installed"' ;;
            45)
                zeroes=$(awk 'BEGIN { for (i = 0; i < 128; i++) printf "0" }')
                jq --arg h "$zeroes" '
                    .dsseEnvelope.payload |= (@base64d | fromjson | .subject[0].digest.sha512 = $h | tojson | @base64)
                ' "$bundle" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$bundle"
                expected='provenance does not match the lock' ;;
            46) rewrite "$lock" 's/integrity: sha512-\(.*\)xg==}/integrity: sha512-\1xw==}/'
                repin "$tree"
                expected='provenance does not match the lock' ;;
            47) rm "$tree/$LAYERS/chrome-devtools/chrome-devtools-mcp.sh"; expected='guarded script missing: chrome-devtools/chrome-devtools-mcp.sh' ;;
            48)
                jq '.packageManager = "pnpm@11.11"' "$pnpmdir/package.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$pnpmdir/package.json"
                repin "$tree"
                expected='packageManager must be pnpm@EXACT' ;;
            49) awk '$0 != "allowBuilds: {}"' "$pnpmdir/pnpm-workspace.yaml" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$pnpmdir/pnpm-workspace.yaml"
                repin "$tree"
                expected='must keep allowBuilds: {}' ;;
            # 50 proves repin itself is byte-faithful, so 39-41/46/48/49 fail on
            # content rather than on a broken pin file.
            50) repin "$tree" ;;
            # 51-79: the reviewed Rust toolchain closure and its recipe (#102).
            # Cases 55-60 mutate pin selection data while leaving the original
            # signed evidence intact, so the semantic rule must reject them.
            51) rm "$rusttool/pin.json"; expected='missing input: pin.json' ;;
            52) printf ' ' >> "$rusttool/channel.toml"; expected='bytes differ from the committed digest' ;;
            53) rm "$rusttool/channel.toml.asc"; expected='missing input: channel.toml.asc' ;;
            54) printf ' ' >> "$rusttool/rust-release-key.asc"; expected='bytes differ' ;;
            55)
                jq 'del(.platforms["linux/arm64"])' "$rusttool/pin.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rusttool/pin.json"
                expected='platforms must be exactly linux/amd64 and linux/arm64' ;;
            56)
                jq '.platforms["linux/arm64"].host = "x86_64-unknown-linux-gnu"' "$rusttool/pin.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rusttool/pin.json"
                expected='host/musl triples do not match the architecture' ;;
            57)
                jq '(.platforms["linux/amd64"].archives) |= map(select(.target | endswith("-musl") | not))' \
                    "$rusttool/pin.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rusttool/pin.json"
                expected='selected closure is incomplete' ;;
            58)
                jq '.platforms["linux/amd64"].archives += [(.platforms["linux/amd64"].archives[0] | .package = "rust-docs")]' \
                    "$rusttool/pin.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rusttool/pin.json"
                expected='unexpected selected archive' ;;
            59)
                jq '.platforms["linux/arm64"].archives[0].sha256 = ("0" * 64)' "$rusttool/pin.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rusttool/pin.json"
                expected='differs from the pin' ;;
            60)
                jq '.platforms["linux/amd64"].archives[0].url |= sub("static\\.rust-lang\\.org"; "static.rust-lang.org.evil.test")' \
                    "$rusttool/pin.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rusttool/pin.json"
                expected='not a canonical rust-lang.org HTTPS URL' ;;
            61) rm "$rusttool/evidence/amd64-cargo.asc"; expected='missing input: evidence/amd64-cargo.asc' ;;
            62)
                jq '.bootstrap_exception.method = "assumed-signed"' "$rusttool/pin.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rusttool/pin.json"
                expected='hash-only exception' ;;
            63)
                rewrite "$rustdocker" 's|^ARG RUST_TOOLCHAIN=.*|ARG RUST_TOOLCHAIN=1.0.0|'
                expected='asserted channel' ;;
            64)
                rewrite "$rustdocker" '/source=rust_toolchain.py,target=\/tmp\/rust_toolchain.py,ro/d'
                expected='installer RUN lacks the reviewed runtime bind: source=rust_toolchain.py' ;;
            65)
                rewrite "$rustdocker" '/source=rust-toolchain,target=\/tmp\/rust-toolchain,ro/d'
                expected='installer RUN lacks the reviewed runtime bind: source=rust-toolchain' ;;
            66)
                rewrite "$rustdocker" 's|bash /tmp/install-rust.sh|bash /tmp/install-rust.sh \|\| echo skipped|'
                expected='installer RUN must be exactly' ;;
            67)
                echo 'RUN curl -sSf https://sh.rustup.rs | sh' >> "$rustdocker"
                expected='live bootstrap' ;;
            68)
                rewrite "$rustdocker" '/^ENV RUSTUP_AUTO_INSTALL=0/d'
                expected='no-implicit-install contract' ;;
            69)
                echo 'ENV CARGO_HOME=/opt/cargo' >> "$rustdocker"
                expected='CARGO_HOME must not be exported' ;;
            70)
                echo 'ENV RUSTUP_TOOLCHAIN=1.98.1' >> "$rustdocker"
                expected='RUSTUP_TOOLCHAIN must not be forced' ;;
            71)
                rewrite "$rustdocker" 's|RUN --network=none|RUN|'
                expected='verifier must run with --network=none' ;;
            72) printf 'unexpected' > "$rusttool/.extra"; expected='unreviewed input: .extra' ;;
            73)
                rm "$rusttool/channel.toml"
                ln -s channel.toml.asc "$rusttool/channel.toml"
                expected='symlink channel.toml' ;;
            74)
                mv "$rusttool/evidence" "$SCRATCH/evidence-away"
                ln -s "$SCRATCH/evidence-away" "$rusttool/evidence"
                expected='symlink evidence' ;;
            75) rm "$tree/$LAYERS/rust-dev/rust_toolchain.py"
                expected='guarded script missing: rust-dev/rust_toolchain.py' ;;
            76)
                awk 'BEGIN { skip = 0 }
                     /^if \[ "\$arch" = amd64 \]; then$/ { skip = 1 }
                     skip && /^fi$/ { skip = 0; next }
                     !skip { print }' "$rustverify" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rustverify"
                expected='Verus smoke lacks: if [ "$arch" = amd64 ]; then' ;;
            77)
                rewrite "$rustverify" 's|"\$verus" --crate-type=lib smoke.rs|"$verus" --version|'
                expected='Verus smoke lacks: --crate-type=lib' ;;
            78)
                awk '/verification results:: \[1-9\]/ { print $0 " || true"; next } { print }' \
                    "$rustverify" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rustverify"
                expected='must not swallow failures' ;;
            79) : ;;
            # 80-81: a skipped install and a swallowed verifier failure. Both
            # keep every mount and token, so only the exact shape can catch them.
            80)
                rewrite "$rustdocker" 's|bash /tmp/install-rust.sh|: # bash /tmp/install-rust.sh|'
                expected='installer RUN must be exactly' ;;
            81)
                rewrite "$rustdocker" 's|/bin/bash /run/rust-verify/verify-toolchain.sh|/bin/bash /run/rust-verify/verify-toolchain.sh \|\| true|'
                expected='verifier RUN must be exactly' ;;
        esac
        rc=0
        EXAMPLE_LAYERS_REPORT_BASH=1 "$BASH" "$CHECKER" --root "$tree" > "$SCRATCH/out" 2>&1 || rc=$?
        telemetry="interpreter $BASH $BASH_VERSION"
        grep -qxF "$telemetry" "$SCRATCH/out" || fail "case $n: child interpreter mismatch"
        echo "case $n: $telemetry"
        if [ -z "$expected" ]; then
            [ "$rc" -eq 0 ] || { cat "$SCRATCH/out" >&2; fail "case $n: expected pass, got $rc"; }
        elif [ "$rc" -ne 1 ] || ! grep -qF "$expected" "$SCRATCH/out"; then
            cat "$SCRATCH/out" >&2
            fail "case $n: expected $expected, got $rc/$(cat "$SCRATCH/out")"
        fi
        echo "case $n passed: ${expected:-pass}"
    done
    echo 'example layers self-test passed (81 cases)'
}
case "${1:-}" in
    '') [ "$#" -eq 0 ] || usage ;;
    --root) [ "$#" -eq 2 ] && [ -n "$2" ] || usage; ROOT=$2 ;;
    --self-test) [ "$#" -eq 1 ] || usage; self_test; exit 0 ;;
    *) usage ;;
esac
if [ "${EXAMPLE_LAYERS_REPORT_BASH:-0}" = 1 ]; then
    printf 'interpreter %s %s\n' "$BASH" "$BASH_VERSION"
fi
check
