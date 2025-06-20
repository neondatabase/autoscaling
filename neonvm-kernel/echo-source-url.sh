#!/usr/bin/env bash
#
# Helper script to generate the URL to download the kernel source.
#
# Example usage:
#
#   curl "$(./echo-source-url 6.12.26)" -o linux-6.12.26.tar.xz

set -eu -o pipefail

USAGE="$0 <VERSION>"

if [ "$#" -ne 1 ]; then
    echo "$USAGE" >/dev/stderr
    echo "Expected 1 arg, got $#." >/dev/stderr
    exit 1
fi

version="$1"
major="$(echo "$version" | sed -E 's/^([0-9]+)\.[0-9]+\.[0-9]+$/\1/')"

echo "https://cdn.kernel.org/pub/linux/kernel/v${major}.x/linux-${version}.tar.xz"
