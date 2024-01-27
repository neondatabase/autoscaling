#!/neonvm/bin/sh

# udevadm trigger and settle is needed to ensure the virtio-ports symlinks are created.
# serial port is used for logs.
#
# /neonvm/bin/udevadm trigger
# /neonvm/bin/udevadm settle

# Unfortunately I didn't find a way to make udevadm trigger reliably, since there is no way to wait for
# the udevd startup. Instead, as a temporary solution until we have systemd, we are going to perform the
# actions ourselves.

chmod 666 /dev/vport0p1
mkdir -p /dev/virtio-ports
ln -s /dev/vport0p1 /dev/virtio-ports/tech.neon.log.0
