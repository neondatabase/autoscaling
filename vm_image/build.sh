#!/usr/bin/env bash
#
# Helper script to set up some of the files before `docker build`, as well as packaging the 'vmdata'
# container into what'll be used by NeonVM. This script is expected to be run as root.

set -eu -o pipefail

if [[ "$OSTYPE" != "darwin"* && "$EUID" != 0 ]]; then
    echo "Must be running as root (EUID != 0)"
    exit 1
fi

# Allow this script to be run from outside the vm_image directory
cd -P -- "$(dirname -- "$0")"

source '../scripts-common.sh'

NEONVM_BUILDER_PATH='neonvm-builder'
NEONVM_REPO='https://github.com/neondatabase/neonvm'
NEONVM_VERSION='v0.4.5'

if [ -e "$NEONVM_BUILDER_PATH" ]; then
    echo "Skipping downloading NeonVM vm-builder because '$NEONVM_BUILDER_PATH' already exists"
else
    echo "Downloading NeonVM vm-builder to '$NEONVM_BUILDER_PATH'..."
    curl -sSLf -o "$NEONVM_BUILDER_PATH" "$NEONVM_REPO/releases/download/$NEONVM_VERSION/vm-builder"
    echo "Done."
    chmod +x "$NEONVM_BUILDER_PATH"
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

echo 'Building vm-informant'
echo 'Note: this is only used locally so we can build it for linux & copy it into the dockerfile'

../build/vm-informant/build.sh

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
