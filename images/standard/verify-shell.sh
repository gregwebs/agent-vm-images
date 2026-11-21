#!/bin/sh
# Build-time certification of the interactive shell defaults the recipe appends
# to the distro rc files. Presence of a file proves nothing about what a session
# reads, and the scratch HOME here is what a caller's mount looks like: writable,
# not ours, and the only place the user-level rc files could come from.
set -eu

fail() {
    echo "verify-shell.sh: $*" >&2
    exit 1
}

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

# stderr carries bash's missing-tty job-control warning, which is an artifact of
# probing an interactive shell without a terminal.
bash_session() {
    HOME="$scratch" bash -ic "$1" 2>/dev/null
}
zsh_session() {
    HOME="$scratch" zsh -ic "$1" 2>/dev/null
}

expect() {
    [ "$2" = "$3" ] || fail "$1: expected '$3', got '$2'"
}

command -v zsh >/dev/null || fail 'zsh is not on PATH'
[ -f /usr/share/bash-completion/bash_completion ] ||
    fail 'the bash-completion package did not install its completion script'

# Agent commands run non-interactive shells, and those must keep the image
# environment: the defaults exist for a person, not for a script.
if bash -c 'shopt -q histappend'; then
    fail 'bash -c sources the interactive defaults'
fi
if zsh -c 'setopt' | grep -qx sharehistory; then
    fail 'zsh -c sources the interactive defaults'
fi

# The image PATH is the launcher's contract; no rc file may move it.
expect 'bash PATH under -i' "$(bash_session 'printf %s "$PATH"')" "$PATH"
expect 'zsh PATH under -i' "$(zsh_session 'printf %s "$PATH"')" "$PATH"

expect 'bash SHELL' "$(bash_session 'printf %s "$SHELL"')" /bin/bash
expect 'bash PS1' "$(bash_session 'printf %s "$PS1"')" '\[\e[32m\]\w\[\e[0m\] \$ '
expect 'bash HISTSIZE' "$(bash_session 'printf %s "$HISTSIZE"')" 50000
expect 'bash HISTFILESIZE' "$(bash_session 'printf %s "$HISTFILESIZE"')" 50000
expect 'bash HISTCONTROL' "$(bash_session 'printf %s "$HISTCONTROL"')" 'ignoreboth:erasedups'
expect 'bash histappend' "$(bash_session 'shopt -q histappend && printf on || printf off')" on
expect 'bash checkwinsize' "$(bash_session 'shopt -q checkwinsize && printf on || printf off')" on
[ -n "$(bash_session 'printf %s "${BASH_COMPLETION_VERSINFO-}"')" ] ||
    fail 'the bash defaults did not load bash-completion'

expect 'zsh SHELL' "$(zsh_session 'printf %s "$SHELL"')" /bin/zsh
expect 'zsh PROMPT' "$(zsh_session 'printf %s "$PROMPT"')" '%F{green}%~%f %# '
expect 'zsh HISTFILE' "$(zsh_session 'printf %s "$HISTFILE"')" "$scratch/.zsh_history"
expect 'zsh HISTSIZE' "$(zsh_session 'printf %s "$HISTSIZE"')" 50000
expect 'zsh SAVEHIST' "$(zsh_session 'printf %s "$SAVEHIST"')" 50000
for option in sharehistory histignorealldups histignorespace; do
    zsh_session 'setopt' | grep -qx "$option" || fail "the zsh defaults do not set $option"
done
expect 'zsh git completion' "$(zsh_session 'printf %s "${+_comps[git]}"')" 1

# Two sessions per shell, one history file: what a second session writes must
# not replace what the first left behind.
printf 'echo bash-first\n' | HOME="$scratch" bash -i >/dev/null 2>&1
printf 'echo bash-second\n' | HOME="$scratch" bash -i >/dev/null 2>&1
for line in 'echo bash-first' 'echo bash-second'; do
    grep -qx "$line" "$scratch/.bash_history" || fail "bash history lost: $line"
done
printf 'echo zsh-first\n' | HOME="$scratch" zsh -i >/dev/null 2>&1
printf 'echo zsh-second\n' | HOME="$scratch" zsh -i >/dev/null 2>&1
for line in 'echo zsh-first' 'echo zsh-second'; do
    grep -qx "$line" "$scratch/.zsh_history" || fail "zsh history lost: $line"
done
