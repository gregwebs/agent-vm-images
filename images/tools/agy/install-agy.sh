#!/bin/sh
# Owning install hook for the agy layer: validate the exact version slot, require
# the committed agy pin for it, and hand the install to the shared release-artifact
# installer. Bind-mounted, never COPYed, so it stays out of the shipped image.
#
# agy publishes no signature, so this hook adds no verification of its own: the
# installer is the one place that re-checks the committed corroboration against the
# pin and accepts only the committed digests. What the hook owns is the boundary
# refusal — an exact-but-unpinned version stops here, before any fetch, so the pin
# stays the only authority on what may be installed.
set -eu

version="${AGENT_VM_VERSION_AGY:?install-agy.sh needs AGENT_VM_VERSION_AGY}"
source_dir="${AGENT_VM_AGY_SOURCE_DIR:?install-agy.sh needs AGENT_VM_AGY_SOURCE_DIR}"
artifact_source="${ARTIFACT_SOURCE_DIR:-/tmp/artifact-source}"
contract="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
arch="${AGENT_VM_AGY_ARCH:?install-agy.sh needs AGENT_VM_AGY_ARCH}"

# The version grammar, the architecture, the committed pin's readability and the
# pin/version agreement are one offline read through the image's own reader (ADR
# 0017). The reader owns the grammar, so a hook cannot accept a version the loader
# rejects, and a refusal stops here -- before any fetch -- so the pin stays the only
# authority on what may be installed.
"$contract/recipe-contract" agy-pin check --pin-dir "$source_dir" \
    --version "$version" --arch "$arch" || exit 1

# No soft-fail branch: agy is required and has no absence producer.
exec bash "$artifact_source/install-artifact.sh" agy "$source_dir/pin.json" "$arch"
