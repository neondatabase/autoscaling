#!/usr/bin/env bash
#
# Convenience script to run a qemu monitor into a particular VM
#
# This script takes arguments from environment variables -- all optional. They are:
#
#   * VM_NAME - the vm.neon.tech/name of the VM. Optional if there is only one VM, otherwise must
#     be provided.
#   * RAW - if set to 1, socat instead of qmp-shell will be used to connect to the monitor.

set -e -o pipefail

# Allow the script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"

source '../scripts-common.sh'

# can't enable `set -u` for get_vm_name because we might not have $RAW
RAW="$RAW" # because -u isn't set yet, this sets RAW to "" if it's undefined

vm_name="$(get_vm_name)"
pod="$(kubectl get neonvm "$vm_name" -o jsonpath='{.status.podName}')"
set -u

trap "trap - SIGTERM && kill -- -$$" SIGINT SIGTERM EXIT
kubectl port-forward $pod 20184:20184 &
sleep 1
if [ "$RAW" = "1" ]; then
    socat -  TCP:localhost:20184
else
    qmp-shell localhost:20184
fi
