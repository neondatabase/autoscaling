#!/neonvm/bin/sh

set -euo pipefail

USAGE="USAGE: /neonvm/bin/set-disk-quota <NEW_SIZE_IN_KB> <MOUNTPOINT>"

if [ "$#" -eq 2 ]; then
    size="$1"
    mountpoint="$2"
else
    echo "expected 2 args, got $#" >&2
    echo "$USAGE" >&2
    exit 1
fi

echo "Setting disk quota for $mountpoint to $size KB"

# setquota -P <project_id> <block-softlimit> <block-hardlimit> <inode-softlimit> <inode-hardlimit> <filesystem>
/neonvm/bin/setquota -P 0 0 "$size" 0 0 "$mountpoint"

echo "DONE! Disk quota set"
/neonvm/bin/repquota -P -a

