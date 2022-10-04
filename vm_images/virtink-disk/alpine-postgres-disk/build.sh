#!/bin/bash

set -e

IMAGE_SIZE="8G"
REGISTRY="cicdteam"
TAG="4"

clean () {
  set +e
  docker rm -f ${id} >/dev/null 2>&1 || true
  docker rmi vmdata >/dev/null 2>&1 || true
  sudo umount ${tdir} >/dev/null 2>&1 || true
  sudo rm -rf ${tdir} >/dev/null 2>&1 || true
  rm -f disk.raw disk.qcow2  >/dev/null 2>&1 || true
}

trap clean TERM KILL EXIT

docker build --quiet --no-cache --pull -t vmdata .
id=$(docker create vmdata)

rm -f disk.raw disk.qcow2
dd if=/dev/zero of=disk.raw bs=1 count=0 seek=${IMAGE_SIZE}
mkfs.ext4 -F disk.raw
tdir=$(mktemp -d -p .)
sudo mount -o loop disk.raw "${tdir}"
docker export ${id} | sudo tar x -C "${tdir}"
sudo umount "${tdir}"
docker rm -f ${id}
docker image rm -f vmdata

qemu-img convert -f raw -O qcow2 disk.raw disk.qcow2
rm -f disk.raw
echo
qemu-img info disk.qcow2

docker build --quiet --no-cache --pull -t ${REGISTRY}/vm-alpine-p14-disk:${TAG} -f Dockerfile.disk .
docker push --quiet ${REGISTRY}/vm-alpine-p14-disk:${TAG}
docker build --quiet --no-cache --pull -t ${REGISTRY}/vm-alpine-p14-cdi:${TAG} -f Dockerfile.cdi .
docker push --quiet ${REGISTRY}/vm-alpine-p14-cdi:${TAG}

#cp disk.qcow2 linux.qcow2
rm -f disk.qcow2
