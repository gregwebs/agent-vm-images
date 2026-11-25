#!/usr/bin/env bash
# Static image contracts only; launcher agreement and native builds have other owners.
set -euo pipefail
CHECKER="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# Image-owned interface: agent-vm's CHROME_MCP_CAPABILITY_PATH and
# CHROME_MCP_WRAPPER_PATH (crates/agent-vm/src/defaults.rs) must name these
# paths; agent-vm checks that side (docs/image-source-ownership.md).
CHROME_MARKER=/etc/agent-vm-capabilities/chrome-devtools-mcp
CHROME_WRAPPER=/usr/local/bin/agent-vm-chrome-mcp
LAYERS=examples/layers
EXAMPLES='chrome-devtools go-dev rust-dev wirenboard-cpp'
# The reviewed ARG sets are one committed data file, the only copy: the Rust gate
# reads the same file (script/test/image-sources, `include_str!`), so the two
# cannot drift about which names are reviewed. It sits next to this script rather
# than under ROOT because it is part of the gate, not of the tree under review;
# its header carries the rule (#235, #237).
REVIEWED_ARGS_FILE=${CHECKER%/*}/reviewed-args.txt
# The host helper build wrapper, through this script's own location so the
# self-test builds the tree it is reviewing (`--root "$ROOT"`) and never the
# checkout it was started from.
HELPER_BUILD=${CHECKER%/*}/../build/example-helper.sh
# Known space-free paths, explicitly moved from agent-vm's ci-contracts.sh:
# deletion/rename must fail rather than silently leave lint coverage.
GUARDED='chrome-devtools/agent-vm-chrome-mcp chrome-devtools/chrome-devtools-mcp.sh go-dev/gopls_closure.py go-dev/install-go.sh go-dev/install-golangci-lint.sh go-dev/install-gopls.sh go-dev/verify-toolchain.sh rust-dev/install-kache.sh rust-dev/install-rust.sh rust-dev/install-verus.sh rust-dev/verify-toolchain.sh'
# A layer may also carry its own Go module under `helper/` (ADR 0017, example
# layers stay self-contained). Its non-vendor files are named here one entry per
# file, like GUARDED_DATA: a reviewer of a new source file sees it in the diff, a
# listed file that vanishes fails as `guarded script missing`, and a `.go` file
# nobody listed fails as `unreviewed helper file`. The vendor tree is deliberately
# out of this list: its bytes belong to the module's vendor drift gate.
GUARDED_GO='rust-dev/helper/go.mod rust-dev/helper/go.sum rust-dev/helper/main.go rust-dev/helper/main_test.go rust-dev/helper/imports_test.go rust-dev/helper/seams_fixture.go rust-dev/helper/seams_shipped.go rust-dev/helper/seams_test.go rust-dev/helper/kacheprobe/probe.go rust-dev/helper/kacheprobe/probe_test.go rust-dev/helper/internal/gpg/gpg.go rust-dev/helper/internal/gpg/gpg_test.go rust-dev/helper/internal/strictjson/decode.go rust-dev/helper/internal/strictjson/decode_test.go rust-dev/helper/toolchain/toolchain.go rust-dev/helper/toolchain/policy.go rust-dev/helper/toolchain/types.go rust-dev/helper/toolchain/refusal.go rust-dev/helper/toolchain/decode.go rust-dev/helper/toolchain/pin.go rust-dev/helper/toolchain/inputs.go rust-dev/helper/toolchain/manifest.go rust-dev/helper/toolchain/installed.go rust-dev/helper/toolchain/commands.go rust-dev/helper/toolchain/types_test.go rust-dev/helper/toolchain/decode_test.go rust-dev/helper/toolchain/pin_test.go rust-dev/helper/toolchain/manifest_test.go rust-dev/helper/toolchain/installed_test.go rust-dev/helper/toolchain/fuzz_test.go rust-dev/helper/toolchain/events.go rust-dev/helper/toolchain/preflight.go rust-dev/helper/toolchain/prepare.go rust-dev/helper/toolchain/install.go rust-dev/helper/toolchain/stage.go rust-dev/helper/toolchain/stage_test.go rust-dev/helper/toolchain/preflight_test.go rust-dev/helper/toolchain/prepare_test.go rust-dev/helper/toolchain/install_test.go rust-dev/helper/toolchain/fuzz_install_test.go rust-dev/helper/helpers_test.go rust-dev/helper/internal/fetch/fetch.go rust-dev/helper/internal/fetch/loopback_fixture.go rust-dev/helper/internal/fetch/fetch_test.go rust-dev/helper/internal/fetch/loopback_fixture_test.go rust-dev/helper/internal/tarwalk/tarwalk.go rust-dev/helper/internal/tarwalk/tarwalk_test.go'
# The rust-dev helper's builder stage. It is a reviewed parent:
# `images/parent-pins.json` records its digest, and its tag is the root go.mod's
# toolchain. Both are named by the literals below rather than by a variable, so a
# reviewed line reads as one string.

# The other half of the same contract, with the same weight: every committed
# input that is not a script is a file under its layer's `data/` directory and is
# named here. One entry is one file, so a reviewer of a new pin sees it in the
# diff; a listed file that vanishes fails as `guarded script missing`, and a file
# under `data/` that is listed nowhere fails as `unreviewed layer input`.
GUARDED_DATA='chrome-devtools/data/chrome-devtools-mcp.sigstore.json chrome-devtools/data/chrome-devtools-mcp/package.json chrome-devtools/data/chrome-devtools-mcp/pnpm-inputs.sha256 chrome-devtools/data/chrome-devtools-mcp/pnpm-lock.yaml chrome-devtools/data/chrome-devtools-mcp/pnpm-workspace.yaml go-dev/data/gopls-closure/go.mod go-dev/data/gopls-closure/go.sum go-dev/data/gopls-closure/gopls.go.mod go-dev/data/gopls-closure/pin.json go-dev/data/gopls-closure/THIRD_PARTY_LICENSES rust-dev/data/kache-config.toml rust-dev/data/kache/README.md rust-dev/data/kache/kache-aarch64-unknown-linux-musl.tar.gz.sha256 rust-dev/data/kache/kache-x86_64-unknown-linux-musl.tar.gz.sha256 rust-dev/data/kache/release-key.asc rust-dev/data/rust-toolchain/channel.toml rust-dev/data/rust-toolchain/channel.toml.asc rust-dev/data/rust-toolchain/channel.toml.sha256 rust-dev/data/rust-toolchain/evidence/amd64-cargo.asc rust-dev/data/rust-toolchain/evidence/amd64-clippy-preview.asc rust-dev/data/rust-toolchain/evidence/amd64-rust-std-musl.asc rust-dev/data/rust-toolchain/evidence/amd64-rust-std.asc rust-dev/data/rust-toolchain/evidence/amd64-rustc.asc rust-dev/data/rust-toolchain/evidence/amd64-rustfmt-preview.asc rust-dev/data/rust-toolchain/evidence/arm64-cargo.asc rust-dev/data/rust-toolchain/evidence/arm64-clippy-preview.asc rust-dev/data/rust-toolchain/evidence/arm64-rust-std-musl.asc rust-dev/data/rust-toolchain/evidence/arm64-rust-std.asc rust-dev/data/rust-toolchain/evidence/arm64-rustc.asc rust-dev/data/rust-toolchain/evidence/arm64-rustfmt-preview.asc rust-dev/data/rust-toolchain/evidence/rustup-amd64.sha256 rust-dev/data/rust-toolchain/evidence/rustup-arm64.sha256 rust-dev/data/rust-toolchain/pin.json rust-dev/data/rust-toolchain/rust-release-key.asc'
# The self-test table's case count. The case labels are checked against it as a
# complete 1..N sequence before any case runs, so a duplicated, skipped or
# appended label fails instead of silently not running (81 labels were defined
# while the loop stopped at 79).
SELF_TEST_CASES=163
fail() { echo "$*" >&2; exit 1; }
usage() { echo "usage: example-layers.sh [--self-test | --root DIR]" >&2; exit 2; }
checksum_token() {
    awk '!/^[[:space:]]*#/ && /sha256sum/ { found=1 } END { exit !found }' "$1"
}
# Only instruction boundaries for parent checks: no shell/JSON parsing, variable
# expansion or general Dockerfile validation. Heredoc words must be simple;
# quoted/escaped shell context before an operator is unsupported, not guessed.
# Fast drift guard limits: no full quoted heredoc delimiter grammar, distinction
# of heredoc-looking LABEL/ENV values, escaped-backslash boundaries, or a repeated
# escape directive.
#
# Parser directives are the exception to "no general Dockerfile validation", and
# the only part of this reading that is a build verdict rather than a shape
# check. BuildKit reads a directive only before the first comment, empty line or
# instruction, and `syntax=` replaces the frontend that reads every later line:
# it would pull an unreviewed image that defeats the FROM pins, `--network=none`
# and the stage allowlist at once (#235). So `escape` is the one directive this
# file honours, every other directive is refused by name, and a directive outside
# that leading window is refused instead of skipped as a comment - BuildKit treats
# it as a comment there, and a gate that honoured one anyway would be reporting on
# a parse the build never makes. A directive-shaped comment whose key BuildKit
# does not know (`# GOTOOLCHAIN=local ...`) stays a comment, as it is to BuildKit.
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
            if ($0 ~ /^[ \t]*#[ \t]*[A-Za-z][A-Za-z0-9]*[ \t]*=/) {
                key=$0
                sub(/^[^#]*#[ \t]*/, "", key)
                sub(/[ \t]*=.*$/, "", key)
                key=tolower(key)
                if (!leading) {
                    if (key != "escape" && key != "syntax" && key != "check") next
                    reject("parser directive outside the leading window: " key " (BuildKit stops reading directives at the first comment, empty line or instruction)")
                }
                if (key != "escape")
                    reject("unsupported parser directive: " key " (only escape is reviewed: syntax= replaces the frontend that reads this recipe)")
                directive=$0
                sub(/^[^=]*=[ \t]*/, "", directive)
                sub(/[ \t]+$/, "", directive)
                if (directive != "\\" && directive != "`") reject("unsupported escape directive: " directive)
                escape=directive
                next
            }
            if ($0 ~ /^[ \t]*(#|$)/) { leading=0; next }
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
# One recipe's reviewed ARG names, read from the shared data file: a recipe with
# no line, or with more than one, has no reviewed set to check against.
reviewed_args() { # EXAMPLE
    awk -v recipe="$LAYERS/$1/Dockerfile" '
        $1 == recipe {
            lines++
            for (i = 2; i <= NF; i++) print $i
        }
        END { exit !(lines == 1) }
    ' "$REVIEWED_ARGS_FILE"
}
# A global ARG is the one instruction that can precede FROM, so it reaches the
# parent line while every other instruction check looks after it. The reviewed
# set is what refuses a new one by name.
review_args() { # EXAMPLE INSTRUCTIONS
    local example=$1 instructions=$2 reviewed
    reviewed=$(reviewed_args "$example") || fail "ARG review: $example has no reviewed ARG set"
    printf '%s\n' "$instructions" | REVIEWED_ARGS=$reviewed awk -v example="$example" '
        function reject(message) {
            print "ARG review: " message > "/dev/stderr"
            failed=1
            exit 1
        }
        # Docker accepts space, tab, vertical tab, form feed and carriage return
        # after a keyword. The default awk field split knows only some of those
        # (BSD awk does not split on the vertical tab), so the separators are
        # normalized before the keyword and the names are read; the Rust gate
        # matches the same set (#237, #238 review). The shell single-quotes this
        # program, so no apostrophe may appear in any of its comments.
        function normalize(line) {
            gsub(/[[:space:]]+/, " ", line)
            sub(/^ /, "", line)
            return line
        }
        BEGIN {
            # A bare path in the data file is a reviewed recipe that declares no
            # ARG, so an empty set is legal and must not become one empty name.
            if (ENVIRON["REVIEWED_ARGS"] != "")
                for (i = 1; i <= split(ENVIRON["REVIEWED_ARGS"], names, "\n"); i++) reviewed_name[names[i]]=1
        }
        {
            instruction=normalize($0)
        }
        toupper(instruction) ~ /^FROM([[:space:]]|$)/ { stage=1 }
        toupper(instruction) ~ /^ARG / {
            count=split(instruction, operands, " ")
            for (i = 2; i <= count; i++) {
                name=operands[i]
                sub(/=.*$/, "", name)
                declared[name]=1
                if (!stage && !(name in reviewed_name))
                    reject(example " declares an unreviewed global ARG: " name)
            }
        }
        END {
            if (failed) exit 1
            for (name in reviewed_name)
                if (!(name in declared))
                    reject(example " no longer declares the reviewed ARG " name)
        }
    '
}
check() {
    [ -d "$ROOT" ] && [ -r "$ROOT" ] && [ -x "$ROOT" ] || fail "root missing: $ROOT"
    cd "$ROOT" || fail "root missing: $ROOT"
    local example dockerfile instructions first refs token wrapper arg file rel rest
    # Presence before content, because a guarded file that is gone is the sweep's
    # own message and not a shell or grep error from an assertion that reads it.
    # The data list is consulted at the end instead: the layer helpers read those
    # files first and report a deletion as their own `missing input: ...`.
    [ -f "$REVIEWED_ARGS_FILE" ] || fail "reviewed ARG data file missing: $REVIEWED_ARGS_FILE"
    for rel in $GUARDED $GUARDED_GO; do
        [ -f "$LAYERS/$rel" ] || fail "guarded script missing: $rel"
    done
    for example in $EXAMPLES; do
        dockerfile="$LAYERS/$example/Dockerfile"
        [ -f "$dockerfile" ] || fail "parent pin: ARG shape: $example Dockerfile missing"
        instructions=$(logical_instructions "$dockerfile" "$example") || exit 1
        first=$(printf '%s\n' "$instructions" | awk 'NR == 1 { print }')
        # C2a first-ARG shape begin
        [[ "$first" =~ ^ARG\ BASE_IMAGE=ghcr\.io/gregwebs/agent-vm-standard@sha256:[0-9a-f]{64}$ ]] || fail "parent pin: ARG shape: $example"
        # C2a first-ARG shape end
        [ "$(printf '%s\n' "$instructions" | awk '
            # The same separator set as review_args above: the count must see a
            # BASE_IMAGE redeclaration however the keyword is separated.
            function normalize(line) {
                gsub(/[[:space:]]+/, " ", line)
                sub(/^ /, "", line)
                return line
            }
            {
                instruction=normalize($0)
                count=split(instruction, operands, " ")
                if (toupper(instruction) ~ /^ARG /)
                    for (i=2; i<=count; i++) if (operands[i] ~ /^BASE_IMAGE(=|$)/) found++
            }
            END { print found+0 }
        ')" -eq 1 ] || fail "parent pin: ARG shape: $example redefinition"
        if [ "$example" = rust-dev ]; then
            # The one example whose helper module is compiled in-image: its first
            # FROM is the pinned compiler stage and its last is the parent. The
            # exact builder line is asserted with the rest of the rust-dev recipe
            # below; here only the shape and the order are held, for every layer,
            # so a builder stage cannot appear in a layer that has no module.
            [ "$(printf '%s\n' "$instructions" | awk 'toupper($0) ~ /^[[:space:]]*FROM([[:space:]]|$)/' | wc -l)" -eq 2 ] ||
                fail "parent pin: FROM shape: $example must carry exactly the helper stage and the parent"
            printf '%s\n' "$instructions" |
                awk 'toupper($0) ~ /^[[:space:]]*FROM([[:space:]]|$)/ { count++; line[count] = $0 }
                     END { exit !(count == 2 && line[count] == "FROM ${BASE_IMAGE}") }' ||
                fail "parent pin: FROM shape: $example must end on FROM \${BASE_IMAGE}"
        else
            printf '%s\n' "$instructions" | awk 'toupper($0) ~ /^[[:space:]]*FROM([[:space:]]|$)/ { found=1; if ($0 != "FROM ${BASE_IMAGE}") bad=1 } END { exit (!found || bad) }' || fail "parent pin: FROM shape: $example"
        fi
        review_args "$example" "$instructions"
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
    for token in 'failed to prepare chrome NSS DB' 'sudo -u chrome -H -n' 'cannot cd to HOME'; do
        grep -qF "$token" "$wrapper" || fail "Chrome wrapper lacks: $token"
    done
    if grep -qF '|| true' "$wrapper"; then fail 'Chrome wrapper must not swallow failures'; fi
    # The preinstalled MCP selection is installed by exactly one reviewed helper
    # invocation over the committed inputs; nothing at first use resolves or
    # fetches (issue #41).
    mcpdir="$LAYERS/chrome-devtools/data/chrome-devtools-mcp"
    helper="$LAYERS/chrome-devtools/chrome-devtools-mcp.sh"
    instructions=$(logical_instructions "$dockerfile" chrome-devtools) || exit 1
    grep -qxF 'COPY data/chrome-devtools-mcp/ /opt/agent-vm/chrome-devtools-mcp/' "$dockerfile" ||
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
    bundle="$LAYERS/chrome-devtools/data/chrome-devtools-mcp.sigstore.json"
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
    for token in '^ARG RUST_TOOLCHAIN=[0-9]+\.[0-9]+(\.[0-9]+)?$' '^ARG VERUS_RELEASE=[^[:space:]]+$' '^ARG VERUS_SHA256=[0-9a-f]{64}$' \
        '^ARG KACHE_VERSION=[0-9]+\.[0-9]+\.[0-9]+$' '^ARG KACHE_AMD64_SHA256=[0-9a-f]{64}$' \
        '^ARG KACHE_ARM64_SHA256=[0-9a-f]{64}$' '^ARG KACHE_RELEASE_CERT_SHA256=[0-9a-f]{64}$'; do
        grep -qE "$token" "$dockerfile" || fail "rust-dev pins: $token"
    done
    file="$LAYERS/rust-dev/install-verus.sh"
    checksum_token "$file" || fail 'rust-dev: install-verus.sh lacks non-comment sha256sum token'
    # --- rust-dev: reviewed toolchain closure (#102) -------------------------
    # The committed data directory is the reviewed input set; the helper is the
    # only program that verifies and installs it. Directory presence is checked
    # before the helper is invoked so a whole missing directory reports itself;
    # the missing files inside it are the helper's own `missing input: ...`.
    rustdir=$LAYERS/rust-dev
    datadir=$rustdir/data/rust-toolchain
    [ -d "$datadir" ] || fail 'rust-dev: missing Rust input directory'
    rust_arg=$(sed -nE 's/^ARG RUST_TOOLCHAIN=(.*)$/\1/p' "$rustdir/Dockerfile")
    [ -n "$rust_arg" ] || fail 'rust-dev pins: ARG RUST_TOOLCHAIN'
    instructions=$(logical_instructions "$rustdir/Dockerfile" rust-dev) || exit 1
    # Normalized for comparison: `logical_instructions` keeps a continuation's
# spacing, so runs of blanks are collapsed before the exact shape comparison.
normalize_instruction() { printf '%s' "$1" | tr -s ' \t' ' ' | sed -e 's/^ //' -e 's/ $//'; }
    # --- rust-dev: the helper module and the compiler guard chain (#187) -----
    # The layer's own program is a Go module under helper/, compiled by the first
    # FROM and mounted into the verifier. Four links bind that compiler to the
    # repository, each with the check that fails when it is deleted:
    #
    #   1. the stage's own RUN: the image compiler must be helper/go.mod's
    #      `toolchain` line (asserted by the exact shape below, and it fails a
    #      build, not only a gate);
    #   2. helper/go.mod's `go` and `toolchain` lines must be the root go.mod's,
    #      so the two modules cannot select different compilers;
    #   3. the builder FROM must be exactly `golang:<root toolchain>-trixie` at the
    #      digest images/parent-pins.json records for `.pins["golang-builder"]`,
    #      so the reviewed version and the reviewed bytes move together;
    #   4. script/test/image-sources binds the same FROM to the same inventory
    #      reference.
    #
    # The stage itself is a closed instruction set (see the allowlist below): the
    # chain binds a compiler version and the recipe/inventory relationship, not
    # source bytes, so a comment-only edit under helper/ changes nothing any gate
    # sees.
    #
    helperdir=$rustdir/helper
    [ -f "$helperdir/vendor/modules.txt" ] ||
        fail 'rust-dev: helper/vendor is the build closure; it must stay committed'
    [ -f "$ROOT/go.mod" ] || fail 'rust-dev: missing the root go.mod the helper is bound to'
    root_go=$(awk '$1 == "go" { print $2 }' "$ROOT/go.mod")
    root_toolchain=$(awk '$1 == "toolchain" { print $2 }' "$ROOT/go.mod")
    [ -n "$root_go" ] && [ -n "$root_toolchain" ] ||
        fail 'rust-dev: the root go.mod must state a go and a toolchain line'
    helper_go=$(awk '$1 == "go" { print $2 }' "$helperdir/go.mod")
    helper_toolchain=$(awk '$1 == "toolchain" { print $2 }' "$helperdir/go.mod")
    [ "$helper_go" = "$root_go" ] ||
        fail "rust-dev: helper/go.mod's go line ($helper_go) must equal the root go.mod's ($root_go)"
    [ "$helper_toolchain" = "$root_toolchain" ] ||
        fail "rust-dev: helper/go.mod's toolchain ($helper_toolchain) must equal the root go.mod's ($root_toolchain)"
    # Link 5 -- the root module imports this one (#232), so the root binary and the
    # layer's build compile one library instead of two copies of the rules. Two
    # consequences are checked here: the import is a local replace at v0.0.0 (that
    # is what puts the layer's own source in vendor/, which
    # internal/vendordrift then compares byte for byte), and every requirement the
    # two modules share is at one version, because minimal version selection in the
    # root could otherwise compile the helper against a version the layer's own
    # build never selects.
    module_pins() {
        awk '$1 == "require" && $2 == "(" { block = 1; next }
             block && $1 == ")" { block = 0; next }
             block && NF >= 2 { print $1, $2; next }
             $1 == "require" && NF >= 3 { print $2, $3 }' "$1"
    }
    module_pins "$ROOT/go.mod" | grep -qx 'github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper v0.0.0' ||
        fail 'rust-dev: the root go.mod must require the helper module at v0.0.0'
    grep -Eq '^replace github\.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper => \./examples/layers/rust-dev/helper$' "$ROOT/go.mod" ||
        fail 'rust-dev: the root go.mod must replace the helper module with ./examples/layers/rust-dev/helper'
    shared_mismatch=$(awk 'NR == FNR { rootpins[$1] = $2; next }
                           $1 in rootpins && rootpins[$1] != $2 {
                               print "  " $1 ": helper " $2 ", root " rootpins[$1] }' \
        <(module_pins "$ROOT/go.mod") <(module_pins "$helperdir/go.mod"))
    [ -z "$shared_mismatch" ] ||
        fail "rust-dev: the two go.mod files require different versions of a shared module:"$'\n'"$shared_mismatch"
    [ -f "$ROOT/images/parent-pins.json" ] || fail 'rust-dev: missing images/parent-pins.json'
    builder_ref=$(jq -er '.pins["golang-builder"].reference' "$ROOT/images/parent-pins.json") ||
        fail 'rust-dev: images/parent-pins.json carries no golang-builder pin'
    case "$builder_ref" in
        "golang:${root_toolchain#go}-trixie@sha256:"*) ;;
        *)
            fail "rust-dev: the golang-builder pin ($builder_ref) must be golang:${root_toolchain#go}-trixie at the digest of the reviewed image"
            ;;
    esac
    first_from=$(printf '%s\n' "$instructions" |
        awk 'toupper($0) ~ /^[[:space:]]*FROM([[:space:]]|$)/ { print; exit }')
    last_from=$(printf '%s\n' "$instructions" |
        awk 'toupper($0) ~ /^[[:space:]]*FROM([[:space:]]|$)/ { line = $0 } END { print line }')
    reviewed_builder="FROM $builder_ref AS rust-dev-helper"
    [ "$(normalize_instruction "$last_from")" = 'FROM ${BASE_IMAGE}' ] ||
        fail "rust-dev: the helper stage must precede the parent: the last FROM must be FROM \${BASE_IMAGE} (got: $(normalize_instruction "$last_from"))"
    [ "$(normalize_instruction "$first_from")" = "$reviewed_builder" ] ||
        fail "rust-dev: the first FROM must be exactly: $reviewed_builder (got: $(normalize_instruction "$first_from"))"
    builder_line=$(printf '%s\n' "$instructions" |
        awk 'toupper($0) ~ /^[[:space:]]*RUN([[:space:]]|$)/ && /rust-dev-helper/ { print; exit }')
    for token in '--network=none' 'source=helper,target=/src,ro' 'GOTOOLCHAIN=local go env GOVERSION' \
        'GOFLAGS=-mod=vendor' 'GOPROXY=off' 'CGO_ENABLED=0' '-trimpath' '-buildvcs=false' \
        'go build'; do
        case "$builder_line" in
            *"$token"*) ;;
            *) fail "rust-dev: the helper stage lacks the reviewed compile token: $token" ;;
        esac
    done
    # A tag would compile the fixture seams in, and a second build flags assignment
    # after the reviewed one would let a later line win over this text.
    if printf '%s\n' "$builder_line" | grep -qE '(^|[[:space:]])-tags([[:space:]=]|$)'; then
        fail 'rust-dev: the helper stage must not pass -tags: the shipped binary honours no fixture seam'
    fi
    # --- the stage allowlist, which is what the guarantees above rest on ------
    # The stage may hold exactly two instructions: the pinned FROM and this RUN.
    # Every other instruction class reaches the compile without changing the line
    # compared above, and two of them were proven against the real compiler:
    #
    #   * `SHELL ["/bin/sh","-c","eval \"$(printf %s \"$0\" | sed
    #     's/go build /go build -tags=example_fixture /')\""]` keeps the RUN
    #     byte-identical and compiles the fixture seams in - the binary then
    #     answers `--fixture-seams` with all three names;
    #   * `ADD https://example.com/ /out/fetched.html` downloads inside a stage
    #     whose RUN has `--network=none`, so "the only egress is the FROM pull"
    #     is only true while the instruction set is closed.
    #
    # `ARG GOFLAGS` and `ENV GOFLAGS=-tags=...` lose to the RUN's own explicit
    # assignment today, but a gate that permits them is enforcing that this RUN
    # happens to be written defensively rather than a compile-time boundary. So
    # the classes are refused, not argued about: no SHELL, ADD, COPY, ENV, ARG,
    # WORKDIR, USER, ONBUILD or VOLUME, and no second FROM (the per-example FROM
    # shape check above refuses a third, whichever stage it declares).
    builder_stage=$(printf '%s\n' "$instructions" | awk -v stage=rust-dev-helper '
        toupper($0) ~ /^[[:space:]]*FROM([[:space:]]|$)/ { inside = ($NF == stage); next }
        inside { print }')
    stage_keywords=$(printf '%s\n' "$builder_stage" | awk 'NF { print toupper($1) }' |
        LC_ALL=C sort -u | tr '\n' ' ')
    [ "${stage_keywords% }" = RUN ] ||
        fail "rust-dev: the helper stage may not carry these instructions: ${stage_keywords% } (only its reviewed RUN is allowed)"
    builder_body=$(printf '%s\n' "$builder_stage" | grep -c . || true)
    [ "$builder_body" -eq 1 ] ||
        fail "rust-dev: the helper stage must carry exactly one RUN (found $builder_body)"
    read -r -d '' reviewed_helper_builder <<'HELPER_BUILDER' || true
RUN --network=none --mount=type=bind,source=helper,target=/src,ro cd /src && { [ "$(GOTOOLCHAIN=local go env GOVERSION)" = "$(awk '$1 == "toolchain" { print $2 }' go.mod)" ] || { echo "rust-dev-helper: the image compiler is not go.mod's toolchain" >&2; exit 1; }; } && GOTOOLCHAIN=local GOWORK=off GOENV=off GOFLAGS=-mod=vendor GOPROXY=off CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/rust-dev-helper .
HELPER_BUILDER
    [ "$(normalize_instruction "$builder_stage")" = "$reviewed_helper_builder" ] ||
        fail "rust-dev: the helper stage RUN must be exactly: $reviewed_helper_builder (got: $(normalize_instruction "$builder_stage"))"
    # One reader. `check` moved to the layer's own Go helper in #187 slice 2 and
    # the Python oracle is deleted (#245), so this gate holds the Go answer: a
    # refusal is reported with its own text, and the channel assertion must match
    # the Dockerfile's ARG. Nothing compares two implementations any more.
    host_os=$(uname -s | tr '[:upper:]' '[:lower:]')
    go_helper=$(bash "$HELPER_BUILD" rust-dev --root "$ROOT" \
        --os "$host_os") || fail 'rust-dev pins: cannot build the host helper'
    go_out='' go_status=0
    go_out=$("$go_helper" check "$ROOT/$datadir" --expect-channel "$rust_arg" 2>&1) || go_status=$?
    [ "$go_status" -eq 0 ] || fail "rust-dev pins: $go_out"
    [ "$go_out" = "$rust_arg" ] || fail "rust-dev pins: channel disagreement"
    selected=$go_out
reviewed_installer='RUN --mount=type=bind,source=install-rust.sh,target=/tmp/install-rust.sh,ro --mount=type=bind,from=rust-dev-helper,source=/out/rust-dev-helper,target=/tmp/rust-dev-helper,ro --mount=type=bind,source=data/rust-toolchain,target=/tmp/rust-toolchain,ro bash /tmp/install-rust.sh'
reviewed_verifier='RUN --network=none --mount=type=bind,source=verify-toolchain.sh,target=/run/rust-verify/verify-toolchain.sh,ro --mount=type=bind,from=rust-dev-helper,source=/out/rust-dev-helper,target=/run/rust-verify/rust-dev-helper,ro --mount=type=bind,source=data/rust-toolchain,target=/run/rust-verify/rust-toolchain,ro RUST_TOOLCHAIN=$RUST_TOOLCHAIN KACHE_VERSION=$KACHE_VERSION VERUS_DIR=/opt/verus /bin/bash /run/rust-verify/verify-toolchain.sh'
installer_runs=$(printf '%s\n' "$instructions" | grep -cF 'install-rust.sh' || true)
    [ "$installer_runs" -eq 1 ] || fail 'rust-dev: installer RUN must appear exactly once'
    installer_line=$(printf '%s\n' "$instructions" | grep -F 'install-rust.sh')
    for token in 'source=install-rust.sh' \
        'from=rust-dev-helper,source=/out/rust-dev-helper,target=/tmp/rust-dev-helper,ro' \
        'source=data/rust-toolchain,target=/tmp/rust-toolchain,ro'; do
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
        'from=rust-dev-helper,source=/out/rust-dev-helper,target=/run/rust-verify/rust-dev-helper,ro' \
        'data/rust-toolchain,target=/run/rust-verify/rust-toolchain,ro' \
        'RUST_TOOLCHAIN=$RUST_TOOLCHAIN' 'KACHE_VERSION=$KACHE_VERSION' 'VERUS_DIR=/opt/verus'; do
        case "$verifier_line" in
            *"$token"*) ;;
            *) fail "rust-dev: verifier RUN lacks the reviewed delivery: $token" ;;
        esac
    done
    [ "$(normalize_instruction "$verifier_line")" = "$reviewed_verifier" ] ||
        fail "rust-dev: verifier RUN must be exactly: $reviewed_verifier (got: $(normalize_instruction "$verifier_line"))"
    # The verifier script's own two halves: the Go verb, and no Python at all.
    grep -qF '"$VERIFY_DIR/rust-dev-helper" verify-installed "$VERIFY_DIR/rust-toolchain"' \
        "$rustdir/verify-toolchain.sh" ||
        fail 'rust-dev: verify-toolchain.sh must run the Go verifier'
    if grep -qE 'python3|PYTHONDONTWRITEBYTECODE' "$rustdir/verify-toolchain.sh"; then
        fail 'rust-dev: the verifier step must run no Python'
    fi
    # The install leg is Go too (#231): python3 leaves this layer's apt line (the
    # parent still ships /usr/bin/python3 as a user feature), the entry point runs
    # the helper, and no Python name survives in it.
    apt_line=$(printf '%s\n' "$instructions" | grep -F 'apt-get install')
    case "$apt_line" in
        *python3*) fail 'rust-dev: apt install line must not carry python3' ;;
    esac
    for token in 'gnupg' 'ca-certificates'; do
        case "$apt_line" in
            *"$token"*) ;;
            *) fail "rust-dev: apt install line must keep $token" ;;
        esac
    done
    grep -qF '"$HERE/rust-dev-helper" install "$HERE/rust-toolchain"' \
        "$rustdir/install-rust.sh" ||
        fail 'rust-dev: install-rust.sh must run rust-dev-helper install'
    if grep -qE 'python3|PYTHONDONTWRITEBYTECODE' "$rustdir/install-rust.sh"; then
        fail 'rust-dev: the install step must run no Python'
    fi
    grep -qxF 'ENV RUSTUP_AUTO_INSTALL=0' "$rustdir/Dockerfile" ||
        fail 'rust-dev: missing the no-implicit-install contract (ENV RUSTUP_AUTO_INSTALL=0)'
    grep -qxF 'ENV RUSTUP_HOME=/opt/rustup' "$rustdir/Dockerfile" ||
        fail 'rust-dev: RUSTUP_HOME ENV'
    grep -qxE 'ENV PATH=/opt/kache/shims:/opt/kache/bin:/opt/cargo/bin:/opt/verus/verus-x86-linux:\$\{PATH\}' "$rustdir/Dockerfile" ||
        fail 'rust-dev: PATH must stay additive and lead with the kache shims'
    grep -qxF 'ENV RUSTC_WRAPPER=/opt/kache/bin/kache' "$rustdir/Dockerfile" ||
        fail 'rust-dev: the guest cargo wrapper must be the pinned kache'
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
    for token in 'doctor --probe --json' 'kache-engine' \
        '"${RUSTC_WRAPPER:-}" = "$KACHE_DIR/bin/kache"' '.kache-shims' 'cc -o'; do
        grep -qF -- "$token" "$verifier" || fail "rust-dev: kache smoke lacks: $token"
    done
    # --- rust-dev: the pinned kache compiler cache ---------------------------
    # A third-party release artifact: the tarball digest, the published sidecar
    # and the release key are committed pins, and install-kache.sh verifies the
    # detached signature over the tarball before anything is extracted. The
    # committed evidence and the Dockerfile ARGs are checked against each other
    # here so a re-pin cannot leave one of them behind.
    kachedir=$rustdir/data/kache
    installer=$rustdir/install-kache.sh
    checksum_token "$installer" || fail 'rust-dev: install-kache.sh lacks non-comment sha256sum token'
    awk '!/^[[:space:]]*#/ && /sha256sum -c/ { found=1 } END { exit !found }' "$installer" ||
        fail 'rust-dev: install-kache.sh must verify the tarball with sha256sum -c'
    if grep -nE 'releases/latest|/latest/|(^|[^[:alnum:]_])(curl|wget)[^|]*\|[[:space:]]*(ba)?sh' "$installer"; then
        fail 'rust-dev: the kache installer must not resolve a mutable tag or pipe a download'
    fi
    kache_version_arg=$(sed -nE 's/^ARG KACHE_VERSION=(.*)$/\1/p' "$rustdir/Dockerfile")
    [ "$(printf '%s\n' "$kache_version_arg" | grep -c .)" -eq 1 ] || fail 'rust-dev pins: ARG KACHE_VERSION'
    for pair in amd64:x86_64 arm64:aarch64; do
        kache_arch=${pair%%:*}
        kache_host=${pair##*:}
        kache_asset=kache-${kache_host}-unknown-linux-musl.tar.gz
        kache_sha=$(sed -nE "s/^ARG KACHE_${kache_arch^^}_SHA256=(.*)\$/\1/p" "$rustdir/Dockerfile")
        [ -f "$kachedir/$kache_asset.sha256" ] ||
            fail "rust-dev: missing kache evidence: data/kache/$kache_asset.sha256"
        [ "$(cat "$kachedir/$kache_asset.sha256")" = "$kache_sha  $kache_asset" ] ||
            fail "rust-dev: data/kache/$kache_asset.sha256 does not record the KACHE_${kache_arch^^}_SHA256 pin"
    done
    [ -f "$kachedir/release-key.asc" ] || fail 'rust-dev: missing kache/release-key.asc'
    [ "$(sha256sum "$kachedir/release-key.asc" | cut -d' ' -f1)" = \
        "$(sed -nE 's/^ARG KACHE_RELEASE_CERT_SHA256=(.*)$/\1/p' "$rustdir/Dockerfile")" ] ||
        fail 'rust-dev: data/kache/release-key.asc does not hash to the KACHE_RELEASE_CERT_SHA256 pin'
    grep -qxF -- '-----BEGIN PGP PUBLIC KEY BLOCK-----' "$kachedir/release-key.asc" ||
        fail 'rust-dev: data/kache/release-key.asc is not an armored public key'
    reviewed_kache_installer='RUN --mount=type=bind,source=install-kache.sh,target=/tmp/install-kache.sh,ro --mount=type=bind,source=data/kache,target=/tmp/kache-pin,ro KACHE_VERSION=$KACHE_VERSION KACHE_AMD64_SHA256=$KACHE_AMD64_SHA256 KACHE_ARM64_SHA256=$KACHE_ARM64_SHA256 KACHE_RELEASE_CERT_SHA256=$KACHE_RELEASE_CERT_SHA256 KACHE_DIR=$KACHE_DIR KACHE_DATA=/tmp/kache-pin KACHE_SHIM_DIR=$KACHE_DIR/shims bash /tmp/install-kache.sh'
    kache_installer_runs=$(printf '%s\n' "$instructions" | grep -cF 'install-kache.sh' || true)
    [ "$kache_installer_runs" -eq 1 ] || fail 'rust-dev: the kache installer RUN must appear exactly once'
    kache_installer_line=$(printf '%s\n' "$instructions" | grep -F 'install-kache.sh')
    [ "$(normalize_instruction "$kache_installer_line")" = "$reviewed_kache_installer" ] ||
        fail "rust-dev: the kache installer RUN must be exactly the reviewed invocation (got: $(normalize_instruction "$kache_installer_line"))"
    # --- rust-dev: the C/C++ shims, which are image state ---------------------
    # They are not a guest setup step (ADR 0015): the installer creates them from
    # the pinned binary (and asserts the marker-and-six-links shape itself), the
    # image PATH leads with their directory, and the offline verifier requires
    # `kache doctor` to report them active. A farm that stops being installed, a
    # PATH that stops leading with it, and a doctor finding that quietly becomes
    # optional again all fail here instead of in a guest.
    for token in 'init --shims' '--canonical-only' '.kache-shims' 'KACHE_SHIM_DIR'; do
        grep -qF -- "$token" "$installer" || fail "rust-dev: install-kache.sh lacks the shim farm: $token"
    done
    grep -qF -- 'KACHE_SHIM_DIR=$KACHE_DIR/shims' "$rustdir/Dockerfile" ||
        fail 'rust-dev: the kache installer RUN must create the shim farm'
    # No presence guard: a missing probe is already the sweep's message, checked
    # before any layer assertion runs.
    # The probe is the layer's own binary now; the requirement is the same string
    # in the same place, and #156 owns turning it into a behaviour test.
    grep -qF -- 'C/C++ shims' "$rustdir/helper/kacheprobe/probe.go" ||
        fail 'rust-dev: the kache-probe must require the C/C++ shims to be active'
    # --- rust-dev: the cache layout and the shared remote, which are
    # --- configuration state -------------------------------------------------
    # The policy is an image input like the pins: the reviewed RUN that installs
    # it, the two ENVs that select it, its five effective values, and its
    # position after the verifier are contracts. The store's location is the
    # value that corrupts something if it drifts: /var/tmp is the sandbox's own
    # disk, while $HOME is a host-backed virtio-fs mount whose WAL index has
    # already corrupted once, so a "tidy-up" back to the default is a data bug,
    # not a preference. The ordering is the subtle one: the verifier decides a
    # `doctor --probe` report taken with no remote configured, and a configured
    # remote makes the daemon checks mandatory, so installing the policy earlier
    # would change what the verifier is able to pass on.
    kache_config=$rustdir/data/kache-config.toml
    [ -f "$kache_config" ] || fail 'rust-dev: missing data/kache-config.toml'
    for token in 'local_store = "/var/tmp/kache"' 'local_max_size = "4G"' \
        'gc_evict_shared = false' 'type = "filesystem"' 'path = "/mnt/kache-remote"'; do
        grep -qxF -- "$token" "$kache_config" ||
            fail "rust-dev: data/kache-config.toml must keep: $token"
    done
    if grep -v '^[[:space:]]*#' "$kache_config" | grep -qF -- 'atomic_write_dir'; then
        fail 'rust-dev: kache-config.toml must not stage uploads across filesystems'
    fi
    for env in 'ENV KACHE_CONFIG=/opt/kache/config.toml' 'ENV KACHE_CACHE_DIR=/var/tmp/kache'; do
        grep -qxF "$env" "$rustdir/Dockerfile" ||
            fail "rust-dev: the kache policy must be selected with ${env#ENV }"
    done
    reviewed_kache_config='RUN --mount=type=bind,source=data/kache-config.toml,target=/tmp/kache-config.toml,ro install -D -m 0644 /tmp/kache-config.toml /opt/kache/config.toml && mkdir -p /mnt/kache-remote && kache_check=$(mktemp -d) && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml /opt/kache/bin/kache config get cache.local_store | grep -qxF /var/tmp/kache && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml /opt/kache/bin/kache config get cache.local_max_size | grep -qxF 4G && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml /opt/kache/bin/kache config get cache.gc_evict_shared | grep -qx false && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml /opt/kache/bin/kache config get cache.remote.type | grep -qx filesystem && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml /opt/kache/bin/kache config get cache.remote.path | grep -qxF /mnt/kache-remote && rm -rf "$kache_check"'
    kache_config_runs=$(printf '%s\n' "$instructions" | grep -cF 'source=data/kache-config.toml' || true)
    [ "$kache_config_runs" -eq 1 ] || fail 'rust-dev: the kache policy RUN must appear exactly once'
    kache_config_line=$(printf '%s\n' "$instructions" | grep -F 'source=data/kache-config.toml')
    [ "$(normalize_instruction "$kache_config_line")" = "$reviewed_kache_config" ] ||
        fail "rust-dev: the kache policy RUN must be exactly the reviewed invocation (got: $(normalize_instruction "$kache_config_line"))"
    verifier_at=$(printf '%s\n' "$instructions" | grep -nF '/run/rust-verify/verify-toolchain.sh' | cut -d: -f1)
    config_at=$(printf '%s\n' "$instructions" | grep -nF 'source=data/kache-config.toml' | cut -d: -f1)
    { [ -n "$verifier_at" ] && [ -n "$config_at" ] && [ "$verifier_at" -lt "$config_at" ]; } ||
        fail 'rust-dev: the kache policy must be installed after the verifier'
    for arg in GO_AMD64_SHA256 GO_ARM64_SHA256 GOLANGCI_LINT_AMD64_SHA256 GOLANGCI_LINT_ARM64_SHA256; do
        grep -qE "^ARG $arg=[0-9a-f]{64}$" "$LAYERS/go-dev/Dockerfile" || fail "go-dev: digest ARG shape: $arg"
    done
    for rel in go-dev/install-go.sh go-dev/install-golangci-lint.sh; do
        file="$LAYERS/$rel"
        [ -f "$file" ] || fail "guarded script missing: $rel"
        checksum_token "$file" || fail "go-dev: ${rel##*/} lacks non-comment sha256sum token"
    done
    # --- go-dev: the reviewed gopls closure (#103) ---------------------------
    # The committed data directory is the reviewed module selection (graph, sums,
    # notices and the record binding them); gopls_closure.py is the only program
    # that verifies or installs it. A whole missing directory is reported here;
    # the missing files inside it are the helper's own `missing input: ...`.
    godir=$LAYERS/go-dev
    helper=$godir/gopls_closure.py
    datadir=$godir/data/gopls-closure
    [ -d "$datadir" ] || fail 'go-dev: missing the gopls closure input directory'
    gopls_arg=$(sed -nE 's/^ARG GOPLS_VERSION=(.*)$/\1/p' "$godir/Dockerfile")
    go_arg=$(sed -nE 's/^ARG GO_VERSION=(.*)$/\1/p' "$godir/Dockerfile")
    [ "$(printf '%s\n' "$gopls_arg" | grep -c .)" -eq 1 ] || fail 'go-dev pins: ARG GOPLS_VERSION'
    [ "$(printf '%s\n' "$go_arg" | grep -c .)" -eq 1 ] || fail 'go-dev pins: ARG GO_VERSION'
    # The committed closure is checked offline here, and both Dockerfile pins are
    # assertions on it: a version bumped without a regenerated closure fails.
    selected=$(PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/$helper" check "$ROOT/$datadir" \
        --expect-gopls "$gopls_arg" --expect-go "$go_arg" 2>&1) || fail "go-dev pins: $selected"
    [ "$selected" = "$gopls_arg" ] || fail 'go-dev pins: the closure and ARG GOPLS_VERSION disagree'
    instructions=$(logical_instructions "$godir/Dockerfile" go-dev) || exit 1
    reviewed_gopls_installer='RUN --mount=type=bind,source=install-gopls.sh,target=/tmp/install-gopls.sh,ro --mount=type=bind,source=gopls_closure.py,target=/tmp/gopls_closure.py,ro --mount=type=bind,source=data/gopls-closure,target=/tmp/gopls-closure,ro bash /tmp/install-gopls.sh'
    reviewed_go_verifier='RUN --network=none --mount=type=bind,source=verify-toolchain.sh,target=/run/go-verify/verify-toolchain.sh,ro --mount=type=bind,source=gopls_closure.py,target=/run/go-verify/gopls_closure.py,ro --mount=type=bind,source=data/gopls-closure,target=/run/go-verify/gopls-closure,ro PYTHONDONTWRITEBYTECODE=1 /bin/bash /run/go-verify/verify-toolchain.sh'
    gopls_installer_runs=$(printf '%s\n' "$instructions" | grep -cF 'install-gopls.sh' || true)
    [ "$gopls_installer_runs" -eq 1 ] || fail 'go-dev: the gopls installer RUN must appear exactly once'
    gopls_installer_line=$(printf '%s\n' "$instructions" | grep -F 'install-gopls.sh')
    for token in 'source=install-gopls.sh,target=/tmp/install-gopls.sh,ro' \
        'source=gopls_closure.py,target=/tmp/gopls_closure.py,ro' \
        'source=data/gopls-closure,target=/tmp/gopls-closure,ro'; do
        case "$gopls_installer_line" in
            *"$token"*) ;;
            *) fail "go-dev: installer RUN lacks the reviewed runtime bind: $token" ;;
        esac
    done
    # Exact shape, not a substring: a disabled or extended command (`: # ...`,
    # `|| true`, an extra argv) is not the reviewed install.
    [ "$(normalize_instruction "$gopls_installer_line")" = "$reviewed_gopls_installer" ] ||
        fail "go-dev: installer RUN must be exactly: $reviewed_gopls_installer (got: $(normalize_instruction "$gopls_installer_line"))"
    gopls_verifier_runs=$(printf '%s\n' "$instructions" | grep -cF 'verify-toolchain.sh' || true)
    [ "$gopls_verifier_runs" -eq 1 ] || fail 'go-dev: the verifier RUN must appear exactly once'
    gopls_verifier_line=$(printf '%s\n' "$instructions" | grep -F 'verify-toolchain.sh')
    case "$gopls_verifier_line" in
        *--network=none*) ;;
        *) fail 'go-dev: verifier must run with --network=none' ;;
    esac
    for token in 'verify-toolchain.sh,target=/run/go-verify/verify-toolchain.sh,ro' \
        'gopls_closure.py,target=/run/go-verify/gopls_closure.py,ro' \
        'data/gopls-closure,target=/run/go-verify/gopls-closure,ro' 'PYTHONDONTWRITEBYTECODE=1'; do
        case "$gopls_verifier_line" in
            *"$token"*) ;;
            *) fail "go-dev: verifier RUN lacks the reviewed delivery: $token" ;;
        esac
    done
    [ "$(normalize_instruction "$gopls_verifier_line")" = "$reviewed_go_verifier" ] ||
        fail "go-dev: verifier RUN must be exactly: $reviewed_go_verifier (got: $(normalize_instruction "$gopls_verifier_line"))"
    [ "$(grep -cxF 'ENV GOTOOLCHAIN=local' "$godir/Dockerfile")" -eq 1 ] ||
        fail 'go-dev: GOTOOLCHAIN=local must be exported exactly once'
    if grep -qE '^ENV .*(GOFLAGS|GOPROXY|GOSUMDB|GONOSUMDB|GOPRIVATE|GONOPROXY|GOINSECURE|GOTOOLCHAIN=(auto|go|path))' "$godir/Dockerfile"; then
        fail 'go-dev: no GO* policy may be exported besides GOTOOLCHAIN=local (GOTOOLCHAIN)'
    fi
    # No ordinary build may resolve a version, regenerate the graph or reach a
    # checksum-database fallback; comments are documentation, so only code lines
    # are matched. The maintenance writer is host-only.
    for file in "$godir/Dockerfile" "$godir/install-gopls.sh" "$godir/verify-toolchain.sh"; do
        if grep -vE '^[[:space:]]*#' "$file" |
            grep -qE 'go[[:space:]]+(install|get)([[:space:]]|$)|go[[:space:]]+mod[[:space:]]+(tidy|download|vendor|edit)|@latest|gopls@|-mod=mod|GONOSUMDB|GOSUMDB=off|GOINSECURE|,direct|GOPROXY=direct|GOTOOLCHAIN=auto'; then
            fail "go-dev: unpinned go install, graph regeneration or checksum bypass in ${file##*/}"
        fi
        if grep -vE '^[[:space:]]*#' "$file" | grep -qE 'upgrade-gopls|gopls-closure-update'; then
            fail "go-dev: maintenance writer must not run in an ordinary build: ${file##*/}"
        fi
    done
    for file in images/Dockerfile images/standard/Dockerfile; do
        [ -f "$file" ] || fail "recipe text missing: $file"
        if grep -qiE 'chromium|google-chrome|agent-vm-chrome-mcp|agent-vm-capabilities' "$file"; then
            fail "recipe text contains Chrome token: $file"
        fi
    done
    # The data list is the backstop, so it is read after the layer assertions that
    # read those files: a listed file that is gone is this comment's message, and
    # a file under data/ that nobody listed is an unreviewed input.
    for rel in $GUARDED_DATA; do
        [ -f "$LAYERS/$rel" ] || fail "guarded script missing: $rel"
    done
    # Every regular file under a layer is classified here, and only these four
    # classes exist: a shebang script in GUARDED, any other input under the
    # layer's data/ in GUARDED_DATA, the layer's own Go module under helper/ in
    # GUARDED_GO, and the layer's own Dockerfile and README.md.
    # That makes coverage exhaustive without parsing mounts: a bind source is a
    # file in the layer, so a mounted file listed nowhere is caught as an
    # unclassified file, and a stray file at a layer root fails by name.
    while IFS= read -r -d '' file; do
        rel=${file#"$LAYERS/"}
        case "$rel" in
            README.md) continue ;; # this directory's own README
            */*) ;;
            *) fail "unclassified example file: $rel" ;;
        esac
        rest=${rel#*/}
        case "$rest" in
            Dockerfile | README.md) continue ;;
        esac
        # A layer's own Go module (ADR 0017): source, the module files, the vendor
        # tree and testdata live here and nothing else. Vendor bytes are the drift
        # gate's to authenticate, so they are the one class this list skips.
        case "$rest" in
            helper | helper/*)
                # The vendor tree first, including any file a dependency ships
                # with a shebang: those bytes are not this repository's and are
                # not shipped, and they are authenticated by the module's vendor
                # drift gate against the pinned go.sum rather than by this list.
                case "$rest" in
                    helper/vendor/*) continue ;;
                esac
                if [ "$(head -c 2 "$file")" = '#!' ]; then
                    fail "unguarded example script: $rel"
                fi
                case "$rest" in
                    helper/go.mod | helper/go.sum) ;;
                    helper/*.go | helper/*/*) ;;
                    *) fail "unclassified example file: $rel" ;;
                esac
                case " $GUARDED_GO " in
                    *" $rel "*) continue ;;
                    *) fail "unreviewed helper file: $rel" ;;
                esac
                ;;
        esac
        if [ "$(head -c 2 "$file")" = '#!' ]; then
            case " $GUARDED " in *" $rel "*) continue ;; esac
            fail "unguarded example script: $rel"
        fi
        case "$rest" in
            data/*) ;;
            *) fail "unclassified example file: $rel" ;;
        esac
        case " $GUARDED_DATA " in *" $rel "*) ;; *) fail "unreviewed layer input: $rel" ;; esac
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
# Insert one instruction immediately after the helper stage's FROM. The cases below
# use it to show which instruction classes the stage allowlist refuses: every one
# of them can change what the reviewed RUN compiles without changing that RUN.
insert_builder_instruction() { # FILE TEXT
    awk -v text="$2" '!placed && /^FROM golang:.* AS rust-dev-helper$/ {
            print; print text; placed = 1; next
        } { print }' "$1" > "$SCRATCH/rewrite"
    cat "$SCRATCH/rewrite" > "$1"
}
# Rewrites preserve copied file modes on both BSD and GNU hosts.
rewrite() {
    local file=$1 expression=$2
    sed "$expression" "$file" > "$SCRATCH/rewrite"
    cat "$SCRATCH/rewrite" > "$file"
}
# Re-pin a copied tree's inputs: the byte pins are what a mutation must trip.
repin() {
    ( cd "$1/$LAYERS/chrome-devtools/data/chrome-devtools-mcp" &&
        sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml > pnpm-inputs.sha256 )
}
# Re-pin the copied gopls closure for a *test-only* tree: recompute `files` for
# the four evidence files and follow the two scope digests. `modules[].h1` is
# deliberately left alone -- the mutations that change it are testing exactly the
# pin/go.sum disagreement that an automatic repair would hide -- and so are the
# reviewer/date, which no test may invent.
repin_gopls() { # TREE
    local dir=$1/$LAYERS/go-dev/data/gopls-closure digests=$SCRATCH/gopls-digests
    ( cd "$dir" && sha256sum go.mod go.sum gopls.go.mod THIRD_PARTY_LICENSES ) > "$digests"
    jq --rawfile digests "$digests" '
        ($digests | split("\n") | map(select(length > 0))
            | map({key: (split("  ")[1]), value: (split("  ")[0])}) | from_entries) as $files
        | .files = $files
        | .authentication.scope["go.mod"] = $files["go.mod"]
        | .authentication.scope["go.sum"] = $files["go.sum"]
    ' "$dir/pin.json" > "$SCRATCH/rewrite"
    cat "$SCRATCH/rewrite" > "$dir/pin.json"
}
# The case labels are an explicit source-format contract, not a Bash parse: one
# decimal label per line inside the marked table body, forming the complete
# sequence 1..COUNT with no duplicates. The sort is numeric and does not dedupe,
# so a repeated label cannot hide behind its first occurrence.
check_self_test_table() { # FILE COUNT SCRATCH
    local file=$1 count=$2 scratch=$3 duplicates
    mkdir -p "$scratch"
    awk '/^[[:space:]]*# SELF_TEST_TABLE_BEGIN$/ { inside=1; next }
         /^[[:space:]]*# SELF_TEST_TABLE_END$/ { inside=0 }
         inside && /^[[:space:]]*[0-9]+[)]/ {
             label=$0; sub(/^[[:space:]]*/, "", label); sub(/[)].*$/, "", label); print label
         }' "$file" | LC_ALL=C sort -n > "$scratch/actual-labels"
    seq 1 "$count" > "$scratch/expected-labels"
    duplicates=$(uniq -d "$scratch/actual-labels")
    [ -z "$duplicates" ] || fail 'self-test case labels contain duplicates'
    cmp -s "$scratch/expected-labels" "$scratch/actual-labels" ||
        fail 'self-test case labels must equal 1..SELF_TEST_CASES'
}
# The label guard is only worth its runtime while it still fails: three scratch
# copies of this file are mutated and must be refused from the label check alone,
# before any case body runs. The appended label keeps the declared bound, so only
# the sequence comparison can catch it.
self_test_label_controls() { # SCRATCH
    local scratch=$1 extra=$((SELF_TEST_CASES + 1)) copy out rc
    local sequence='self-test case labels must equal 1..SELF_TEST_CASES'

    copy=$scratch/label-appended.sh
    awk -v extra="$extra" '!placed && /^[[:space:]]*# SELF_TEST_TABLE_END$/ {
            printf "            %s) : ;;\n", extra; placed = 1
        } { print }' "$CHECKER" > "$copy"
    [ "$(grep -cE '^[[:space:]]*[0-9]+[)]' "$copy")" -eq "$extra" ] ||
        fail 'self-test: the appended-label control did not apply'
    out=$scratch/label-appended.log
    rc=0
    bash "$copy" --self-test > "$out" 2>&1 || rc=$?
    [ "$rc" -eq 1 ] || fail 'self-test: an appended case label must be refused'
    grep -qF "$sequence" "$out" ||
        fail 'self-test: the appended-case refusal is not the sequence check'
    if grep -q '^case ' "$out"; then
        fail 'self-test: the label guard must refuse before any case runs'
    fi
    echo 'self-test: ok   an appended case label is refused before any case runs'

    copy=$scratch/label-duplicate.sh
    awk '!placed && /^[[:space:]]*# SELF_TEST_TABLE_END$/ {
            print "            5) : ;;"; placed = 1
        } { print }' "$CHECKER" > "$copy"
    [ "$(grep -cE '^[[:space:]]*[0-9]+[)]' "$copy")" -eq "$extra" ] ||
        fail 'self-test: the duplicate-label control did not apply'
    out=$scratch/label-duplicate.log
    rc=0
    bash "$copy" --self-test > "$out" 2>&1 || rc=$?
    [ "$rc" -eq 1 ] || fail 'self-test: a duplicated case label must be refused'
    grep -qF 'self-test case labels contain duplicates' "$out" ||
        fail 'self-test: the duplicate-case refusal is not the duplicate check'
    echo 'self-test: ok   a duplicated case label is refused'

    copy=$scratch/label-missing.sh
    awk '$0 !~ /^[[:space:]]*50[)]/' "$CHECKER" > "$copy"
    [ "$(grep -cE '^[[:space:]]*[0-9]+[)]' "$copy")" -eq "$((SELF_TEST_CASES - 1))" ] ||
        fail 'self-test: the missing-label control did not apply'
    out=$scratch/label-missing.log
    rc=0
    bash "$copy" --self-test > "$out" 2>&1 || rc=$?
    [ "$rc" -eq 1 ] || fail 'self-test: a missing case label must be refused'
    grep -qF "$sequence" "$out" ||
        fail 'self-test: the missing-case refusal is not the sequence check'
    echo 'self-test: ok   a missing middle case label is refused'
}
self_test() {
    SCRATCH=$(mktemp -d)
    trap 'rm -rf "$SCRATCH"' EXIT
    local n tree chrome go rust wrapper expected rc telemetry file pnpmdir bundle lock
    local godir godocker goplsdir goplshelper rusttool rustdocker rustverify
    check_self_test_table "$CHECKER" "$SELF_TEST_CASES" "$SCRATCH"
    self_test_label_controls "$SCRATCH"
    for n in $(seq 1 "$SELF_TEST_CASES"); do
        tree="$SCRATCH/case-$n"
        mkdir -p "$tree/examples" "$tree/images/standard"
        cp -R "$ROOT/$LAYERS" "$tree/examples/"
        cp "$ROOT/go.mod" "$tree/go.mod"
        cp "$ROOT/images/Dockerfile" "$tree/images/"
        cp "$ROOT/images/parent-pins.json" "$tree/images/parent-pins.json"
        cp "$ROOT/images/standard/Dockerfile" "$tree/images/standard/"
        chrome="$tree/$LAYERS/chrome-devtools/Dockerfile"
        wrapper="$tree/$LAYERS/chrome-devtools/agent-vm-chrome-mcp"
        pnpmdir="$tree/$LAYERS/chrome-devtools/data/chrome-devtools-mcp"
        bundle="$tree/$LAYERS/chrome-devtools/data/chrome-devtools-mcp.sigstore.json"
        lock="$pnpmdir/pnpm-lock.yaml"
        go="$tree/$LAYERS/go-dev/Dockerfile"
        rust="$tree/$LAYERS/rust-dev/install-verus.sh"
        rusttool="$tree/$LAYERS/rust-dev/data/rust-toolchain"
        rustdocker="$tree/$LAYERS/rust-dev/Dockerfile"
        rustverify="$tree/$LAYERS/rust-dev/verify-toolchain.sh"
        godir="$tree/$LAYERS/go-dev"
        godocker="$godir/Dockerfile"
        goplsdir="$godir/data/gopls-closure"
        goplshelper="$godir/gopls_closure.py"
        expected=''
        # SELF_TEST_TABLE_BEGIN
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
                rewrite "$rustdocker" '/from=rust-dev-helper,source=\/out\/rust-dev-helper,target=\/tmp\/rust-dev-helper,ro/d'
                expected='installer RUN lacks the reviewed runtime bind: from=rust-dev-helper,source=/out/rust-dev-helper,target=/tmp/rust-dev-helper,ro' ;;
            65)
                rewrite "$rustdocker" '/source=data\/rust-toolchain,target=\/tmp\/rust-toolchain,ro/d'
                expected='installer RUN lacks the reviewed runtime bind: source=data/rust-toolchain' ;;
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
                # Selected by the verifier's own bind, not by position or by the
                # shape of its first line: a selector that could pick another RUN
                # makes the verdict depend on what else the file contains.
                awk -v bind='from=rust-dev-helper,source=/out/rust-dev-helper,target=/run/rust-verify/rust-dev-helper' '
                    { lines[NR] = $0 }
                    END {
                        for (i = 1; i <= NR; i++) {
                            if (lines[i] ~ /^RUN/) {
                                start = i; j = i; instruction = lines[i]
                                while (lines[j] ~ /\\$/ && j < NR) { j++; instruction = instruction "\n" lines[j] }
                                if (index(instruction, bind) && !stripped) {
                                    sub(/RUN --network=none/, "RUN", lines[start]); stripped = 1
                                }
                                for (k = start; k <= j; k++) print lines[k]
                                i = j
                            } else {
                                print lines[i]
                            }
                        }
                    }' "$rustdocker" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rustdocker"
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
            75) rm "$tree/$LAYERS/rust-dev/verify-toolchain.sh"
                expected='guarded script missing: rust-dev/verify-toolchain.sh' ;;
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
            # 82-103: the reviewed gopls closure and its recipe (#103). Cases
            # 83-91 and 94-98 mutate one thing in a copied tree and then re-pin,
            # so the rule under test is the one that fires rather than a stale
            # digest; the exceptions (83, 89, 92-93, 97-98) deliberately test a
            # digest, an assert or a recipe shape instead. 103 proves repin_gopls
            # itself is byte-faithful, so the re-pinned cases fail on content.
            82) rm "$goplsdir/pin.json"; expected='missing input: pin.json' ;;
            83) printf ' ' >> "$goplsdir/go.sum"
                expected='go.sum: bytes differ from the committed digest' ;;
            84)
                awk '$0 !~ /^golang\.org\/x\/tools\/gopls v0\.23\.0 h1:/' "$goplsdir/go.sum" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$goplsdir/go.sum"
                repin_gopls "$tree"
                expected='go.sum lacks h1 for golang.org/x/tools/gopls@v0.23.0' ;;
            85)
                printf '\nreplace github.com/fatih/camelcase => github.com/fatih/camelcase v1.0.0\n' \
                    >> "$goplsdir/go.mod"
                repin_gopls "$tree"
                expected='directive not allowed: replace' ;;
            86)
                awk '!placed && $0 ~ /^\tgithub\.com\/fsnotify\/fsnotify v1\.9\.0 / {
                        sub(/v1\.9\.0/, "v1.10.0"); placed = 1
                    } { print }' "$goplsdir/go.mod" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$goplsdir/go.mod"
                repin_gopls "$tree"
                expected="differs from the root module's requirement" ;;
            87)
                printf 'toolchain go1.99.0\n' >> "$goplsdir/go.mod"
                repin_gopls "$tree"
                expected='directive not allowed: toolchain' ;;
            88)
                rewrite "$goplsdir/go.mod" 's|^go 1\.26\.0$|go 1.99.0|'
                repin_gopls "$tree"
                expected='go directive' ;;
            89)
                jq '(.modules[] | select(.path == "github.com/fatih/camelcase") | .h1)
                        = (.modules[] | select(.path == "github.com/BurntSushi/toml") | .h1)' \
                    "$goplsdir/pin.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$goplsdir/pin.json"
                expected='h1 differs from go.sum' ;;
            90)
                printf ' ' >> "$goplsdir/gopls.go.mod"
                repin_gopls "$tree"
                expected='root go.mod evidence' ;;
            91)
                # The sections are length-framed, so the last one is dropped by
                # truncating at its header's byte offset rather than by lines.
                line=$(grep -n '^=== ' "$goplsdir/THIRD_PARTY_LICENSES" | tail -n 1 | cut -d: -f1)
                [ -n "$line" ] || fail 'self-test: case 91 could not find a license section'
                offset=$(head -n "$((line - 1))" "$goplsdir/THIRD_PARTY_LICENSES" | wc -c | tr -d ' ')
                head -c "$offset" "$goplsdir/THIRD_PARTY_LICENSES" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$goplsdir/THIRD_PARTY_LICENSES"
                repin_gopls "$tree"
                expected='license evidence' ;;
            92)
                rewrite "$godocker" 's|^ARG GOPLS_VERSION=.*|ARG GOPLS_VERSION=v0.0.0|'
                expected='asserted gopls version' ;;
            93)
                rewrite "$godocker" 's|^ARG GO_VERSION=.*|ARG GO_VERSION=1.27.4|'
                expected='pin generated with go' ;;
            94)
                rewrite "$godocker" '/source=gopls_closure.py,target=\/tmp\/gopls_closure.py,ro/d'
                expected='installer RUN lacks the reviewed runtime bind: source=gopls_closure.py' ;;
            95)
                rewrite "$godocker" 's|bash /tmp/install-gopls.sh|bash /tmp/install-gopls.sh \|\| true|'
                expected='installer RUN must be exactly' ;;
            96)
                rewrite "$godocker" 's|RUN --network=none|RUN|'
                expected='verifier must run with --network=none' ;;
            97)
                echo 'RUN go install golang.org/x/tools/gopls@latest' >> "$godocker"
                expected='unpinned go install, graph regeneration or checksum bypass in Dockerfile' ;;
            98)
                echo 'ENV GOTOOLCHAIN=auto' >> "$godocker"
                expected='GOTOOLCHAIN' ;;
            99) printf 'unexpected' > "$goplsdir/.extra"; expected='unreviewed input: .extra' ;;
            100)
                rm "$goplsdir/go.sum"
                ln -s go.mod "$goplsdir/go.sum"
                expected='symlink go.sum' ;;
            101) rm "$goplshelper"; expected='guarded script missing: go-dev/gopls_closure.py' ;;
            102)
                jq '.authentication.method = "assumed-signed"' "$goplsdir/pin.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$goplsdir/pin.json"
                expected='module-channel determination' ;;
            103) repin_gopls "$tree" ;;
            # 104-110: the pinned kache release, its committed evidence and the
            # wrapper every guest cargo invocation runs through.
            104) rewrite "$tree/$LAYERS/rust-dev/Dockerfile" '/^ENV RUSTC_WRAPPER=/d'
                expected='the guest cargo wrapper must be the pinned kache' ;;
            105) rewrite "$tree/$LAYERS/rust-dev/Dockerfile" '/^ARG KACHE_AMD64_SHA256=/s/.$//'
                expected='rust-dev pins: ^ARG KACHE_AMD64_SHA256' ;;
            106) printf ' ' >> "$tree/$LAYERS/rust-dev/data/kache/kache-x86_64-unknown-linux-musl.tar.gz.sha256"
                expected='does not record the KACHE_AMD64_SHA256 pin' ;;
            107) rewrite "$tree/$LAYERS/rust-dev/install-kache.sh" '/^[[:space:]]*[(]cd "[$]work" && echo /d'
                expected='must verify the tarball with sha256sum -c' ;;
            108)
                rewrite "$tree/$LAYERS/rust-dev/Dockerfile" 's|bash /tmp/install-kache.sh|: # bash /tmp/install-kache.sh|'
                expected='the kache installer RUN must be exactly' ;;
            109) cp "$rusttool/rust-release-key.asc" "$tree/$LAYERS/rust-dev/data/kache/release-key.asc"
                expected='does not hash to the KACHE_RELEASE_CERT_SHA256 pin' ;;
            110) rm "$tree/$LAYERS/rust-dev/helper/kacheprobe/probe.go"
                expected='guarded script missing: rust-dev/helper/kacheprobe/probe.go' ;;
            # 111-113: the C/C++ shims, which the image installs instead of
            # leaving to a guest's `kache init`.
            111) rewrite "$tree/$LAYERS/rust-dev/Dockerfile" 's|ENV PATH=/opt/kache/shims:|ENV PATH=|'
                expected='PATH must stay additive and lead with the kache shims' ;;
            112) rewrite "$tree/$LAYERS/rust-dev/install-kache.sh" 's|--canonical-only||'
                expected='install-kache.sh lacks the shim farm: --canonical-only' ;;
            113) rewrite "$tree/$LAYERS/rust-dev/helper/kacheprobe/probe.go" 's|C/C++ shims|shims|'
                expected='the kache-probe must require the C/C++ shims to be active' ;;
            # 114: the shared remote's policy is an input, not a preference.
            114) rewrite "$tree/$LAYERS/rust-dev/data/kache-config.toml" 's|"4G"|"20G"|'
                expected='kache-config.toml must keep: local_max_size = "4G"' ;;
            # 115-116: the store is on the sandbox's own disk. Its old home,
            # $HOME/.cache/kache, is a host-backed virtio-fs mount, and a WAL
            # index there has corrupted once; both the file and the ENV that
            # name the new path have to stay.
            115) rewrite "$tree/$LAYERS/rust-dev/data/kache-config.toml" '/^local_store = /d'
                expected='kache-config.toml must keep: local_store = "/var/tmp/kache"' ;;
            116) rewrite "$tree/$LAYERS/rust-dev/Dockerfile" '/^ENV KACHE_CACHE_DIR=/d'
                expected='must be selected with KACHE_CACHE_DIR=/var/tmp/kache' ;;
            # 117-119: the sweep's default is now guarded for every file. A
            # non-script input is covered because it is a file in the layer, so
            # these three are the failure directions the directory convention
            # buys: an unclassified file at a layer root, an unreviewed file
            # under data/, and a reviewed file that does not exist.
            117) printf 'a note\n' > "$tree/$LAYERS/rust-dev/notes.txt"
                expected='unclassified example file: rust-dev/notes.txt' ;;
            118) printf 'unreviewed\n' > "$tree/$LAYERS/rust-dev/data/notes.txt"
                expected='unreviewed layer input: rust-dev/data/notes.txt' ;;
            119) rm "$tree/$LAYERS/rust-dev/data/kache/README.md"
                expected='guarded script missing: rust-dev/data/kache/README.md' ;;
            # 120-132: the helper module and its compiler guard chain (#187). Each
            # case changes one thing, so the link under test is the one that fires:
            # the module's file classes, its two go.mod lines, the builder's tag and
            # digest, the stage order and count, the compile tokens, and the
            # verifier's mount of the binary instead of the Python probe.
            120) printf 'a note\n' > "$tree/$LAYERS/rust-dev/helper/notes.txt"
                expected='unclassified example file: rust-dev/helper/notes.txt' ;;
            121) printf '#!/bin/sh\n' > "$tree/$LAYERS/rust-dev/helper/run.sh"
                expected='unguarded example script: rust-dev/helper/run.sh' ;;
            122)
                rewrite "$tree/$LAYERS/rust-dev/helper/go.mod" \
                    's|^toolchain go1\.27\.3$|toolchain go1.27.4|'
                expected="helper/go.mod's toolchain (go1.27.4) must equal the root go.mod's (go1.27.3)" ;;
            123)
                rewrite "$tree/$LAYERS/rust-dev/helper/go.mod" 's|^go 1\.27\.0$|go 1.27.1|'
                expected="helper/go.mod's go line (1.27.1) must equal the root go.mod's (1.27.0)" ;;
            124)
                rewrite "$tree/$LAYERS/rust-dev/Dockerfile" \
                    's|^FROM golang:1\.27\.3-trixie@|FROM golang:1.27.2-trixie@|'
                expected='the first FROM must be exactly' ;;
            125)
                rewrite "$tree/$LAYERS/rust-dev/Dockerfile" \
                    's|^FROM golang:1\.27\.3-trixie@sha256:[0-9a-f]*|FROM golang:1.27.3-trixie|'
                expected='the first FROM must be exactly' ;;
            126)
                # The other direction of the same binding: the reviewed digest in
                # the inventory moves, and the recipe that names it must fail.
                awk '!placed && /"reference": "golang:/ {
                        sub(/sha256:[0-9a-f]{64}/, "sha256:'"$(printf '0%.0s' $(seq 64))"'"); placed = 1
                     } { print }' "$tree/images/parent-pins.json" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$tree/images/parent-pins.json"
                expected='the first FROM must be exactly' ;;
            127)
                printf 'FROM golang:1.27.3-trixie AS second-builder\n' \
                    >> "$tree/$LAYERS/rust-dev/Dockerfile"
                expected='must carry exactly the helper stage and the parent' ;;
            128)
                # The helper stage moved after the parent: the compiler is not the
                # first FROM any more, and the last FROM is not the image.
                awk '!/^FROM golang:/ && !/^RUN --network=none --mount=type=bind,source=helper/ &&
                     !/^    cd \/src/ && !/^      \|\|/ && !/^ &&/ && !/^    go build -trimpath/ { print }
                     END { print "FROM golang:1.27.3-trixie AS rust-dev-helper" }' \
                    "$tree/$LAYERS/rust-dev/Dockerfile" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$tree/$LAYERS/rust-dev/Dockerfile"
                expected='parent pin: FROM shape: rust-dev must end on FROM ${BASE_IMAGE}' ;;
            129)
                awk '!/^RUN --network=none --mount=type=bind,source=helper/ { print }
                     /^RUN --network=none --mount=type=bind,source=helper/ {
                        sub(/RUN --network=none/, "RUN"); print }' \
                    "$tree/$LAYERS/rust-dev/Dockerfile" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$tree/$LAYERS/rust-dev/Dockerfile"
                expected='the helper stage lacks the reviewed compile token: --network=none' ;;
            130)
                rewrite "$tree/$LAYERS/rust-dev/Dockerfile" \
                    's|-buildvcs=false -o /out/rust-dev-helper|-buildvcs=false -tags example_fixture -o /out/rust-dev-helper|'
                expected='the helper stage must not pass -tags' ;;
            131)
                rewrite "$tree/$LAYERS/rust-dev/Dockerfile" \
                    's|from=rust-dev-helper,source=/out/rust-dev-helper,target=/run/rust-verify/rust-dev-helper,ro|source=kache_probe.py,target=/run/rust-verify/kache_probe.py,ro|'
                expected='verifier RUN lacks the reviewed delivery' ;;
            132) rm "$tree/$LAYERS/rust-dev/helper/vendor/modules.txt"
                expected='helper/vendor is the build closure' ;;
            # 133-142: the helper stage is a closed instruction set. Each case adds
            # one instruction class the reviewed RUN cannot see, and every one of
            # them was possible before the allowlist: `SHELL` rewrites the command
            # while the RUN stays byte-identical (proven: the binary answered
            # `--fixture-seams` with all three names), `ADD` fetches over the
            # network in a stage whose RUN has `--network=none`, and the rest reach
            # the compiler through the environment or the filesystem.
            133)
                read -r -d '' mutation <<'INSTRUCTION' || true
