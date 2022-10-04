#!/bin/bash

KERNEL_VERSION="5.15.12"
DOCKER_IMAGE="cicdteam/clh-kernel"
DOCKER_TAG="${KERNEL_VERSION}"

set -e

if [ -d linux ]; then
    pushd linux
    git fetch
    popd
else
    git clone --depth 1 https://github.com/cloud-hypervisor/linux.git -b ch-${KERNEL_VERSION} linux
fi

curl -sfL https://raw.githubusercontent.com/cloud-hypervisor/cloud-hypervisor/main/resources/linux-config-x86_64 -o linux/.config

./linux/scripts/kconfig/merge_config.sh -m -y -O linux linux/.config kernel_config_override

pushd linux
make -j `nproc`
popd

cp linux/vmlinux ./
#cp -f linux/arch/x86/boot/compressed/vmlinux ./

docker build -t ${DOCKER_IMAGE}:${DOCKER_TAG} -f Dockerfile .
docker push -q  ${DOCKER_IMAGE}:${DOCKER_TAG}
