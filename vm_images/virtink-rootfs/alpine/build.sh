#!/bin/bash

set -e

REGISTRY="cicdteam"
TAG="15"

docker build -t ${REGISTRY}/vm-alpine-rootfs:${TAG} .
docker push -q  ${REGISTRY}/vm-alpine-rootfs:${TAG}
