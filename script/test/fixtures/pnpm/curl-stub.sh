#!/bin/sh
# Archive downloader double: FAIL_DOWNLOAD injects the transport failure, and
# the real `-o <path>` argument names where the pre-built ARCHIVE is copied.
set -eu
[ "${FAIL_DOWNLOAD:-}" != yes ] || exit 28
while [ "$1" != -o ]; do shift; done
cp "$ARCHIVE" "$2"
