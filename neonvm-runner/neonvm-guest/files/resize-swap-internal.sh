#!/neonvm/bin/sh
set -euxo pipefail

new_size="$1"

# this script may be run as root, so we should avoid potentially-malicious path injection
export PATH="/neonvm/bin"

swapdisk="$(/neonvm/bin/blkid -L swapdisk)"

# disable swap. Allow it to fail if it's already disabled.
swapoff "$swapdisk" || true

# if the requested size is zero, then... just exit. There's nothing we need to do.
if [ "$new_size" = '0' ]; then exit 0; fi

# re-make the swap.
#  mkswap expects the size to be given in KiB, so divide the new size by 1K
mkswap -L swapdisk "$swapdisk" $(( new_size / 1024 ))
# ... and then re-enable the swap
#
# nb: busybox swapon only supports '-d', not its long form '--discard'.
swapon -d "$swapdisk"
