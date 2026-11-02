#!/usr/bin/env bash
# Runs inside the image under test as an arbitrary numeric uid, on a network whose
# only route is the strict proxy. It adds the proxy CA to a private trust bundle
# through the environment only: where the software connects stays the image's
# default, and the image is not modified.
#
# Usage (as the container command): run-in-image.sh smoke
#                                   run-in-image.sh destinations --proxy H:P --case NAME CONNECT SNI HOST ...
set -euo pipefail
work=$(mktemp -d /tmp/network-requests.XXXXXX)
cat /etc/ssl/certs/ca-certificates.crt /egress/ca.pem >"$work/trust.pem"
export SSL_CERT_FILE="$work/trust.pem"
export CARGO_HTTP_CAINFO="$work/trust.pem"
mkdir -p "$HOME"
case "${1:-smoke}" in
    smoke)
        mkdir -p "$work/project"
        cp -R /egress/fixture/. "$work/project"
        cd "$work/project"
        exec bash ./smoke.sh
        ;;
    destinations)
        shift
        exec python3 /egress/destination-probe.py --ca "$work/trust.pem" "$@"
        ;;
    *)
        echo "run-in-image: unknown mode $1" >&2
        exit 2
        ;;
esac
