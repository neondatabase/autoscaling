#!/neonvm/bin/sh
#
# Helper script to set up the root cgroup and cgroup namespace for CPU limiting.
#
# USAGE: /neonvm/bin/cg-setup.sh

set -eu

# enable controllers
echo '+cpu +memory' > /sys/fs/cgroup/cgroup.subtree_control

# create a new cgroup called 'neonvm-root'
mkdir /sys/fs/cgroup/neonvm-root

# create a directory and files in tmp to bind mount a new namespace -- otherwise the namespace will
# be removed once all processes exit. We could alternately keep a long-running process in the
# namespace, but it seemed easier to go this route.
mkdir -m 0600 /tmp/neonvm-user-namespace # 0600 to prevent non-root access
touch /tmp/neonvm-user-namespace/cgroup /tmp/neonvm-user-namespace/mnt

# We now need to:
#  1. enter the cgroup and create a fresh cgroup AND mount namespace
#  2. remount /sys/fs/cgroup so that we only have access to the child cgroup from within the mount
#     namespace (via bind to overwrite it)
#  3. OUTSIDE the namespace, we need to bind mount the cgroup and mount namespaces so they're
#     persisted.
#  4. Allow the process in the namespace to exit

mkfifo -m 0600 /tmp/neonvm-cgsetup-childpid.pipe
mkfifo -m 0600 /tmp/neonvm-cgsetup-nsdone.pipe

# In the background, wait for the child PID to be known.
#
# We *could* run the cgexec + unshare in the background instead and have one less child, but it's
# MUCH easier to debug if that's in the foreground.
sh -c '
child_pid="$(cat /tmp/neonvm-cgsetup-childpid.pipe)"
# persist the child namespaces by bind mounting them
mount --bind /proc/$child_pid/ns/cgroup /tmp/neonvm-user-namespace/cgroup
mount --bind /proc/$child_pid/ns/mnt    /tmp/neonvm-user-namespace/mnt
echo "" >> /tmp/neonvm-cgsetup-nsdone.pipe
' &

# 'cgexec ... neonvm-root' - enter the 'neonvm-root' cgroup
# 'unshare --cgroup --mount' - create a new cgroup and mount namespaces
#   - at this point, /sys/fs/cgroup still looks the same, although /proc/self/cgroup says we're at
#   the root (even though we're in 'neonvm-root')
# 'mount ... /sys/fs/cgroup' - restrict what's visible in /sys/fs/cgroup to just the 'neonvm-root' cgroup
cgexec -g cpu,memory:neonvm-root unshare --cgroup --mount sh -c '
echo $$ >> /tmp/neonvm-cgsetup-childpid.pipe
umount /sys/fs/cgroup
mount -t cgroup2 cgroup2 /sys/fs/cgroup
# wait for namespace binding to finish
cat /tmp/neonvm-cgsetup-nsdone.pipe
'

# done with the pipes, can get rid of them.
rm /tmp/neonvm-cgsetup-childpid.pipe /tmp/neonvm-cgsetup-nsdone.pipe

# The default cgroup will be neonvm-root/leaf to allow creation of other cgroups inside neonvm-root,
# due to the "no internal processes" rule that prevents having processes inside a cgroup when
# cgroup.subtree_control is not empty.
mkdir /sys/fs/cgroup/neonvm-root/leaf
echo "+cpu +memory" > /sys/fs/cgroup/neonvm-root/cgroup.subtree_control

# Allow all users to move processes to/from the root cgroup.
#
# This is required in order to be able to 'cgexec' anything, if the entrypoint is not being run as
# root, because moving tasks between one cgroup and another *requires write access to the
# cgroup.procs file of the common ancestor*, and because the entrypoint isn't already in a cgroup,
# any new tasks are automatically placed in the top-level cgroup.
#
# This *would* be bad for security, if we relied on cgroups for security; but instead because they
# are just used for cooperative signaling, this should be mostly ok.
chmod go+w /sys/fs/cgroup/neonvm-root/cgroup.procs
