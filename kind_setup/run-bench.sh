#!/bin/bash

set -e -o pipefail

# Allow the script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"
source 'scripts-common.sh'

DEFAULT_BENCH_IP="10.77.77.203"
BENCH_IP="$( ( set -u; echo "$BENCH_IP" ) 2>/dev/null || echo "$DEFAULT_BENCH_IP" )"

NODE_NAME="$NODE_NAME"

# can't enable `set -u` for get_vm_name because we might not have $VM_NAME
vm_name="$(get_vm_name)"
set -u

echo "get vmPodName (vm_name = $vm_name)"
pod="$(kubectl get vm "$vm_name" -o jsonpath='{.status.vmPodName}')"

echo "get VM static ip (pod = $pod)"
vm_ip="$(get_vm_ip "$pod")"
echo "vm_ip = '$vm_ip'"

QUERY='select length(factorial(length(factorial(1223)::text)/2)::text);'
CMD='
set -eu -o pipefail
apk add postgresql-client
echo '"'$QUERY'"' > factorial.sql
echo "Running pgbench. Query:"
echo '"'   $QUERY'"'
pgbench -h '"$vm_ip"' -U postgres -c 20 -T 1000 -P 1 -f factorial.sql postgres
'

NAD_NAME="vm-bridge-pgbench-$vm_name"
NAD='
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: '"$NAD_NAME"'
spec:
  config: '"'"'{
    "cniVersion": "0.3.1",
    "name": "overlay",
    "type": "bridge",
    "bridge": "vm-bridge",
    "isGateway": false,
    "isDefaultGateway": false,
    "ipam": {
      "type": "static",
      "addresses": [{"address": "'"$BENCH_IP"'/24"}],
      "routes": [{"dst": "10.77.77.0/24"}]
    }
  }'"'"'
'

nad_file="bench-nad-$NAD_NAME.tmp"

cleanup () {
    rm "$nad_file"
    kubectl delete network-attachment-definitions.k8s.cni.cncf.io "$NAD_NAME"
}

trap cleanup EXIT INT TERM

echo "$NAD" > "$nad_file"
kubectl apply -f "$nad_file"

ANNOTATIONS="--annotations=k8s.v1.cni.cncf.io/networks=$NAD_NAME"
if [ -z "$NODE_NAME" ]; then
    kubectl run "pgbench-$vm_name" --rm --image=alpine --restart=Never -it "$ANNOTATIONS" -- /bin/sh -c "$CMD"
else
    OVERRIDES='--overrides={
        "apiVersion": "v1",
        "spec": {
            "nodeSelector": { "kubernetes.io/hostname": "'"$NODE_NAME"'" }
        }
    }'

    kubectl run "pgbench-$vm_name" --rm --image=alpine --restart=Never -it "$ANNOTATIONS" "$OVERRIDES" -- /bin/sh -c "$CMD"
fi

