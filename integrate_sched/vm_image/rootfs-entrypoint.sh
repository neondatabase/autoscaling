#!/bin/sh

set -eu -o pipefail

truncate -s "$2" "$1"
mkfs.ext4 -d /rootfs "$1"
