#!/bin/bash
#
# Generation script that's run inside of the generate.Dockerfile container.

set -eu -o pipefail

CODEGEN_PATH="$GOPATH/src/k8s.io/code-generator/kube_codegen.sh"

source "$CODEGEN_PATH"

# Required to allow git worktrees with non-root ownership on the host to work.
git config --global --add safe.directory "$GOPATH/src/github.com/neondatabase/autoscaling"

echo "Running gen_helpers ..."
kube::codegen::gen_helpers \
    --boilerplate neonvm/hack/boilerplate.go.txt \
    /go/src/github.com/neondatabase/autoscaling/neonvm/apis


echo "Running gen_client ..."
kube::codegen::gen_client \
    --output-dir "$GOPATH/src/github.com/neondatabase/autoscaling/neonvm/client" \
    --output-pkg github.com/neondatabase/autoscaling/neonvm/client \
    --with-watch \
    --boilerplate neonvm/hack/boilerplate.go.txt \
    /go/src/github.com/neondatabase/autoscaling/neonvm/apis

controller-gen object:headerFile="neonvm/hack/boilerplate.go.txt" paths="./neonvm/apis/..."

controller-gen rbac:roleName=manager-role crd webhook paths="./neonvm/..." \
    output:crd:artifacts:config=neonvm/config/crd/bases \
    output:rbac:artifacts:config=neonvm/config/rbac \
    output:webhook:artifacts:config=neonvm/config/webhook
