#!/usr/bin/bash

set -o xtrace
set -o errexit
set -o pipefail

vm_image/start-local-registry.sh
vm_image/build.sh

build/autoscale-scheduler/build.sh
build/autoscaler-agent/build.sh

scripts/download-cni.sh
scripts/download-deployments.sh

scripts/cluster-init.sh

kubectl apply -f vm-deploy.yaml

# check a few times for pods to exist
checks=20
while [ $checks -ne 0 ]; do
    POD_NAME=$(kubectl describe pods -l vm.neon.tech/name=postgres14-disk-test --show-events=false | grep -e '^Name:' | sed 's/Name: *p/p/')

    if [ -z "$POD_NAME" ]; then
        sleep .5s
        checks=$(($checks - 1))
    else
	echo "smoke test succeeded"
	break
    fi
done

if [ -z "$POD_NAME" ]; then
    echo "could not find a running vm pod"
    exit 1
fi

