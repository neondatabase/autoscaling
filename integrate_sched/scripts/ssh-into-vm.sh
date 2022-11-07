#!/bin/bash
#
# Convenience script to ssh into a particular VM using the local private key that was generated for
# it.
#
# This script takes arguments from environment variables -- all optional. They are:
#
#   * VM_NAME - the virtink.io/vm.name of the VM. Optional if there is only one VM, otherwise must
#     be provided.
#   * SSHCLIENT_IP - the IP address of the ssh client on the vm-bridge network. Defaults to
#     10.77.77.213, but must be provided if there is already a pod using that IP address.
#   * NODE_NAME - if provided, a node to require that the pod is bound to. Used to allow access
#     to the VM when inter-node networking is not working.

set -e -o pipefail

# Allow the script to be run from outside this directory
cd -P -- "$(dirname -- "$0")"

source '../scripts-common.sh'

DEFAULT_SSHCLIENT_IP="10.77.77.213"
SSHCLIENT_IP="$( ( set -u; echo "$SSHCLIENT_IP" ) 2>/dev/null || echo "$DEFAULT_SSHCLIENT_IP" )"

NODE_NAME="$NODE_NAME" # because -u isn't set yet, this sets NODE_NAME to "" if it's undefined

# can't enable `set -u` for get_vm_name because we might not have $VM_NAME
vm_name="$(get_vm_name)"
set -u

SECRET_NAME='vm-ssh'

# Check if the secret vm-ssh exists; we need it to grab the private key.
echo "Checking secret exists..."
if ! { kubectl get secret "$SECRET_NAME" 1>&3; } 2>&1 3>/dev/null | indent; then
    exit 1
fi

echo "get vmPodName (vm_name = $vm_name)"
pod="$(kubectl get vm "$vm_name" -o jsonpath='{.status.vmPodName}')"
echo "get VM static ip (pod = $pod)"
vm_ip="$(get_vm_ip "$pod")"
echo "vm_ip = $vm_ip"

NAD_NAME="vm-bridge-ssh-$vm_name"
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
      "addresses": [{"address": "'"$SSHCLIENT_IP"'/24"}],
      "routes": [{"dst": "10.77.77.0/24"}]
    }
  }'"'"'
'

nad_file="ssh-nad-$NAD_NAME.tmp"

cleanup () {
    rm "$nad_file"
    kubectl delete network-attachment-definitions.k8s.cni.cncf.io "$NAD_NAME"
}

trap cleanup EXIT INT TERM

echo "$NAD" > "$nad_file"
kubectl apply -f "$nad_file"

if [ -n "$NODE_NAME" ]; then 
    NODE_SELECTOR='"nodeSelector": { "kubernetes.io/hostname": "'"$NODE_NAME"'" },'
else
    NODE_SELECTOR=""
fi

# Provide a manual configuration for the container so that we can pass through the ssh private key
#
# Note: this requires creating the 'vm-ssh' secret, as described in the readme.
# Note: the defaultMode below, is decimal 384 and therefore octal 600 
# Note: setting StrictHostKeyChecking=no disables the "authenticity of host" dialog
SSH_OPTS='-o StrictHostKeyChecking=no'
CONTAINER_CFG='
{
    "apiVersion": "v1",
    "spec": {
        '"$NODE_SELECTOR"'
        "containers": [{
            "name": "'"ssh-$vm_name"'",
            "image": "alpine:3.16",
            "args": [
                "/bin/sh",
                "-c",
                "apk add openssh-client && ssh -i /ssh-private/id_rsa '"$SSH_OPTS"' root@'"$vm_ip"' || echo dumping into ssh pod shell && /bin/ash"
            ],
            "stdin": true,
            "stdinOnce": true,
            "tty": true,
            "volumeMounts": [{
                "mountPath": "/ssh-private",
                "name": "ssh-id",
                "readOnly": true
            }]
        }],
        "volumes": [{
            "name": "ssh-id",
            "secret": {
                "secretName": "'"$SECRET_NAME"'",
                "optional": false,
                "defaultMode": 384,
                "items": [{
                    "key": "private-key",
                    "path": "id_rsa"
                }]
            }
        }]
    }
}'

ANNOTATIONS="--annotations=k8s.v1.cni.cncf.io/networks=$NAD_NAME"
kubectl run "ssh-$vm_name" --rm --image=alpine --restart=Never -it "$ANNOTATIONS" --overrides="$CONTAINER_CFG"
