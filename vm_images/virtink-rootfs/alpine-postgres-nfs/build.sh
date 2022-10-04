#!/bin/bash

set -e

REGISTRY="cicdteam"
TAG="2"

docker build -t ${REGISTRY}/vm-alpine-p14-rootfs-nfs:${TAG} .
docker push -q  ${REGISTRY}/vm-alpine-p14-rootfs-nfs:${TAG}
