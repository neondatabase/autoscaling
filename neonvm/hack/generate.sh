#!/bin/bash
#
# Generation script that's run inside of the Dockerfile.generate container.

set -eu -o pipefail

CODEGEN_PATH="$GOPATH/src/k8s.io/code-generator/kube_codegen.sh"

# apply a small patch to allow kube_codegen.sh to work with our file structure.
patch "$CODEGEN_PATH" neonvm/hack/kube_codegen.patch

source "$CODEGEN_PATH"

# Required to allow git worktrees with non-root ownership on the host to work.
git config --global --add safe.directory "$GOPATH/src/github.com/neondatabase/autoscaling"

# note: generation requires that "${output_base}/${input_pkg_root}" is valid, and *generally* the
# way to do that is that it's the same directory.
# The only way for that to be true is if $output_base is equal to "$GOPATH/src", which we make
# possible by the way we mount the repo from the host.

echo "Running gen_helpers ..."
kube::codegen::gen_helpers \
    --output-base "/go/src" \
    --input-pkg-root  github.com/neondatabase/autoscaling/neonvm/apis \
    --boilerplate neonvm/hack/boilerplate.go.txt

echo "Running gen_client ..."
kube::codegen::gen_client \
    --output-base "/go/src" \
    --input-pkg-root  github.com/neondatabase/autoscaling/neonvm/apis \
    --output-pkg-root github.com/neondatabase/autoscaling/neonvm/client \
    --with-watch \
    --boilerplate neonvm/hack/boilerplate.go.txt

controller-gen object:headerFile="neonvm/hack/boilerplate.go.txt" paths="./neonvm/apis/..."

controller-gen rbac:roleName=manager-role crd webhook paths="./neonvm/..." \
    output:crd:artifacts:config=neonvm/config/crd/bases \
    output:rbac:artifacts:config=neonvm/config/rbac \
    output:webhook:artifacts:config=neonvm/config/webhook
