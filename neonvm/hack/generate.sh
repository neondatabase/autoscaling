#!/bin/bash
#
# Generation script that's run inside of the Dockerfile.generate container.

set -eu -o pipefail

bash $GOPATH/src/k8s.io/code-generator/generate-groups.sh "deepcopy,client,informer,lister" \
    github.com/neondatabase/neonvm/client \
    github.com/neondatabase/neonvm/apis \
    neonvm:v1 \
    --go-header-file hack/boilerplate.go.txt

controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./apis/..."
