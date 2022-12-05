#!/bin/bash
#
# Helper script to set up some of the files before `docker build`, as well as packaging the 'vmdata'
# container into what'll be used by NeonVM. This script is expected to be run as root.

set -eu -o pipefail

if [[ "$EUID" != '0' ]]; then
    echo "Must be running as root (EUID != 0)"
    exit 1
fi

# Allow this script to be run from outside the vm_image directory
cd -P -- "$(dirname -- "$0")"

source '../scripts-common.sh'

NEONVM_BUILDER_PATH='neonvm-builder'
NEONVM_COMMITISH='sharnoff/dev' # Temporary; we require some special stuff for autoscaling (as of 2022-11-27)
# note: explicitly use http here so that we don't require ssh credentials
NEONVM_REPO='https://github.com/neondatabase/neonvm'

if [ -e "$NEONVM_BUILDER_PATH" ]; then
    echo "Skipping building vm-builder because '$NEONVM_BUILDER_PATH' already exists"
else
    tmpdir="$(mktemp -d vm-builder.build.XXXX)"
    echo "Building new vm-builder in '$tmpdir'..."

    cleanup() { if [ -e "$tmpdir" ]; then rm -rf "$tmpdir"; fi }
    trap cleanup EXIT INT TERM

    # execute in a sub-shell so we un-cd when done automatically
    (
        cd "$tmpdir"
        set -x
        git clone -b "$NEONVM_COMMITISH" "$NEONVM_REPO" . # clone into current dir
        go build -o vm-builder tools/vm-builder/main.go
    )

    mv "$tmpdir/vm-builder" "$NEONVM_BUILDER_PATH"

    echo "Done building vm-builder, removing tmpdir"
    cleanup
fi

if [ -a 'ssh_id_rsa' ]; then
    echo "Skipping keygen because 'ssh_id_rsa' already exists"
else
    echo "Generating new keypair..."
    ssh-keygen -t rsa -f 'ssh_id_rsa' | indent
    # This script is probably called as root; make sure that the plain user can interact with the
    # generated files.
    chmod uga+rw 'ssh_id_rsa' 'ssh_id_rsa.pub'
fi

REGISTRY="localhost:5001"
NAME="pg14-disk-test"
TAG="latest"
IMAGE_SIZE="1G"

echo 'Build Configuration:'
echo " * REGISTRY = '$REGISTRY'"
echo " * NAME     = '$NAME'"
echo " * TAG      = '$TAG'"
echo " * IMAGE_SIZE = '$IMAGE_SIZE'"

echo "Building 'Dockerfile.vmdata'..."
docker buildx build -t "vmdata-temp:$TAG" -f Dockerfile.vmdata . | indent
echo "Building NeonVM image..."
"./$NEONVM_BUILDER_PATH" --src "vmdata-temp:$TAG" --dst "$REGISTRY/$NAME:$TAG" --size "$IMAGE_SIZE" | indent
echo "Push completed image"
docker push "$REGISTRY/$NAME:$TAG" | indent
