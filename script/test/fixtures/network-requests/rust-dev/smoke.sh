#!/usr/bin/env bash
# Runs inside the image under test, as an arbitrary uid, on a network whose only
# route out is the restricted proxy. `--locked` fixes the fetched set to the
# committed Cargo.lock, so a new host cannot enter through a version bump.
set -euo pipefail
exec cargo fetch --locked
