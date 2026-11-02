#!/usr/bin/env bash
# Runs inside the image under test, as an arbitrary uid, on a network whose only
# route out is the restricted proxy. There is deliberately no go.sum: a missing
# sum forces checksum verification, which is the traffic the sumdb host covers.
# -x prints each module operation, so the transcript names what was fetched.
set -euo pipefail
exec go mod download -x
