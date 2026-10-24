#!/bin/sh
# Replace every package manager on PATH with the recorder, so a runtime check can
# prove no manager ran (armed image, R-series). With the argument `argv`, also
# replace the installed MCP binary with the argv recorder (R5/R6a).
#
# Both recorders arrive with the fixtures directory at /agent-vm-test.
set -eu

for name in npm npx pnpm pnx pnpx corepack yarn yarnpkg bun bunx; do
    path=$(command -v "$name" 2>/dev/null) || continue
    rm -f "$path"
    cp /agent-vm-test/manager-recorder.sh "$path"
    chmod 0755 "$path"
    # The wrapper preserves PATH, so the seam directory is what a manager
    # started with the image's own PATH resolves to.
    mkdir -p /agent-vm-test/managers
    cp /agent-vm-test/manager-recorder.sh "/agent-vm-test/managers/$name"
    chmod 0755 "/agent-vm-test/managers/$name"
done

if [ "${1:-}" = argv ]; then
    bin=/opt/agent-vm/chrome-devtools-mcp/node_modules/.bin/chrome-devtools-mcp
    target=$(readlink -f "$bin")
    cp /agent-vm-test/argv-recorder.sh "$target"
    chmod 0755 "$target"
fi
