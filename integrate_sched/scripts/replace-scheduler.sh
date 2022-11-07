#!/bin/sh
#
# Helper script to replace the currently-running autoscale-scheduler

set -eu -o pipefail

# Allow the script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"
cd .. # but all of the references are to things in the upper directory

set -x

pod="$(kubectl get pod -n kube-system -l name=autoscale-scheduler -o jsonpath='{.items[*].metadata.name}')"
if [ -n "$pod" ]; then
    kubectl delete pod -n kube-system "$pod"
fi

kubectl apply -f deploy/scheduler-deploy.yaml
