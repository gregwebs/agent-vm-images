#!/bin/sh
set -eu

# Headless browser bound, owned here because the browser runs in this container.
BROWSER_LIMIT_SECONDS=30

# Pinned preinstall (#41): the launcher's exact MCP entry, the prefix the wrapper
# maps it onto, and the recorders the runtime controls read.
MCP_VERSION=1.0.1
MCP_PREFIX=/opt/agent-vm/chrome-devtools-mcp
MCP_WRAPPER=/usr/local/bin/agent-vm-chrome-mcp
MCP_PROBE=/agent-vm-test/mcp-probe.mjs
HELPER=/agent-vm-test/chrome-devtools-mcp.sh
ARGV_OUT=/tmp/argv-out
MANAGER_CALLS=/tmp/agent-vm-manager-calls
ROW_ERR=/tmp/argv-row-err

# A pipeline such as `producer | grep -q PATTERN` reports grep's status under
# dash `set -eu`, so a producer that prints the expected text and then fails (or
# times out) still passes. Capture the producer's output, check its status, and
# only then match the completed output.
require_output() { # require_output PATTERN COMMAND [ARG...]
    pattern=$1
    shift
    output=$(mktemp)
    status=0
    "$@" >"$output" || status=$?
    if [ "$status" -ne 0 ]; then
        rm -f "$output"
        echo "checks.sh: producer failed with status $status" >&2
        return 1
    fi
    status=0
    grep -q "$pattern" "$output" || {
        status=$?
        echo "checks.sh: output did not match: $pattern" >&2
    }
    rm -f "$output"
    return "$status"
}

# Control for require_output: each mutant emits the expected text and then fails,
# which a bare pipeline would accept.
expect_rejection() { # expect_rejection LABEL PATTERN COMMAND [ARG...]
    label=$1
    shift
    if (require_output "$@"); then
        echo "checks.sh: self-test not rejected: $label" >&2
        return 1
    fi
    echo "self-test: rejected $label"
}

# --- runtime: R-series (#41) -----------------------------------------------

# Both recorders must be empty before a row: in the argv image an accepted row
# writes the argv and a refused row must write nothing at all.
reset_records() { rm -f "$ARGV_OUT" "$MANAGER_CALLS"; }

# R1-R3: first use through the wrapper must leave no package-manager state
# behind, in any home a later run could read. The installed server itself asks
# npm for its configured registry at startup (`npm config get registry`) - a
# query, not a resolve or fetch, which the armed image's sentinel rejects with
# exit 97 - so that one line is allowed and every other call is not.
MCP_MANAGER_QUERY='(^|/)npm config get registry$'

no_manager_state() {
    if [ -e "$MANAGER_CALLS" ]; then
        started=$(grep -Ev "$MCP_MANAGER_QUERY" "$MANAGER_CALLS" || true)
        if [ -n "$started" ]; then
            echo "checks.sh: a package manager was started: $(printf '%s' "$started" | tr '\n' ';')" >&2
            return 1
        fi
    fi
    for path in /root/.npm /home/chrome/.npm "$HOME/.npm" \
        "$HOME/.local/share/pnpm" "$HOME/.cache/pnpm"; do
        if [ -e "$path" ]; then
            echo "checks.sh: package-manager state left behind: $path" >&2
            return 1
        fi
    done
}

probe_fail() { # probe_fail MESSAGE
    cat "$probe_output" >&2
    rm -f "$probe_output"
    echo "checks.sh: $1" >&2
    exit 1
}

