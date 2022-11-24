#!/bin/bash
#
# Generation script that's run inside of the Dockerfile.generate container.

set -eu -o pipefail

bash $GOPATH/src/k8s.io/code-generator/generate-groups.sh "deepcopy,client,informer,lister" \
    github.com/neondatabase/neonvm/client \
    github.com/neondatabase/neonvm/apis \
    neonvm:v1 \
    --go-header-file hack/boilerplate.go.txt

go fmt ./client/...

# if buildvcs=false is not given, then we can run into issues with git worktrees.
GOFLAGS="-buildvcs=false" controller-gen rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
