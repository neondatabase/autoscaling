# Image URL to use all building/pushing image targets
IMG ?= controller:dev
IMG_RUNNER ?= runner:dev
IMG_VXLAN ?= vxlan-controller:dev
VM_EXAMPLE_SOURCE ?= postgres:15-alpine
VM_EXAMPLE_IMAGE ?= vm-postgres:15-alpine

# kernel for guests
VM_KERNEL_VERSION ?= "5.15.80"

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.23.0

# Get the currently used golang base path
GOPATH=$(shell go env GOPATH)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# Generate a number of things:
#  * Code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations
#  * WebhookConfiguration, ClusterRole, and CustomResourceDefinition objects
#  * Go client
.PHONY: generate
generate: ## Generate boilerplate DeepCopy methods, manifests, and Go client
	iidfile=$$(mktemp /tmp/iid-XXXXXX) && \
	docker build -f hack/Dockerfile.generate --iidfile $$iidfile . && \
	docker run --rm -v $$PWD:/go/src/github.com/neondatabase/neonvm -w /go/src/github.com/neondatabase/neonvm $$(cat $$iidfile) ./hack/generate.sh && \
	rm -rf $$iidfile
	go fmt ./...

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/common/crd/bases output:rbac:artifacts:config=config/common/rbac output:webhook:artifacts:config=config/common/webhook

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	# `go vet` requires gcc
	# ref https://github.com/golang/go/issues/56755
	CGO_ENABLED=0 go vet ./...

.PHONE: e2e
e2e: ## Run e2e kuttl tests
	kubectl kuttl test --config tests/e2e/kuttl-test.yaml

# TODO: fix/write tests
.PHONY: test
test: fmt vet envtest ## Run tests.
#	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: fmt vet ## Build controller binary.
	go build -o bin/controller       main.go
	go build -o bin/vxlan-controller tools/vxlan/controller/main.go
	go build -o bin/vxlan-ipam       tools/vxlan/ipam/main.go
	go build -o bin/runner           runner/main.go
	go build -o bin/vm-builder       tools/vm-builder/main.go

.PHONY: run
run: fmt vet ## Run a controller from your host.
	go run ./main.go

# If you wish built the controller image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64 ). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: build test ## Build docker image with the controller.
	docker build --build-arg VM_RUNNER_IMAGE=$(IMG_RUNNER) -t $(IMG) .
	docker build -t $(IMG_RUNNER) -f runner/Dockerfile .
	bin/vm-builder -src $(VM_EXAMPLE_SOURCE) -dst $(VM_EXAMPLE_IMAGE)
	docker build -t $(IMG_VXLAN) -f tools/vxlan/Dockerfile .

#.PHONY: docker-push
#docker-push: ## Push docker image with the controller.
#	docker push ${IMG}

# PLATFORMS defines the target platforms for  the controller image be build to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - able to use docker buildx . More info: https://docs.docker.com/build/buildx/
# - have enable BuildKit, More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image for your registry (i.e. if you do not inform a valid value via IMG=<myregistry/image:<tag>> than the export will fail)
# To properly provided solutions that supports more than one platform you should use this option.

#PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
#.PHONY: docker-buildx
#docker-buildx: test ## Build and push docker image for the controller for cross-platform support
#	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
#	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
#	- docker buildx create --name project-v3-builder
#	docker buildx use project-v3-builder
#	- docker buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross
#	- docker buildx rm project-v3-builder
#	rm Dockerfile.cross

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: kernel
kernel: ## Build linux kernel.
	rm -f hack/vmlinuz
	docker buildx build \
		--build-arg KERNEL_VERSION=$(VM_KERNEL_VERSION) \
		--output type=local,dest=hack/ \
		--platform linux/amd64 \
		--pull \
		--no-cache \
		--file hack/Dockerfile.kernel-builder .

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

