#!/bin/bash
#
# Generation script that's run inside of the Dockerfile.generate container.

set -eu -o pipefail

bash $GOPATH/src/k8s.io/code-generator/kube_codegen.sh "deepcopy,client,informer,lister" \
    github.com/neondatabase/autoscaling/neonvm/client \
    github.com/neondatabase/autoscaling/neonvm/apis \
    neonvm:v1 \
    --go-header-file neonvm/hack/boilerplate.go.txt

controller-gen object:headerFile="neonvm/hack/boilerplate.go.txt" paths="./neonvm/apis/..."

controller-gen rbac:roleName=manager-role crd webhook paths="./neonvm/..." \
    output:crd:artifacts:config=neonvm/config/crd/bases \
    output:rbac:artifacts:config=neonvm/config/rbac \
    output:webhook:artifacts:config=neonvm/config/webhook
