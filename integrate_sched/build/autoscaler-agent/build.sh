#!/bin/sh

#!/bin/sh

set -eu -o pipefail

# Allow this script to be run from outside the directory
cd -P -- "$(dirname -- "$0")"
# Move up to the root integrate_sched directory
cd ../..
pwd

source './scripts-common.sh'

require_root

REGISTRY="localhost:5001"
NAME="autoscaler-agent"
TAG="latest"

echo "Building Dockerfile"
docker buildx build --no-cache --pull \
    -t "$REGISTRY/$NAME:$TAG" \
    -f build/autoscaler-agent/Dockerfile \
    . | indent
echo "Push completed image"
docker push --quiet "$REGISTRY/$NAME:$TAG" | indent
