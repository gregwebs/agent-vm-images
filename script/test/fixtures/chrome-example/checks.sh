#!/bin/sh
set -eu

# Headless browser bound, owned here because the browser runs in this container.
BROWSER_LIMIT_SECONDS=30

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
    self-test) self_test ;;
    *) echo 'usage: checks.sh parent|structure|as-chrome|as-numeric|nss|browser|browser-root|self-test' >&2; exit 2 ;;
esac
