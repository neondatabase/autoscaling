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

kubectl cp "$(readlink autoscaler.py)" "$pod:/autoscaler.py" -c cloud-hypervisor
kubectl exec "$pod" -- apk add curl net-tools python3 busbox-extras
kubectl exec -it "$pod" -- python3 autoscaler.py "$pod_ip"
