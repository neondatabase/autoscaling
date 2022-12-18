#!/bin/sh

#!/bin/sh

set -eu -o pipefail

# Allow this script to be run from outside the directory
cd -P -- "$(dirname -- "$0")"
# Move up to the repository root
cd ../..

source './scripts-common.sh'

require_root

DEFAULT_REGISTRY='localhost:5001'
DEFAULT_IMG_NAME='autoscaler-agent'
DEFAULT_TAG='latest'

REGISTRY_OR_USER="$( get_var REGISTRY_OR_USER "$DEFAULT_REGISTRY" )"
IMG_NAME="$( get_var IMG_NAME "$DEFAULT_IMG_NAME" )"
TAG="$( get_var TAG "$DEFAULT_TAG" )"

echo "Building Dockerfile, tagged as $REGISTRY_OR_USER/$IMG_NAME:$TAG"
docker buildx build --pull \
    -t "$REGISTRY_OR_USER/$IMG_NAME:$TAG" \
    -f build/autoscaler-agent/Dockerfile \
    . | indent
echo "Push completed image"
docker push "$REGISTRY_OR_USER/$IMG_NAME:$TAG" | indent
