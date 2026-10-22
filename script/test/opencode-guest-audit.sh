#!/usr/bin/env bash
# Mounted by the finished-image audit; no production certification predicate.
set -euo pipefail
source_dir=$1 audit=$2 version=$3
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/home" "$work/project/nested"
export HOME="$work/home" XDG_CONFIG_HOME="$work/home/config" XDG_CACHE_HOME="$work/home/cache"
export XDG_DATA_HOME="$work/home/data" XDG_STATE_HOME="$work/home/state"
cd "$work/project/nested"
timeout --kill-after=5 25 node "$source_dir/check-claude-runtime.mjs" /opt/agent-vm/opencode-plugins /opt/agent/.local/bin/claude >"$work/runtime"
timeout --kill-after=5 80 python3 "$source_dir/probe-opencode.py" capture --binary /opt/agent/.opencode/bin/opencode --state-root "$work" --directory "$PWD" --timeout 60 >"$work/evidence"
python3 "$audit" --evidence "$work/evidence" --runtime "$work/runtime" --status /opt/agent-vm/install-status/opencode-claude --version "$version" --directory "$PWD"
# Audit both lexical package links and physical virtual-store contents.
python3 /audit/opencode-access-files.py /opt/agent-vm/opencode-plugins >"$work/files"
xargs -0 /contract/recipe-contract tool-access --quiet --content <"$work/files"
/contract/recipe-contract tool-access --quiet --content /opt/agent-vm/opencode/opencode.json
