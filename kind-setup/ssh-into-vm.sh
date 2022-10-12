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
        "containers": [{
            "name": "'"ssh-$vm_name"'",
            "image": "alpine:3.16",
            "args": [
                "/bin/sh",
                "-c",
                "apk add openssh-client && ssh -i /ssh-private/id_rsa '"$SSH_OPTS"' root@'"$pod_ip"'"
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
                "secretName": "vm-ssh",
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

kubectl run ssh-"$vm_name" --rm --image=alpine --restart=Never -it --overrides="$CONTAINER_CFG"