mcp_check() { # mcp_check wrapper|manager|cold
    probe_output=$(mktemp)
    probe_status=0
    case "$1" in
        wrapper)
            node "$MCP_PROBE" --expect-version "$MCP_VERSION" -- \
                "$MCP_WRAPPER" npx -y "chrome-devtools-mcp@$MCP_VERSION" --headless=true --isolated=true \
                >"$probe_output" 2>&1 || probe_status=$?
            [ "$probe_status" -eq 0 ] || probe_fail "MCP probe exit $probe_status"
            grep -q "^PROBE OK version=$MCP_VERSION tools=" "$probe_output" ||
                probe_fail 'MCP probe did not report the installed server'
            # Mirror the wrapper's choice of home: as root it prepares chrome's,
            # as a numeric user its own $HOME.
            if [ "$(id -u)" = 0 ]; then nss_home=/home/chrome; else nss_home=$HOME; fi
            test -f "$nss_home/.pki/nssdb/cert9.db" ||
                probe_fail "wrapper did not initialize the NSS DB in $nss_home/.pki/nssdb"
            no_manager_state || probe_fail 'the wrapper left package-manager state'
            # The probe's own report is the verdict's evidence in the harness log.
            grep '^PROBE OK version=' "$probe_output"
            ;;
        manager)
            # R6a: `sh -c` starts a manager indirectly, which the wrapper does
            # not claim to stop (the documented non-guarantee). The manager
            # sentinel must fire and the probe must see a server exit.
            node "$MCP_PROBE" --expect-version "$MCP_VERSION" -- \
                "$MCP_WRAPPER" sh -c 'exec npx -y chrome-devtools-mcp@1.0.1 "$@"' sh \
                --headless=true --isolated=true >"$probe_output" 2>&1 || probe_status=$?
            [ "$probe_status" -ne 0 ] || probe_fail 'the indirect manager path was not rejected'
            grep -q '^PROBE FAIL server-exit' "$probe_output" ||
                probe_fail 'the indirect manager path did not fail as a server exit'
            grep -q 'npx -y chrome-devtools-mcp@1.0.1' "$MANAGER_CALLS" 2>/dev/null ||
                probe_fail 'the manager sentinel did not fire'
            grep '^PROBE FAIL ' "$probe_output"
            ;;
        cold)
            # R6b: the unarmed image really tries the registry, so the failure is
            # a network failure of a live npm, not a missing program.
            node "$MCP_PROBE" --expect-version "$MCP_VERSION" -- \
                "$MCP_WRAPPER" sh -c 'exec npx -y chrome-devtools-mcp@1.0.1 "$@"' sh \
                --headless=true --isolated=true >"$probe_output" 2>&1 || probe_status=$?
            [ "$probe_status" -ne 0 ] || probe_fail 'the cold manager path was not rejected'
            grep -q '^PROBE FAIL server-exit' "$probe_output" ||
                probe_fail 'the cold manager path did not fail as a server exit'
            ! grep -q '^PROBE FAIL timeout' "$probe_output" || probe_fail 'the cold manager path timed out'
            ! grep -q '^PROBE FAIL spawn' "$probe_output" || probe_fail 'the cold manager path could not spawn'
            grep -q 'EAI_AGAIN' "$probe_output" || probe_fail 'npm did not report a DNS failure'
            grep -q 'registry.npmjs.org' "$probe_output" || probe_fail 'npm did not name the registry'
            grep '^PROBE FAIL ' "$probe_output"
            # Evidence, not assertion: the checks above already required both.
            grep -E 'EAI_AGAIN|registry\.npmjs\.org' "$probe_output" | head -n 5
            ;;
        *) rm -f "$probe_output"; echo 'usage: checks.sh mcp wrapper|manager|cold' >&2; exit 2 ;;
    esac
    rm -f "$probe_output"
}

# R5: one row of the wrapper's accepted/refused matrix. Accepted rows are
# asserted on the argv that reached the installed program, refused rows on the
# exit status, the message, and both recorders staying absent.
row_accept() { # row_accept LABEL EXPECTED_ARGV ARGS...
    row_label=$1
    row_expected=$2
    shift 2
    reset_records
    row_status=0
    "$MCP_WRAPPER" "$@" 2>"$ROW_ERR" || row_status=$?
    if [ "$row_status" -ne 0 ]; then
        cat "$ROW_ERR" >&2
        echo "checks.sh: argv row $row_label: exit $row_status" >&2
        exit 1
    fi
    [ -f "$ARGV_OUT" ] || { echo "checks.sh: argv row $row_label: installed bin did not run" >&2; exit 1; }
    if [ "$(cat "$ARGV_OUT")" != "$row_expected" ]; then
        echo "checks.sh: argv row $row_label: argv was $(tr '\n' ' ' < "$ARGV_OUT")" >&2
        exit 1
    fi
    [ ! -e "$MANAGER_CALLS" ] || { echo "checks.sh: argv row $row_label: a package manager ran" >&2; exit 1; }
}

