#!/bin/bash

echo "wait for cloud-init finish"
cloud-init status --wait

CLUSTER_ADDRESS=$1
TOKEN=$2

echo "install cni plugins"
sudo mkdir -p /opt/cni/bin
curl -sfL https://github.com/containernetworking/plugins/releases/download/v1.1.1/cni-plugins-linux-amd64-v1.1.1.tgz -o - | sudo tar xzf - -C /opt/cni/bin

echo "join to cluster"
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable K3S_URL=https://${CLUSTER_ADDRESS}:6443 K3S_TOKEN=${TOKEN} sudo -E -H sh -s - \
    --kubelet-arg 'system-reserved=cpu=500m,memory=1024Mi,ephemeral-storage=1Gi' \
    --kubelet-arg 'kube-reserved=cpu=200m,memory=256Mi'

echo "done"
