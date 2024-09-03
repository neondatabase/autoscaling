#!/neonvm/bin/sh

set -euo pipefail

USAGE="USAGE: /neonvm/bin/set-disk-quota <NEW_SIZE_IN_KB> <MOUNTPOINT>"

if [ "$#" -eq 2 ]; then
    size="$1"
    mountpoint="$2"
else
    echo "expected 1 or 2 args, got $#" >&2
    echo "$USAGE" >&2
    exit 1
fi

/neonvm/bin/setquota -P 0 0 "$size" 0 0 "$mountpoint"

