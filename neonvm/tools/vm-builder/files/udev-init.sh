#!/neonvm/bin/sh

# udevadm trigger and settle is needed to ensure the virtio-ports symlinks are created.
# serial port is used for logs.
/neonvm/bin/udevadm trigger
/neonvm/bin/udevadm settle
