#!/bin/bash
# setup-go reads the language minimum, not the toolchain directive. Assert the
# module-selected compiler so a runner newer than the pin cannot hide drift.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${1:-$root/images/recipe-contract}"
GOTOOLCHAIN=auto go test -tags recipe_contract_image_compiler -run '^TestImageCompiler$' ./internal/buildcheck
