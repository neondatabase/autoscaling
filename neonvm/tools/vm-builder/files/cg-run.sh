#!/neonvm/bin/sh
#
# Helper script to run a program in the root cgroup (+ cgroup namespace).
# This is automatically used to run user-provided programs so that they're transparently included in
# a cgroup that we have control over (and can limit the CPU of, for fractional CPU support).
#
# USAGE: /neonvm/bin/cg-run.sh <COMMAND...>

set -eux

# cgexec ... - run in the neonvm-root/leaf cgroup
# nsenter ... - run in the cgroup namespace
# "$@" - the command we were asked to run
exec /neonvm/bin/cgexec -g cpu,memory:neonvm-root/leaf \
    /neonvm/bin/nsenter --cgroup=/tmp/neonvm-user-namespace/cgroup --mount=/tmp/neonvm-user-namespace/mnt \
    "$@"
