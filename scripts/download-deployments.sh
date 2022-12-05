#!/bin/sh
#
# Helper script to download the external deployment files we use.

set -eu -o pipefail

# Allow this script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"
cd .. # but all of the references are to things in the upper directory

echo "downloading 'deploy/flannel.yaml'..."
curl -sS https://raw.githubusercontent.com/flannel-io/flannel/v0.19.2/Documentation/kube-flannel.yml \
    -o deploy/flannel.yaml
echo "downloading 'cert-manager.yaml'..."
curl -sSL https://github.com/cert-manager/cert-manager/releases/download/v1.8.2/cert-manager.yaml \
    -o deploy/cert-manager.yaml
echo "downloading 'multus-daemonset.yaml'..."
curl -sS https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset.yml \
    -o deploy/multus-daemonset.yaml
