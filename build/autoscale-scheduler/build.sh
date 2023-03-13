#!/usr/bin/env bash

set -eu -o pipefail

# Allow this script to be run from outside the directory
cd -P -- "$(dirname -- "$0")"
# Move up to the root integrate_sched directory
cd ../..

source './scripts-common.sh'

require_root

DEFAULT_IMG_NAME="kube-autoscale-scheduler"
DEFAULT_TAG="dev"

IMG_NAME="$( get_var IMG_NAME "$DEFAULT_IMG_NAME" )"
TAG="$( get_var TAG "$DEFAULT_TAG" )"

GIT_INFO="$(git_info)"
echo "Git info: $GIT_INFO"

echo "Building Dockerfile, tagged as $IMG_NAME:$TAG"
docker buildx build --pull \
    -t "$IMG_NAME:$TAG" \
    --build-arg "GIT_INFO=$GIT_INFO" \
    -f build/autoscale-scheduler/Dockerfile \
    . | indent
