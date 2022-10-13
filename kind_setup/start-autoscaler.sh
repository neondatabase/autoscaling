#!/bin/bash

set -e -o pipefail

# Allow the script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"

source 'scripts-common.sh'

# can't enable `set -u` for get_vm_name because we might not have $VM_NAME
vm_name="$(get_vm_name)"
set -u

echo "get vmPodName (vm_name = $vm_name)"
pod="$(kubectl get vm "$vm_name" -o jsonpath='{.status.vmPodName}')"
echo "get pod ip (pod = $pod)"
pod_ip="$(kubectl get pod "$pod" -o jsonpath='{.status.podIP}')"
echo "pod_ip = $pod_ip"

kubectl cp -c cloud-hypervisor autoscaler.py "$pod:/autoscaler.py"
kubectl exec -c cloud-hypervisor "$pod" -- apk add vim curl net-tools python3 busybox-extras
# TODO: why is it 10.0.2.2 ?
kubectl exec -c cloud-hypervisor -it "$pod" -- python3 autoscaler.py 10.0.2.2
