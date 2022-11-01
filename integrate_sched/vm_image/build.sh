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

REGISTRY="localhost:5001"
NAME="pg14-tmpfs-test"
TAG="latest"

echo "Building 'Dockerfile'..."
docker buildx build -t "$REGISTRY/$NAME:$TAG" .
echo "Push completed image"
docker push "$REGISTRY/$NAME:$TAG" | indent
