#!/bin/bash

set -e

REGISTRY="cicdteam"
TAG="3"

docker build -t ${REGISTRY}/vm-alpine-p14-rootfs-tmpfs:${TAG} .
docker push -q  ${REGISTRY}/vm-alpine-p14-rootfs-tmpfs:${TAG}
