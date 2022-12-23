#!/bin/sh

set -eu -o pipefail

# Allow this script to be run from outside the directory
cd -P -- "$(dirname -- "$0")"
# Move up to the root integrate_sched directory
cd ../..

source './scripts-common.sh'

require_root

DEFAULT_REGISTRY="localhost:5001"
DEFAULT_IMG_NAME="kube-autoscale-scheduler"
DEFAULT_TAG="latest"

REGISTRY_OR_USER="$( get_var REGISTRY_OR_USER "$DEFAULT_REGISTRY" )"
IMG_NAME="$( get_var IMG_NAME "$DEFAULT_IMG_NAME" )"
TAG="$( get_var TAG "$DEFAULT_TAG" )"

GIT_INFO="$(git_info)"
echo "Git info: $GIT_INFO"

echo "Building Dockerfile, tagged as $REGISTRY_OR_USER/$IMG_NAME:$TAG"
docker buildx build --pull \
    -t "$REGISTRY_OR_USER/$IMG_NAME:$TAG" \
    --build-arg "GIT_INFO=$GIT_INFO" \
    -f build/autoscale-scheduler/Dockerfile \
    . | indent
echo "Push completed image"
docker push "$REGISTRY_OR_USER/$IMG_NAME:$TAG" | indent
