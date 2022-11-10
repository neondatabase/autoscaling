#!/bin/bash
#
# Helper script to initialize the kind cluster, once everything is built.

set -eu -o pipefail

# Allow this script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"
cd .. # but all of the references are to things in the upper directory

source './scripts-common.sh'

set -x

kind create cluster -n autoscale-sched --config=kind/config.yaml

kubectl apply -f deploy/flannel.yaml -f deploy/multus-daemonset.yaml -f deploy/cert-manager.yaml \
    | indent

kubectl wait pod -n cert-manager --for=condition=Ready --timeout=2m \
    -l 'app.kubernetes.io/instance=cert-manager,app.kubernetes.io/name=webhook' \
    | indent

kubectl apply -f deploy/virtink_localhost:5001.yaml | indent

kubectl wait pod -n virtink-system --for=condition=Ready -l 'name=virt-controller' | indent

kubectl apply -f deploy/scheduler-deploy.yaml -f deploy/autoscaler-agent-deploy.yaml | indent

kubectl create secret generic vm-ssh --from-file=private-key=vm_image/ssh_id_rsa | indent

echo 'done initializing kubernetes specifics. setting up networking' >/dev/null

scripts/cluster-init-vxlan.sh 2>&1 | indent

echo 'all done' >/dev/null # already printed because of set -x
