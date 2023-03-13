#!/usr/bin/env bash
#
# Helper script to initialize the kind cluster, once everything is built.

set -eu -o pipefail

# Allow this script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"
cd .. # but all of the references are to things in the upper directory

source './scripts-common.sh'

set -x

CLUSTER="autoscale-sched"

kind create cluster -n $CLUSTER --config=kind/config.yaml

kubectl apply -f deploy/flannel.yaml -f deploy/multus-daemonset.yaml -f deploy/cert-manager.yaml \
    | indent

kubectl wait pod -n cert-manager --for=condition=Ready --timeout=2m \
    -l 'app.kubernetes.io/instance=cert-manager,app.kubernetes.io/name=webhook' \
    | indent

kubectl apply -f deploy/neonvm.yaml | indent

kubectl wait deployment -n neonvm-system neonvm-controller --for=condition=Available=True | indent

for img in pg14-disk-test:dev kube-autoscale-scheduler:dev autoscaler-agent:dev; do
    kind load docker-image -n $CLUSTER $img | indent
done

kubectl apply -f deploy/autoscale-scheduler.yaml -f deploy/autoscaler-agent.yaml | indent

kubectl create secret generic vm-ssh --from-file=private-key=vm_image/ssh_id_rsa | indent

echo 'all done' >/dev/null # already printed because of set -x
