#!/neonvm/bin/sh

set -euo pipefail

USAGE="USAGE: /neonvm/bin/resize-swap [ --once ] <NEW_SIZE_IN_BYTES>"

if [ "$#" -eq 2 ]; then
    if [ "$1" != '--once' ]; then
        echo "expected first arg to be '--once'" >&2
        echo "$USAGE" >&2
        exit 1
    fi
    size="$2"
    once=yes
elif [ "$#" -eq 1 ]; then
    size="$1"
    once=no
else
    echo "expected 1 or 2 args, got $#" >&2
    echo "$USAGE" >&2
    exit 1
fi

if [ ! -f /neonvm/runtime/resize-swap-internal.sh ]; then
    echo 'not found: swap may not be enabled' >&2
    exit 1
fi

/neonvm/bin/sh /neonvm/runtime/resize-swap-internal.sh "$size"
if [ "$once" = 'yes' ]; then
    # remove *this* script so that it cannot be called again.
    rm /neonvm/bin/resize-swap
fi