row_refuse() { # row_refuse LABEL EXPECTED_STDERR ARGS...
    row_label=$1
    row_pattern=$2
    shift 2
    reset_records
    row_status=0
    "$MCP_WRAPPER" "$@" 2>"$ROW_ERR" || row_status=$?
    if [ "$row_status" -ne 2 ]; then
        cat "$ROW_ERR" >&2
        echo "checks.sh: argv row $row_label: expected refusal exit 2, got $row_status" >&2
        exit 1
    fi
    if ! grep -qF -- "$row_pattern" "$ROW_ERR"; then
        cat "$ROW_ERR" >&2
        echo "checks.sh: argv row $row_label: stderr lacks: $row_pattern" >&2
        exit 1
    fi
    if [ -e "$ARGV_OUT" ] || [ -e "$MANAGER_CALLS" ]; then
        echo "checks.sh: argv row $row_label: a refusal left a recorder file" >&2
        exit 1
    fi
}

argv_matrix() { # argv_matrix root|numeric
    case "$1" in
        root) user_uid=9999 ;;
        numeric) user_uid=12345 ;;
        *) echo 'usage: checks.sh argv-matrix root|numeric' >&2; exit 2 ;;
    esac
    not_bin=/tmp/argv-generic/chrome-devtools-mcp
    bin=$MCP_PREFIX/node_modules/.bin/chrome-devtools-mcp
    unnamed='does not name the installed chrome-devtools-mcp@1.0.1'
    manager='is a package manager'
    dash="command must not start with '-'"
    # A generic path with a non-manager basename, so row 14 proves exec rather
    # than a 127: /tmp is writable for the numeric user too.
    mkdir -p /tmp/argv-generic
    cp /agent-vm-test/argv-recorder.sh "$not_bin"
    chmod 0755 "$not_bin"

    row_accept 1 '--headless=true
--isolated=true' npx -y "chrome-devtools-mcp@$MCP_VERSION" --headless=true --isolated=true
    row_accept 2 '' npx -y "chrome-devtools-mcp@$MCP_VERSION"
    row_accept 3 'a b
*
--
--x' npx -y "chrome-devtools-mcp@$MCP_VERSION" 'a b' '*' -- --x
    row_accept 4 '--headless=true' chrome-devtools-mcp --headless=true
    row_accept 5 '--version' "$bin" --version
    row_refuse 6 "$unnamed" npx -y chrome-devtools-mcp@9.9.9 --headless=true
    row_refuse 7a "$unnamed" npx --yes "chrome-devtools-mcp@$MCP_VERSION"
    row_refuse 7b "$unnamed" npx "chrome-devtools-mcp@$MCP_VERSION"
    row_refuse 7c "$unnamed" npx -y -- "chrome-devtools-mcp@$MCP_VERSION"
    row_refuse 7d "$unnamed" npx -y "chrome-devtools-mcp@$MCP_VERSION "
    row_refuse 8a "$unnamed" npx
    row_refuse 8b "$unnamed" npx -y
    row_refuse 9a "$manager" /usr/bin/npx -y "chrome-devtools-mcp@$MCP_VERSION"
    row_refuse 9b "$manager" ./npx -y "chrome-devtools-mcp@$MCP_VERSION"
    row_refuse 9c "$manager" /x/.bin/npx -y "chrome-devtools-mcp@$MCP_VERSION"
    row_refuse 10a "$manager" npm exec chrome-devtools-mcp
    row_refuse 10b "$manager" pnpm dlx chrome-devtools-mcp
    row_refuse 10c "$manager" pnx chrome-devtools-mcp
    row_refuse 10d "$manager" pnpx chrome-devtools-mcp
    row_refuse 10e "$manager" corepack enable
    row_refuse 10f "$manager" yarn dlx chrome-devtools-mcp
    row_refuse 10g "$manager" bunx chrome-devtools-mcp
    row_refuse 10h "$manager" /usr/local/bin/pnpm install
    row_refuse 11 'usage: agent-vm-chrome-mcp COMMAND [ARG ...]'
    row_refuse 12a "$dash" -- npx -y "chrome-devtools-mcp@$MCP_VERSION"
    row_refuse 12b "$dash" --
    row_refuse 12c "$dash" -x
    row_accept 14 '--version' "$not_bin" --version
    # Row 13: everything else is a generic exec, including the launcher's
    # rewritten user identity and PR A's browser path.
    reset_records
    row_status=0
    "$MCP_WRAPPER" sh -ec "test \"\$(id -u)\" = $user_uid" 2>"$ROW_ERR" || row_status=$?
    [ "$row_status" -eq 0 ] || { cat "$ROW_ERR" >&2; echo "checks.sh: argv row 13: exit $row_status" >&2; exit 1; }
    require_output '<html' timeout "${BROWSER_LIMIT_SECONDS}s" \
        "$MCP_WRAPPER" chromium --headless --disable-gpu --dump-dom about:blank
    [ ! -e "$MANAGER_CALLS" ] || { echo 'checks.sh: argv row 13: a package manager ran' >&2; exit 1; }
}

