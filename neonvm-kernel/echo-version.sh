#!/usr/bin/env bash
#
# Helper script to print the current kernel version in use.
#
# Example usage:
#
#   echo "Using version $(./echo-version.sh)"

set -eu -o pipefail

cd -P -- "$(dirname "$0")"

linux_config="$(ls ./linux-config-*)"
kernel_version="${linux_config##*-}"

echo "$kernel_version"
