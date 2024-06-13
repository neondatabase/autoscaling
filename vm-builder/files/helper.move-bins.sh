#!/neonvm/bin/sh
#
# Helper script to move binaries into /neonvm/bin and copy the dynamically loaded libraries they
# depend upon into /neonvm/lib.

set -eux -o pipefail

for name in "$@"; do
    path="$(which "$name")"
    cp "$path" /neonvm/bin/
    cp -f $(ldd "$path" | grep '=>' | grep -oE '[^ ]*/lib[^ ]+') /neonvm/lib/
done