# --- tree invariant: V-series (#41) ----------------------------------------

# A mutation that did not take effect must fail here: `verify` printing a
# rejection for an untouched tree would otherwise read as a passing control.
mutated() { # mutated MESSAGE
    echo "checks.sh: mutate-tree $case_id: mutation did not apply: $1" >&2
    exit 1
}

mutate_tree() { # mutate_tree V0..V11
    case_id=$1
    installed=$MCP_PREFIX/node_modules/chrome-devtools-mcp
    physical=$(readlink -f "$installed")
    bin=$MCP_PREFIX/node_modules/.bin/chrome-devtools-mcp
    case "$case_id" in
        V0) ;; # unmutated: the real pnpm tree with its 777 link must be accepted
        V1)
            chmod g+w "$physical/package.json"
            find "$physical/package.json" -maxdepth 0 -perm /0020 -print | grep -q . ||
                mutated "$physical/package.json"
            ;;
        V2)
            chmod o+w "$MCP_PREFIX/node_modules/.bin"
            find "$MCP_PREFIX/node_modules/.bin" -maxdepth 0 -perm /0002 -print | grep -q . ||
                mutated "$MCP_PREFIX/node_modules/.bin"
            ;;
        V3)
            chown 9999 "$MCP_PREFIX/pnpm-lock.yaml"
            [ "$(stat -c '%u' "$MCP_PREFIX/pnpm-lock.yaml")" = 9999 ] || mutated pnpm-lock.yaml
            ;;
        V4)
            ln -sfn /tmp "$installed"
            [ "$(readlink "$installed")" = /tmp ] || mutated "$installed"
            ;;
        V5)
            ln -s /etc "$MCP_PREFIX/node_modules/evil"
            [ "$(readlink "$MCP_PREFIX/node_modules/evil")" = /etc ] || mutated node_modules/evil
            ;;
        V6)
            ln -s .pnpm/missing "$MCP_PREFIX/node_modules/dangling"
            [ "$(readlink "$MCP_PREFIX/node_modules/dangling")" = .pnpm/missing ] || mutated node_modules/dangling
            ;;
        V7)
            chmod o+w /opt/agent-vm
            find /opt/agent-vm -maxdepth 0 -perm /0002 -print | grep -q . || mutated /opt/agent-vm
            ;;
        V8)
            chmod g+w "$MCP_WRAPPER"
            find "$MCP_WRAPPER" -maxdepth 0 -perm /0020 -print | grep -q . || mutated "$MCP_WRAPPER"
            ;;
        V9)
            mkfifo "$MCP_PREFIX/node_modules/fifo"
            find "$MCP_PREFIX/node_modules/fifo" -maxdepth 0 -type p -print | grep -q . ||
                mutated "$MCP_PREFIX/node_modules/fifo"
            ;;
        V10)
            target=$(readlink -f "$bin")
            chmod u+s "$target"
            find "$target" -maxdepth 0 -perm /4000 -print | grep -q . || mutated "$target"
            ;;
        V11)
            chown -h 9999 "$installed"
            [ "$(stat -c '%u' "$installed")" = 9999 ] || mutated "$installed"
            ;;
        *) echo 'usage: checks.sh mutate-tree V0..V11' >&2; exit 2 ;;
    esac
}