SHELL ["/bin/sh", "-c", "eval \"$(printf '%s' \"$0\" | sed 's/go build /go build -tags=example_fixture /')\""]
INSTRUCTION
                insert_builder_instruction "$rustdocker" "$mutation"
                expected='may not carry these instructions: RUN SHELL' ;;
            134) insert_builder_instruction "$rustdocker" 'ADD https://example.com/ /out/fetched.html'
                expected='may not carry these instructions: ADD RUN' ;;
            135) insert_builder_instruction "$rustdocker" 'COPY go.mod /src/go.mod'
                expected='may not carry these instructions: COPY RUN' ;;
            136) insert_builder_instruction "$rustdocker" 'ENV GOFLAGS=-tags=example_fixture'
                expected='may not carry these instructions: ENV RUN' ;;
            137) insert_builder_instruction "$rustdocker" 'ARG GOFLAGS'
                expected='may not carry these instructions: ARG RUN' ;;
            138) insert_builder_instruction "$rustdocker" 'WORKDIR /src'
                expected='may not carry these instructions: RUN WORKDIR' ;;
            139) insert_builder_instruction "$rustdocker" 'USER nobody'
                expected='may not carry these instructions: RUN USER' ;;
            140) insert_builder_instruction "$rustdocker" 'ONBUILD RUN true'
                expected='may not carry these instructions: ONBUILD RUN' ;;
            141) insert_builder_instruction "$rustdocker" 'VOLUME /out'
                expected='may not carry these instructions: RUN VOLUME' ;;
            142) insert_builder_instruction "$rustdocker" \
                    'FROM golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS rust-dev-helper'
                expected='parent pin: FROM shape: rust-dev must carry exactly the helper stage and the parent' ;;
            # 143-145: parser directives and the global ARG set (#235). Each case is
            # the mutation that passed every gate before its rule existed: an
            # unreviewed frontend, an escape directive BuildKit ignores where the
            # gate honoured it, and a top-level ARG the recipe never named.
            143)
                { printf '# syntax=evil.example.com/frontend:latest\n'; cat "$rustdocker"; } > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$rustdocker"
                expected='parent pin: unsupported parser directive: syntax' ;;
            144)
                # Before the rule, `leading` survived the ordinary comment, so the
                # gate read the backtick continuations while BuildKit read `\`.
                { printf '# An ordinary comment\n# escape=`\n'; sed 's/\\$/`/' "$godocker"; } > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$godocker"
                expected='parent pin: parser directive outside the leading window: escape' ;;
            145)
                awk '/^ARG BASE_IMAGE=/ { print; print "ARG GOFLAGS=-tags=example_fixture"; next } { print }' \
                    "$godocker" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$godocker"
                expected='ARG review: go-dev declares an unreviewed global ARG: GOFLAGS' ;;
            # 146-150: the keyword is separated by any of Docker's five separators
            # and the reviewed sets are one committed data file (#237). 146, 147
            # and 149 are the lines this gate reads by field rather than by a
            # literal space, so a reading that stopped splitting on a tab or a
            # vertical tab would pass them to the parent checks (BSD awk does not
            # split fields on the vertical tab, so 149 is the case that needs the
            # normalization); 150 is the same vertical tab in the BASE_IMAGE
            # redefinition count, which a field-split read cannot see either; 148
            # is the reverse direction of the set, the stale permission the data
            # file's equality still refuses.
            146)
                awk '/^ARG BASE_IMAGE=/ { print; print "ARG\tGOFLAGS=-tags=example_fixture"; next } { print }' \
                    "$godocker" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$godocker"
                grep -qF "$(printf 'ARG\tGOFLAGS=-tags=example_fixture')" "$godocker" ||
                    fail 'self-test: case 146 did not write a tab-delimited ARG'
                expected='ARG review: go-dev declares an unreviewed global ARG: GOFLAGS' ;;
            147)
                awk '!placed && $0 == "FROM ${BASE_IMAGE}" {
                        print "FROM\t${BASE_IMAGE}"; placed = 1; next
                    } { print }' "$godocker" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$godocker"
                grep -qxF "$(printf 'FROM\t${BASE_IMAGE}')" "$godocker" ||
                    fail 'self-test: case 147 did not write a tab-delimited FROM'
                expected='parent pin: FROM shape: go-dev' ;;
            148)
                awk '$0 != "ARG GO_PREFIX=/opt/go"' "$godocker" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$godocker"
                if grep -qxF 'ARG GO_PREFIX=/opt/go' "$godocker"; then
                    fail 'self-test: case 148 did not drop ARG GO_PREFIX'
                fi
                expected='ARG review: go-dev no longer declares the reviewed ARG GO_PREFIX' ;;
            149)
                awk '/^ARG BASE_IMAGE=/ { print; print "ARG\vGOFLAGS=-tags=example_fixture"; next } { print }' \
                    "$godocker" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$godocker"
                grep -qF "$(printf 'ARG\vGOFLAGS=-tags=example_fixture')" "$godocker" ||
                    fail 'self-test: case 149 did not write a vertical-tab-delimited ARG'
                expected='ARG review: go-dev declares an unreviewed global ARG: GOFLAGS' ;;
            150)
                awk '/^ARG BASE_IMAGE=/ { print; print "ARG\vBASE_IMAGE=debian:13"; next } { print }' \
                    "$godocker" > "$SCRATCH/rewrite"
                cat "$SCRATCH/rewrite" > "$godocker"
                grep -qF "$(printf 'ARG\vBASE_IMAGE=debian:13')" "$godocker" ||
                    fail 'self-test: case 150 did not write a vertical-tab-delimited ARG'
                expected='parent pin: ARG shape: go-dev redefinition' ;;
            # 151-154: one reader (#245 deleted the Python oracle). 151 changes
            # the compiled trust root, so the reader must refuse the committed
            # data. 152 re-adds a reader file to the layer: it is not in GUARDED,
            # so the sweep names it. 153 restores a Python mount and 154 the
            # bytecode variable; both must fail the exact RUN shape.
            151)
                rewrite "$tree/$LAYERS/rust-dev/helper/toolchain/policy.go" \
                    's|108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE|108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FF|'
                expected='is not the reviewed primary key' ;;
            152)
                printf '#!/bin/sh\nexit 0\n' > "$tree/$LAYERS/rust-dev/legacy-reader.py"
                expected='unguarded example script: rust-dev/legacy-reader.py' ;;
            153)
                rewrite "$rustdocker" 's|--mount=type=bind,source=data/rust-toolchain,target=/run/rust-verify/rust-toolchain,ro|--mount=type=bind,source=legacy-reader.py,target=/run/rust-verify/legacy-reader.py,ro --mount=type=bind,source=data/rust-toolchain,target=/run/rust-verify/rust-toolchain,ro|'
                expected='verifier RUN must be exactly' ;;
            154)
                rewrite "$rustdocker" 's|^    /bin/bash /run/rust-verify/verify-toolchain.sh|    PYTHONDONTWRITEBYTECODE=1 /bin/bash /run/rust-verify/verify-toolchain.sh|'
                expected='verifier RUN must be exactly' ;;
            # 155-156: the vendored dependency boundary (#187 slice 2). 155 is the
            # lookalike: a directory whose name starts with `vendor` is not the
            # vendor tree, so its script is still a script the sweep must name --
            # the exclusion is the exact path `helper/vendor/`, not a prefix. 156 is
            # the excluded class itself: a `.go` file under `helper/vendor/` is
            # upstream's, so the layer sweep passes and the module's vendor drift
            # gate (`example-helper-vendor.sh`) is the check that authenticates it.
            155)
                mkdir -p "$tree/$LAYERS/rust-dev/helper/vendorx"
                printf '#!/bin/sh\ntrue\n' > "$tree/$LAYERS/rust-dev/helper/vendorx/run.sh"
                expected="unguarded example script: rust-dev/helper/vendorx/run.sh" ;;
            156)
                mkdir -p "$tree/$LAYERS/rust-dev/helper/vendor/example.test/pkg"
                printf 'package pkg\n' \
                    > "$tree/$LAYERS/rust-dev/helper/vendor/example.test/pkg/pkg.go"
                expected='' ;;
            # 157-161: the zero-Python install leg (#231). The apt line must drop
            # python3 without losing gpg, the entry point must run the Go verb and
            # name no Python, the installer must not mount the Python reader, and
            # a helper source file that is not in GUARDED_GO must fail.
            157)
                rewrite "$rustdocker" 's|        gnupg ca-certificates|        python3 gnupg ca-certificates|'
                expected='apt install line must not carry python3' ;;
            158)
                rewrite "$rustdocker" 's|        gnupg ca-certificates|        ca-certificates|'
                expected='apt install line must keep gnupg' ;;
            159)
                rewrite "$tree/$LAYERS/rust-dev/install-rust.sh" \
                    's|"\$HERE/rust-dev-helper" install|python3 "$HERE/legacy-reader.py" install|'
                expected='install-rust.sh must run rust-dev-helper install' ;;
            160)
                rewrite "$rustdocker" 's|--mount=type=bind,source=data/rust-toolchain,target=/tmp/rust-toolchain,ro|--mount=type=bind,source=legacy-reader.py,target=/tmp/legacy-reader.py,ro --mount=type=bind,source=data/rust-toolchain,target=/tmp/rust-toolchain,ro|'
                expected='installer RUN must be exactly' ;;
            161)
                printf 'package fetch\n' > "$tree/$LAYERS/rust-dev/helper/internal/fetch/unreviewed.go"
                expected='unreviewed helper file: rust-dev/helper/internal/fetch/unreviewed.go' ;;
            162)
                rewrite "$tree/go.mod" '/^replace github.com\/gregwebs\/agent-vm-images\/examples\/layers\/rust-dev\/helper/d'
                expected='must replace the helper module with' ;;
            163)
                printf '\nrequire (\n\tgithub.com/ulikunitz/xz v0.5.16\n)\n' >> "$tree/go.mod"
                expected='require different versions of a shared module' ;;
            # SELF_TEST_TABLE_END
            *) fail "case $n: not in the case table" ;;
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
    echo "example layers self-test passed ($SELF_TEST_CASES cases)"
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
