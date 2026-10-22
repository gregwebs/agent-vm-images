#!/bin/bash
# Install the module-owned lint pin through the Go checksum database.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root/images/recipe-contract"
version=$(<golangci-lint.version)
GOTOOLCHAIN=auto go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$version"