self_test() {
    require_output '<html' printf '<html>\n'
    require_output '^9999$' printf '9999\n'
    # A missing `timeout` would make the hang mutant below a vacuous rejection.
    require_output '<html' timeout 5 printf '<html>\n'
    expect_rejection nonzero-after-output '<html' sh -c 'printf "<html>\n"; exit 17'
    expect_rejection hang-after-output '<html' timeout 1 sh -c 'printf "<html>\n"; sleep 300'
    expect_rejection uid-nonzero-after-output '^9999$' sh -c 'printf "9999\n"; exit 17'
    # The content assertion must stay live, not just the status check.
    expect_rejection uid-wrong-content '^9999$' printf '1234\n'
    expect_rejection browser-wrong-content '^<html' printf '<!doctype html>\n'
    echo 'checks.sh: self-test passed'
}

case "${1:-}" in
    parent)
        ! command -v chromium || exit 1
        ! test -e /usr/bin/google-chrome || exit 1
        ! getent passwd chrome || exit 1
        ! test -e /home/chrome || exit 1
        ! test -e /etc/sudoers.d/agent-vm-chrome || exit 1
        ! test -e /usr/local/bin/agent-vm-chrome-mcp || exit 1
        ! test -e /etc/agent-vm-capabilities/chrome-devtools-mcp || exit 1
        ! test -e /etc/agent-vm-image-version || exit 1
        # The preinstall (#41) happens in the example layer, never in the parent.
        ! test -e /opt/agent-vm/chrome-devtools-mcp || exit 1
        ;;
    structure)
        chromium --version
        test "$(readlink -f /usr/bin/google-chrome)" = /usr/bin/chromium
        test "$(readlink -f /usr/bin/google-chrome-stable)" = /usr/bin/chromium
        test "$(readlink -f /opt/google/chrome/chrome)" = /usr/bin/chromium
        test "$(getent passwd chrome | cut -d: -f3,4,6,7)" = 9999:9999:/home/chrome:/bin/bash
        test "$(getent group chrome | cut -d: -f3)" = 9999
        grep -q '^chrome:!:' /etc/shadow
        test "$(stat -c '%u:%g:%a' /home/chrome)" = 9999:9999:755
        visudo -cf /etc/sudoers.d/agent-vm-chrome
        require_output '^9999$' sudo -n -u chrome -- id -u
        ! sudo -n -u chrome -- sudo -n id -u || exit 1
        test -x /usr/local/bin/agent-vm-chrome-mcp
        sudo -u chrome -H -- test -w /home/chrome/.pki/nssdb
        test -e /etc/agent-vm-capabilities/chrome-devtools-mcp
        test "$(stat -c '%u:%g' /home/chrome/.pki/nssdb)" = 9999:9999
        test "$(stat -c '%u:%g:%a' /etc/sudoers.d/agent-vm-chrome)" = 0:0:440
        test "$(stat -c '%u:%g:%a' /usr/local/bin/agent-vm-chrome-mcp)" = 0:0:755
        # No build-time per-user state (an earlier recipe warmed a package cache
        # here), and the installed tree satisfies its own invariant (#41).
        test "$(ls -A /home/chrome)" = .pki
        sh "$HELPER" verify
        ;;
    as-chrome)
        test "$(id -u)" = 9999
        test "$(id -g)" = 9999
        test "$HOME" = /home/chrome
        test "$USER" = chrome
        test "$LOGNAME" = chrome
        test "$(pwd)" = /home/chrome
        ! sudo -n -u root -- id -u || exit 1
        ;;
    as-numeric)
        test "$(id -u)" = 12345
        test "$HOME" = /tmp
        test "$(pwd)" = /tmp
        ! sudo -n -u root -- id -u || exit 1
        ! sudo -n -u chrome -- id -u || exit 1
        ;;
    nss)
        test -f "$HOME/.pki/nssdb/cert9.db"
        certutil -L -d "sql:$HOME/.pki/nssdb" -n microsandbox >/dev/null
        ;;
    browser | browser-root)
        require_output '<html' timeout "${BROWSER_LIMIT_SECONDS}s" \
            /usr/local/bin/agent-vm-chrome-mcp chromium --headless --disable-gpu --dump-dom about:blank
        ;;
    mcp) mcp_check "${2:-}" ;;
    argv-matrix) argv_matrix "${2:-}" ;;
    mutate-tree) mutate_tree "${2:-}" ;;
    self-test) self_test ;;
    *)
        echo 'usage: checks.sh parent|structure|as-chrome|as-numeric|nss|browser|browser-root|mcp MODE|argv-matrix MODE|mutate-tree CASE|self-test' >&2
        exit 2
        ;;
esac
