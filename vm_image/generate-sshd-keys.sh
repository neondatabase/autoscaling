#!/bin/sh
#
# note: We've been careful to ensure that this script is POSIX shell compliant, so that we don't
# require bash in the test VM image.

echo "Generating sshd keys"
mkdir -p /run/sshd/etc
ln -s /run/sshd /run/sshd/etc/ssh
ssh-keygen -A -f /run/sshd # using -f here just prefixes, so the paths are /run/sshd/etc/ssh/ssh_host_*
rm /run/sshd/etc/ssh
