#!/bin/bash
#
# Helper script to set up some of the files before `docker build`. This script is expected to be run
# as root.

set -eu -o pipefail

if [[ "$EUID" != '0' ]]; then
    echo "Must be running as root (EUID != 0)"
    exit 1
fi

# Allow this script to be run from outside the vm_image directory
cd -P -- "$(dirname -- "$0")"

source '../scripts-common.sh'

if [ -a 'ssh_id_rsa' ]; then
    echo "Skipping keygen because 'ssh_id_rsa' already exists"
else
    echo "Generating new keypair..."
    ssh-keygen -t rsa -f 'ssh_id_rsa' | indent
    # This script is probably called as root; make sure that the plain user can interact with the
    # generated files.
    chmod uga+rw 'ssh_id_rsa' 'ssh_id_rsa.pub'
fi

clean () {
    set +eu
    docker rm -f "$id" >/dev/null 2>&1 || true
    docker rmi vmdata-build >/dev/null 2>&1 || true
    umount "$tmpdir" >/dev/null 2>&1 || true
    rm -rf "$tmpdir" >/dev/null 2>&1 || true
    rm -f disk.raw disk.qcow2 >/dev/null 2>&1 || true
}

trap clean TERM KILL EXIT

REGISTRY="localhost:5001"
NAME="pg14-disk-test"
TAG="latest"
IMAGE_SIZE="2G"

# Make a temporary container so that we include it in the final build
echo "Building 'Dockerfile.vmdata'..."
docker build --quiet --no-cache --pull -t vmdata-build -f Dockerfile.vmdata . | indent
id="$(docker create vmdata-build)"

echo "Creating disk.raw ext4 filesystem..."
echo "> dd:"
dd if=/dev/zero of='disk.raw' bs=1 count=1 seek="$IMAGE_SIZE" 2>&1 | indent
echo "> mkfs.ext4:"
mkfs.ext4 -F 'disk.raw' 2>&1 | indent
tmpdir="$(mktemp -d -p .)"
echo "Mount 'disk.raw' at $tmpdir"
mount -o loop 'disk.raw' "$tmpdir" | indent

echo "Exporting vmdata image into disk.raw"
docker export "$id" | tar -x -C "$tmpdir" | indent
echo "Unmount $tmpdir"
umount "$tmpdir" | indent

echo "Clean up temporary docker artifacts"
docker rm -f "$id" | indent
docker image rm -f "vmdata-build" | indent

echo "Convert 'disk.raw' -> 'disk.qcow2'"
qemu-img convert -f raw -O qcow2 'disk.raw' 'disk.qcow2' | indent
echo "Clean up 'disk.raw'"
rm -f 'disk.raw'

echo "Build final 'Dockerfile.img'..."
docker build --quiet --no-cache --pull -t "$REGISTRY/$NAME:$TAG" -f Dockerfile.img . | indent
echo "Push completed image"
docker push --quiet "$REGISTRY/$NAME:$TAG" | indent
