#!/usr/bin/env bash
# One first-use canary leg, executed inside the example image.
#
#   canary-guest.sh PERSISTED_SETTING SETUP_COMMAND COMMAND
#
# The harness passes the three strings as real argv (never through a shell), so
# the leg definitions stay data. This program then, in the image's own layout:
#
#   1. clones the read-only shared homes into writable scratch copies;
#   2. sets the persisted `auto-install` setting the leg names;
#   3. runs the leg's setup, then its command, each as its own `bash -c`.
#
# Status contract, so the harness never mistakes a harness fault for a policy
# observation:
#
#   98  the setup or command is not valid Bash (nothing ran)
#   99  the leg's own setup failed (the policy under test was never reached)
#   *   the command's own status, including a tool's expected nonzero
#
# `CANARY_ROOT`, `CANARY_PROJECT` and the `CANARY_IMAGE_*` overrides exist only so
# the daemon-free self-test can execute this same program against a scratch tree.
# The native leg sets none of them.
set -euo pipefail

root=${CANARY_ROOT:-}
tmp="$root/tmp"
project=${CANARY_PROJECT:-$tmp/project}
image_rustup=${CANARY_IMAGE_RUSTUP:-/opt/rustup}
image_cargo=${CANARY_IMAGE_CARGO:-/opt/cargo}
image_work=${CANARY_IMAGE_WORK:-/work}
image_toolchain=${CANARY_IMAGE_TOOLCHAIN:-/toolchain}

persisted=$1
setup=${2:-}
command=$3

# A syntax error is a harness defect, not a policy result: refuse before the
# setup so a negative leg cannot pass by failing to launch.
for program in "$setup" "$command"; do
    if ! bash -n -c "$program"; then
        echo 'CANARY-PARSE-FAILED: the leg is not valid Bash' >&2
        exit 98
    fi
done

# Setup is the harness's own preparation, so every operation is classified as
# status 99 individually. A `{ ... } || handler` group would run with `errexit`
# suppressed, and a failed `rustup set` could then be masked by a later success
# -- the leg would claim a persisted policy it never applied.
setup_failed() {
    echo 'CANARY-SETUP-FAILED: the leg setup did not complete' >&2
    exit 99
}

mkdir -p "$tmp/home" "$tmp/project" || setup_failed
cp -R "$image_rustup" "$tmp/rustup" || setup_failed
cp -R "$image_cargo" "$tmp/cargo" || setup_failed
export RUSTUP_HOME="$tmp/rustup" CARGO_HOME="$tmp/cargo"
# The image already has the real bin directory on PATH; prepending the writable
# clone is what a scratch run needs and changes nothing in the image.
export PATH="$tmp/cargo/bin:$PATH"
export WORK="$image_work" TOOLCHAIN="$image_toolchain" PROJECT="$project"

"$tmp/cargo/bin/rustup" set auto-install "$persisted" || setup_failed
cp -R "$WORK/." "$tmp/project" || setup_failed
cd "$project" || setup_failed
if [ -n "$setup" ]; then
    bash -c "$setup" || setup_failed
fi

bash -c "$command"
