#!/bin/bash

set -x

export VM_NAME=ubuntu-sergey
export POD=$(kubectl get vm $VM_NAME -o jsonpath='{.status.vmPodName}')
export VM_IP=$(kubectl get pod $POD -o jsonpath='{.status.podIP}')
kubectl run ssh-$VM_NAME --rm --image=alpine --restart=Never -it -- /bin/sh -c "apk add openssh-client sshpass && sshpass -p password ssh -o StrictHostKeyChecking=no ubuntu@$VM_IP"
