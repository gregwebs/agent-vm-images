#!/bin/sh
# Only runtime tests seed a global config into the probe's invocation-private HOME.
set -eu
mkdir -p "$XDG_CONFIG_HOME/opencode"
printf '{"plugins":[]}\n' >"$XDG_CONFIG_HOME/opencode/opencode.json"
exec /opt/agent/.opencode/bin/opencode "$@"
