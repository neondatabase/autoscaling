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

QUERY='select length(factorial(length(factorial(1223)::text)/2)::text);'
CMD='
set -eu -o pipefail
apk add postgresql-client
echo '"'$QUERY'"' > factorial.sql
echo "Running pgbench. Query:"
echo '"'   $QUERY'"'
pgbench -h '"$pod_ip"' -U postgres -c 20 -T 1000 -P 1 -f factorial.sql postgres
'

kubectl run pgbench-"$vm_name" --rm --image=alpine --restart=Never -it -- /bin/sh -c "$CMD"
