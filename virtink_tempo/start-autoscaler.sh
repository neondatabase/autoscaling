#!/bin/bash

set -x

export VM_NAME=ubuntu-sergey
export POD=$(kubectl get vm $VM_NAME -o jsonpath='{.status.vmPodName}')
export VM_IP=$(kubectl get pod $POD -o jsonpath='{.status.podIP}')
kubectl cp autoscaler.py $POD:/autoscaler.py -c cloud-hypervisor
kubectl exec $POD -- apk add vim curl net-tools python3 busybox-extras
kubectl exec -it $POD -- python3 autoscaler.py 10.0.2.2