DEPLOYTS := $(shell date +%s)
.PHONY: deploy
deploy: kind-load manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/common/controller              && $(KUSTOMIZE) edit set image controller=$(IMG)             && $(KUSTOMIZE) edit add annotation deploytime:$(DEPLOYTS) --force
	cd config/default-vxlan/vxlan-controller && $(KUSTOMIZE) edit set image vxlan-controller=$(IMG_VXLAN) && $(KUSTOMIZE) edit add annotation deploytime:$(DEPLOYTS) --force
	cd config/default-vxlan/vxlan-ipam       && $(KUSTOMIZE) edit set image vxlan-controller=$(IMG_VXLAN) && $(KUSTOMIZE) edit add annotation deploytime:$(DEPLOYTS) --force
	$(KUSTOMIZE) build config/default-vxlan/multus > neonvm-multus.yaml
	$(KUSTOMIZE) build config/default-vxlan > neonvm-vxlan.yaml
	cd config/common/controller              && $(KUSTOMIZE) edit remove annotation deploytime
	cd config/default-vxlan/vxlan-controller && $(KUSTOMIZE) edit remove annotation deploytime
	cd config/default-vxlan/vxlan-ipam       && $(KUSTOMIZE) edit remove annotation deploytime
	kubectl apply -f neonvm-multus.yaml
	kubectl -n kube-system rollout status daemonset kube-multus-ds
	kubectl apply -f neonvm-vxlan.yaml
	kubectl -n neonvm-system rollout status  deployment neonvm-vxlan-ipam
	kubectl -n neonvm-system rollout status  daemonset  neonvm-vxlan-controller
	kubectl -n neonvm-system rollout status  deployment neonvm-controller

.PHONY: deploy-controller
deploy-controller: kind-load manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/common/controller && $(KUSTOMIZE) edit set image controller=$(IMG) && $(KUSTOMIZE) edit add annotation deploytime:$(DEPLOYTS) --force
	$(KUSTOMIZE) build config/default > neonvm.yaml
	cd config/common/controller && $(KUSTOMIZE) edit remove annotation deploytime
	kubectl apply -f neonvm.yaml
	kubectl -n neonvm-system rollout status deployment neonvm-controller

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default-vxlan | kubectl delete --ignore-not-found=$(ignore-not-found) -f - || true
	$(KUSTOMIZE) build config/default-vxlan/multus | kubectl delete --ignore-not-found=$(ignore-not-found) -f - || true

##@ Local cluster

.PHONY: local-cluster
local-cluster:  ## Create local cluster by kind tool and prepared config
	kind create cluster --config hack/kind.yaml
	kubectl apply -f https://raw.githubusercontent.com/projectcalico/calico/v3.25.0/manifests/calico.yaml
	kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
	kubectl wait -n kube-system deployment calico-kube-controllers --for condition=Available --timeout -1s
	kubectl wait -n cert-manager deployment cert-manager --for condition=Available --timeout -1s

.PHONY: kind-load
kind-load: docker-build  ## Push docker images to the kind cluster.
	kind load docker-image $(IMG)
	kind load docker-image $(IMG_RUNNER)
	kind load docker-image $(IMG_VXLAN)
	kind load docker-image $(VM_EXAMPLE_IMAGE)

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUSTOMIZE ?= $(LOCALBIN)/kustomize
ENVTEST ?= $(LOCALBIN)/setup-envtest
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

## Tool Versions
KUSTOMIZE_VERSION ?= v4.5.7
CONTROLLER_TOOLS_VERSION ?= v0.9.2

KUSTOMIZE_INSTALL_SCRIPT ?= "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh"
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	test -s $(LOCALBIN)/kustomize || { curl -Ss $(KUSTOMIZE_INSTALL_SCRIPT) | bash -s -- $(subst v,,$(KUSTOMIZE_VERSION)) $(LOCALBIN); }

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	test -s $(LOCALBIN)/setup-envtest || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)


.PHONY: cert-manager
cert-manager: ## install cert-manager to cluster
	kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml

