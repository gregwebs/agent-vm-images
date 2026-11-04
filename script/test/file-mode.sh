#!/usr/bin/env bash
# Sourced by the suites that read file modes, never run on its own: it is not in
# contracts.sh's suite list.
#
# file_mode prints one path's permission mode the way `chmod` accepts it, or
# returns 1 without printing anything. It does not exit, because only the suite
# knows whether an unreadable mode is fatal, and it says nothing itself, because
# the caller's failure message carries its own label. A caller that reads it
# through `$( )` MUST check that status: `[ "$(file_mode a)" = "$(file_mode b)" ]`
# compares two empty strings when both reads fail and passes.
#
# The two dialects stay separate, status-checked substitutions. GNU `stat -f`
# means --file-system, so it prints a filesystem block BEFORE it fails; folding
# both dialects into one `stat -f ... || stat -c ...` substitution yields a
# multi-line string that is not a mode.
file_mode() { # $1 = path
    local mode
    if mode="$(stat -c '%a' "$1" 2>/dev/null)" && [ -n "$mode" ]; then
        printf '%s\n' "$mode"
        return 0
    fi
    if mode="$(stat -f '%Lp' "$1" 2>/dev/null)" && [ -n "$mode" ]; then
        printf '%s\n' "$mode"
        return 0
    fi
    return 1
}

# Put the stub `stat` for one dialect into $1, so a suite can force the dialect
# its host does not answer in. The stub travels with this file; the caller puts
# $1 on PATH for the read alone. $BASH_SOURCE names this file, so the caller
# needs no path of its own.
install_file_mode_stat_shim() { # $1 = directory that will hold the stub
    mkdir -p "$1"
    cp "${BASH_SOURCE[0]%/*}/fixtures/file-mode-stat.sh" "$1/stat"
    chmod 0755 "$1/stat"
}
