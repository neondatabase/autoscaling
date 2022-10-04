#!/bin/bash

set -e

REGISTRY="cicdteam"
TAG="18"

docker build -t ${REGISTRY}/vm-debian-rootfs:${TAG} .
docker push -q  ${REGISTRY}/vm-debian-rootfs:${TAG}
