#!/bin/sh

set -eu -o pipefail

if [[ "$EUID" != '0' ]]; then
    echo "Must be running as root (EUID != 0)"
    exit 1
fi

# Allow this script to be run from outside the vm_image directory
cd -P -- "$(dirname -- "$0")"

source '../scripts-common.sh'

REGISTRY="localhost:5001"
NAME="autoscaler-sidecar"
TAG="latest"

echo "Building Dockerfile.sidecar"
docker build --quiet --no-cache --pull -t "$REGISTRY/$NAME:$TAG" -f Dockerfile.sidecar . | indent
echo "Push completed image"
docker push --quiet "$REGISTRY/$NAME:$TAG" | indent
