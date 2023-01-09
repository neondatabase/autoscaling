#!/usr/bin/env bash
#
# Helper script that repeatedly deletes the currently-running scheduler, which the deployment will
# automatically replace. This is useful for things like testing scheduler reconnection.

set -eu -o pipefail

EVERY_SECS=10

while true; do
    echo -n "0s/$EVERY_SECS... "

    for i in $(seq "$EVERY_SECS"); do
        sleep 1s
        echo -en "\r${i}s/$EVERY_SECS... "
    done

    pod="$(kubectl get pod -n kube-system -l name=autoscale-scheduler -o jsonpath='{.items[*].metadata.name}')"
    if [ -z "$pod" ]; then
        echo "Couldn't find scheduler pod"
        exit 1
    fi
    kubectl delete pod -n kube-system "$pod"
done
