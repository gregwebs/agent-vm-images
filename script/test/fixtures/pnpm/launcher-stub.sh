#!/bin/sh
# Fixture launcher/source for the real verifier. `--version` and `config get`
# answer from data (PNPM_FIXTURE_VERSION and the fixed defaults below) so the
# verifier can compare a published launcher against its reviewed source.
set -eu
case "$1" in
--version) echo "${PNPM_FIXTURE_VERSION}" ;;
config) case "$3" in
minimum-release-age) echo 1440 ;;
minimum-release-age-strict|ignore-scripts) echo true ;;
minimum-release-age-ignore-missing-time) echo false ;;
trust-policy) echo no-downgrade ;;
esac ;;
esac
