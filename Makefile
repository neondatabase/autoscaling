# Image URL to use all building/pushing image targets
IMG_CONTROLLER ?= controller:dev
IMG_VXLAN_CONTROLLER ?= vxlan-controller:dev
IMG_RUNNER ?= runner:dev
IMG_DAEMON ?= daemon:dev
IMG_SCHEDULER ?= autoscale-scheduler:dev
IMG_AUTOSCALER_AGENT ?= autoscaler-agent:dev

# Shared base image for caching compiled dependencies.
# It's only used during image builds, so doesn't need to be pushed.
GO_BASE_IMG ?= autoscaling-go-base:dev

E2E_TESTS_VM_IMG ?= vm-postgres:15-bullseye
PG16_DISK_TEST_IMG ?= pg16-disk-test:dev

## Golang details (for local tooling)
GOARCH ?= $(shell go env GOARCH)
GOOS ?= $(shell go env GOOS)

# The target architecture for linux kernel. Possible values: amd64 or arm64.
# Any other supported by linux kernel architecture could be added by introducing new build step into neonvm/hack/kernel/Dockerfile.kernel-builder
UNAME_ARCH := $(shell uname -m)
ifeq ($(UNAME_ARCH),x86_64)
    TARGET_ARCH ?= amd64
else ifeq ($(UNAME_ARCH),aarch64)
    TARGET_ARCH ?= arm64
else
    $(error Unsupported architecture: $(UNAME_ARCH))
endif

# Get the currently used golang base path
GOPATH=$(shell go env GOPATH)
# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif
# Go 1.20 changed the handling of git worktrees:
# https://github.com/neondatabase/autoscaling/pull/130#issuecomment-1496276620
export GOFLAGS=-buildvcs=false

GOFUMPT_VERSION ?= v0.7.0

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

GIT_INFO := $(shell git describe --long --dirty)

# in CI environment use 'neonvm' as cluster name
# in other cases add $USER as cluster name suffix
# or fallback to 'neonvm' if $USER variable absent
ifdef CI
  CLUSTER_NAME = neonvm
else ifdef USER
  CLUSTER_NAME = neonvm-$(USER)
else
  CLUSTER_NAME = neonvm
endif

.PHONY: all
all: build lint

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
	# Use uid and gid of current user to avoid mismatched permissions
	set -e ; \
	rm -rf neonvm/client neonvm/apis/neonvm/v1/zz_generated.deepcopy.go
	iidfile=$$(mktemp /tmp/iid-XXXXXX) ; \
	docker build \
		--build-arg USER_ID=$(shell id -u $(USER)) \
		--build-arg GROUP_ID=$(shell id -g $(USER)) \
		--build-arg CONTROLLER_TOOLS_VERSION=$(CONTROLLER_TOOLS_VERSION) \
		--build-arg CODE_GENERATOR_VERSION=$(CODE_GENERATOR_VERSION) \
		--file neonvm/hack/Dockerfile.generate \
		--iidfile $$iidfile . ; \
	volumes=('--volume' "$$PWD:/go/src/github.com/neondatabase/autoscaling") ; \
	if [ -f .git ]; then \
		gitdir="$$(git rev-parse --git-common-dir)" ; \
		gitdir="$$(cd -P -- $$gitdir && pwd)" ; \
		volumes+=('--volume' "$$gitdir:$$gitdir") ; \
	fi ; \
	set -x ; \
	docker run --rm \
		"$${volumes[@]}" \
		--workdir /go/src/github.com/neondatabase/autoscaling \
		--user $(shell id -u $(USER)):$(shell id -g $(USER)) \
		$$(cat $$iidfile) \
		./neonvm/hack/generate.sh ; \
	docker rmi $$(cat $$iidfile) ; \
	rm -rf $$iidfile ; \
	go fmt ./...

.PHONY: fmt
fmt: ## Run go fmt against code.
	go run mvdan.cc/gofumpt@${GOFUMPT_VERSION} -w .

.PHONY: vet
vet: ## Run go vet against code.
	# `go vet` requires gcc
	# ref https://github.com/golang/go/issues/56755
	GOOS=linux CGO_ENABLED=0 go vet ./...


TESTARGS ?= ./...
.PHONY: test
test: vet envtest ## Run tests.
	# chmodding KUBEBUILDER_ASSETS dir to make it deletable by owner,
	# otherwise it fails with actions/checkout on self-hosted GitHub runners
	# 	ref: https://github.com/kubernetes-sigs/controller-runtime/pull/2245
	export KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)"; \
	find $(KUBEBUILDER_ASSETS) -type d -exec chmod 0755 {} \; ; \
	CGO_ENABLED=0 \
		go test $(TESTARGS) -coverprofile cover.out
	go tool cover -html=cover.out -o cover.html

##@ Build

.PHONY: build
build: vet bin/vm-builder ## Build all neonvm binaries.
	GOOS=linux go build -o bin/controller         neonvm-controller/cmd/*.go
	GOOS=linux go build -o bin/vxlan-controller   neonvm-vxlan-controller/cmd/*.go
	GOOS=linux go build -o bin/runner             neonvm-runner/cmd/*.go
	GOOS=linux go build -o bin/daemon             neonvm-daemon/cmd/*.go
	GOOS=linux go build -o bin/autoscaler-agent   autoscaler-agent/cmd/*.go
	GOOS=linux go build -o bin/scheduler          autoscale-scheduler/cmd/*.go

.PHONY: bin/vm-builder
bin/vm-builder: ## Build vm-builder binary.
	GOOS=linux CGO_ENABLED=0 go build -o bin/vm-builder -ldflags "-X main.Version=${GIT_INFO} -X main.NeonvmDaemonImage=${IMG_DAEMON}" vm-builder/main.go
.PHONY: run
run: vet ## Run a controller from your host.
	go run ./neonvm/main.go

.PHONY: lint
lint: ## Run golangci-lint against code.
	GOOS=linux golangci-lint run

# If you wish built the controller image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64 ). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: docker-build-controller docker-build-runner docker-build-daemon docker-build-vxlan-controller docker-build-autoscaler-agent docker-build-scheduler ## Build docker images for NeonVM controllers, NeonVM runner, autoscaler-agent, scheduler

.PHONY: docker-push
docker-push: docker-build ## Push docker images to docker registry
	docker push -q $(IMG_CONTROLLER)
	docker push -q $(IMG_RUNNER)
	docker push -q $(IMG_VXLAN_CONTROLLER)
	docker push -q $(IMG_SCHEDULER)
	docker push -q $(IMG_AUTOSCALER_AGENT)

.PHONY: docker-build-go-base
docker-build-go-base:
	docker build \
		--tag $(GO_BASE_IMG) \
		--file Dockerfile.go-base \
		.

.PHONY: docker-build-controller
docker-build-controller: docker-build-go-base ## Build docker image for NeonVM controller
	docker build \
		--tag $(IMG_CONTROLLER) \
		--build-arg GO_BASE_IMG=$(GO_BASE_IMG) \
		--build-arg VM_RUNNER_IMAGE=$(IMG_RUNNER) \
		--build-arg BUILDTAGS=$(if $(PRESERVE_RUNNER_PODS),nodelete) \
		--file neonvm-controller/Dockerfile \
		.

.PHONY: docker-build-runner
docker-build-runner: docker-build-go-base ## Build docker image for NeonVM runner
	docker build \
		--tag $(IMG_RUNNER) \
		--build-arg GO_BASE_IMG=$(GO_BASE_IMG) \
		--file neonvm-runner/Dockerfile \
		.

.PHONY: docker-build-daemon
docker-build-daemon: docker-build-go-base ## Build docker image for NeonVM daemon.
	docker build \
		--tag $(IMG_DAEMON) \
		--build-arg TARGET_ARCH=$(TARGET_ARCH) \
		--file neonvm-daemon/Dockerfile \
		.

.PHONY: docker-build-vxlan-controller
docker-build-vxlan-controller: docker-build-go-base ## Build docker image for NeonVM vxlan controller
	docker build \
		--tag $(IMG_VXLAN_CONTROLLER) \
		--build-arg GO_BASE_IMG=$(GO_BASE_IMG) \
		--build-arg TARGET_ARCH=$(TARGET_ARCH) \
		--file neonvm-vxlan-controller/Dockerfile \
		.

.PHONY: docker-build-autoscaler-agent
docker-build-autoscaler-agent: docker-build-go-base ## Build docker image for autoscaler-agent
	docker buildx build \
		--tag $(IMG_AUTOSCALER_AGENT) \
		--build-arg GO_BASE_IMG=$(GO_BASE_IMG) \
		--build-arg "GIT_INFO=$(GIT_INFO)" \
		--file autoscaler-agent/Dockerfile \
		.

.PHONY: docker-build-scheduler
docker-build-scheduler: docker-build-go-base ## Build docker image for (autoscaling) scheduler
	docker buildx build \
		--tag $(IMG_SCHEDULER) \
		--build-arg GO_BASE_IMG=$(GO_BASE_IMG) \
		--build-arg "GIT_INFO=$(GIT_INFO)" \
		--file autoscale-scheduler/Dockerfile \
		.

.PHONY: docker-build-examples
docker-build-examples: bin/vm-builder ## Build docker images for testing VMs
	./bin/vm-builder -src postgres:15-bullseye -dst $(E2E_TESTS_VM_IMG) -spec tests/e2e/image-spec.yaml -target-arch linux/$(TARGET_ARCH)

.PHONY: docker-build-pg16-disk-test
docker-build-pg16-disk-test: bin/vm-builder ## Build a VM image for testing
	./bin/vm-builder -src alpine:3.19 -dst $(PG16_DISK_TEST_IMG) -spec vm-examples/pg16-disk-test/image-spec.yaml -target-arch linux/$(TARGET_ARCH)

#.PHONY: docker-push
#docker-push: ## Push docker image with the controller.
#	docker push ${IMG_CONTROLLER}

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
#	- docker buildx build --push --platform=$(PLATFORMS) --tag ${IMG_CONTROLLER} -f Dockerfile.cross
#	- docker buildx rm project-v3-builder
#	rm Dockerfile.cross

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

# Build the kernel for the target architecture.
# The builder image platform is not specified because the kernel is built for the target architecture using crosscompilation.
# Target is generic and can be used for any supported architecture by specifying the TARGET_ARCH variable.
.PHONY: kernel
kernel: ## Build linux kernel.
	rm -f neonvm-kernel/vmlinuz; \
	linux_config=$$(ls neonvm-kernel/linux-config-*) \
	kernel_version=$${linux_config##*-} \
	iidfile=$$(mktemp /tmp/iid-XXXXXX); \
	trap "rm $$iidfile" EXIT; \
	docker buildx build \
	    --build-arg KERNEL_VERSION=$$kernel_version \
		--target "kernel_${TARGET_ARCH}" \
		--pull \
		--load \
		--iidfile $$iidfile \
		--file neonvm-kernel/Dockerfile.kernel-builder \
		neonvm-kernel; \
	id=$$(docker create $$(cat $$iidfile)); \
	docker cp $$id:/vmlinuz neonvm-kernel/vmlinuz; \
	docker rm -f $$id

.PHONY: check-local-context
check-local-context: ## Asserts that the current kubectl context is pointing at k3d or kind, to avoid accidentally applying to prod
	@if [ "$$($(KUBECTL) config current-context)" != 'k3d-$(CLUSTER_NAME)' ] && [ "$$($(KUBECTL) config current-context)" != 'kind-$(CLUSTER_NAME)' ]; then echo "kubectl context is not pointing to local k3d or kind cluster (must be k3d-$(CLUSTER_NAME) or kind-$(CLUSTER_NAME))"; exit 1; fi

.PHONY: install
install: check-local-context kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build neonvm/config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: check-local-context kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build neonvm/config/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

BUILDTS := $(shell date +%s)
RENDERED ?= $(shell pwd)/rendered_manifests
$(RENDERED):
	mkdir -p $(RENDERED)
.PHONY: render-manifests
render-manifests: $(RENDERED) kustomize
	# Prepare:
	cd neonvm-controller && $(KUSTOMIZE) edit set image controller=$(IMG_CONTROLLER) && $(KUSTOMIZE) edit add annotation buildtime:$(BUILDTS) --force
	cd neonvm-vxlan-controller && $(KUSTOMIZE) edit set image vxlan-controller=$(IMG_VXLAN_CONTROLLER) && $(KUSTOMIZE) edit add annotation buildtime:$(BUILDTS) --force
	cd neonvm-runner/image-loader/bases && $(KUSTOMIZE) edit set image runner=$(IMG_RUNNER) && $(KUSTOMIZE) edit add annotation buildtime:$(BUILDTS) --force
	cd autoscale-scheduler && $(KUSTOMIZE) edit set image autoscale-scheduler=$(IMG_SCHEDULER) && $(KUSTOMIZE) edit add annotation buildtime:$(BUILDTS) --force
	cd autoscaler-agent && $(KUSTOMIZE) edit set image autoscaler-agent=$(IMG_AUTOSCALER_AGENT) && $(KUSTOMIZE) edit add annotation buildtime:$(BUILDTS) --force
	# Build:
	$(KUSTOMIZE) build neonvm/config/whereabouts > $(RENDERED)/whereabouts.yaml
	$(KUSTOMIZE) build neonvm/config/multus-aks > $(RENDERED)/multus-aks.yaml
	$(KUSTOMIZE) build neonvm/config/multus-eks > $(RENDERED)/multus-eks.yaml
	$(KUSTOMIZE) build neonvm/config/multus > $(RENDERED)/multus.yaml
	$(KUSTOMIZE) build neonvm/config > $(RENDERED)/neonvm.yaml
	$(KUSTOMIZE) build neonvm-controller > $(RENDERED)/neonvm-controller.yaml
	$(KUSTOMIZE) build neonvm-vxlan-controller > $(RENDERED)/neonvm-vxlan-controller.yaml
	$(KUSTOMIZE) build neonvm-runner/image-loader > $(RENDERED)/neonvm-runner-image-loader.yaml
	$(KUSTOMIZE) build autoscale-scheduler > $(RENDERED)/autoscale-scheduler.yaml
	$(KUSTOMIZE) build autoscaler-agent > $(RENDERED)/autoscaler-agent.yaml
	# Cleanup:
	cd neonvm-controller && $(KUSTOMIZE) edit set image controller=controller:dev && $(KUSTOMIZE) edit remove annotation buildtime --ignore-non-existence
	cd neonvm-vxlan-controller && $(KUSTOMIZE) edit set image vxlan-controller=vxlan-controller:dev && $(KUSTOMIZE) edit remove annotation buildtime --ignore-non-existence
	cd neonvm-runner/image-loader/bases && $(KUSTOMIZE) edit set image runner=runner:dev && $(KUSTOMIZE) edit remove annotation buildtime --ignore-non-existence
	cd autoscale-scheduler && $(KUSTOMIZE) edit set image autoscale-scheduler=autoscale-scheduler:dev && $(KUSTOMIZE) edit remove annotation buildtime --ignore-non-existence
	cd autoscaler-agent && $(KUSTOMIZE) edit set image autoscaler-agent=autoscaler-agent:dev && $(KUSTOMIZE) edit remove annotation buildtime --ignore-non-existence

render-release: $(RENDERED) kustomize
	# Prepare:
	cd neonvm-controller && $(KUSTOMIZE) edit set image controller=$(IMG_CONTROLLER)
	cd neonvm-vxlan-controller && $(KUSTOMIZE) edit set image vxlan-controller=$(IMG_VXLAN_CONTROLLER)
	cd neonvm-runner/image-loader/bases && $(KUSTOMIZE) edit set image runner=$(IMG_RUNNER)
	cd autoscale-scheduler && $(KUSTOMIZE) edit set image autoscale-scheduler=$(IMG_SCHEDULER)
	cd autoscaler-agent && $(KUSTOMIZE) edit set image autoscaler-agent=$(IMG_AUTOSCALER_AGENT)
	# Build:
	$(KUSTOMIZE) build neonvm/config/whereabouts > $(RENDERED)/whereabouts.yaml
	$(KUSTOMIZE) build neonvm/config/multus-aks > $(RENDERED)/multus-aks.yaml
	$(KUSTOMIZE) build neonvm/config/multus-eks > $(RENDERED)/multus-eks.yaml
	$(KUSTOMIZE) build neonvm/config/multus > $(RENDERED)/multus.yaml
	$(KUSTOMIZE) build neonvm/config > $(RENDERED)/neonvm.yaml
	$(KUSTOMIZE) build neonvm-controller > $(RENDERED)/neonvm-controller.yaml
	$(KUSTOMIZE) build neonvm-vxlan-controller > $(RENDERED)/neonvm-vxlan-controller.yaml
	$(KUSTOMIZE) build neonvm-runner/image-loader > $(RENDERED)/neonvm-runner-image-loader.yaml
	$(KUSTOMIZE) build autoscale-scheduler > $(RENDERED)/autoscale-scheduler.yaml
	$(KUSTOMIZE) build autoscaler-agent > $(RENDERED)/autoscaler-agent.yaml
	# Cleanup:
	cd neonvm-controller && $(KUSTOMIZE) edit set image controller=controller:dev
	cd neonvm-vxlan-controller && $(KUSTOMIZE) edit set image vxlan-controller=vxlan-controller:dev
	cd neonvm-runner/image-loader/bases && $(KUSTOMIZE) edit set image runner=runner:dev
	cd autoscale-scheduler && $(KUSTOMIZE) edit set image autoscale-scheduler=autoscale-scheduler:dev
	cd autoscaler-agent && $(KUSTOMIZE) edit set image autoscaler-agent=autoscaler-agent:dev

.PHONY: deploy
deploy: check-local-context docker-build load-images render-manifests kubectl ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	$(KUBECTL) apply -f $(RENDERED)/multus.yaml
	$(KUBECTL) -n kube-system rollout status daemonset kube-multus-ds
	$(KUBECTL) apply -f $(RENDERED)/whereabouts.yaml
	$(KUBECTL) -n kube-system rollout status daemonset whereabouts
	$(KUBECTL) apply -f $(RENDERED)/neonvm-runner-image-loader.yaml
	$(KUBECTL) -n neonvm-system rollout status daemonset neonvm-runner-image-loader
	$(KUBECTL) apply -f $(RENDERED)/neonvm.yaml
	$(KUBECTL) -n neonvm-system rollout status daemonset neonvm-device-plugin
	$(KUBECTL) apply -f $(RENDERED)/neonvm-controller.yaml
	$(KUBECTL) -n neonvm-system rollout status deployment neonvm-controller
	$(KUBECTL) apply -f $(RENDERED)/neonvm-vxlan-controller.yaml
	$(KUBECTL) -n neonvm-system rollout status daemonset neonvm-vxlan-controller
	# NB: typical upgrade path requires updated scheduler before autoscaler-agents.
	$(KUBECTL) apply -f $(RENDERED)/autoscale-scheduler.yaml
	$(KUBECTL) -n kube-system rollout status deployment autoscale-scheduler
	$(KUBECTL) apply -f $(RENDERED)/autoscaler-agent.yaml
	$(KUBECTL) -n kube-system rollout status daemonset autoscaler-agent

.PHONY: load-images
load-images: check-local-context kubectl kind k3d ## Push docker images to the local kind/k3d cluster
	@if [ $$($(KUBECTL) config current-context) = k3d-$(CLUSTER_NAME) ]; then make k3d-load;  fi
	@if [ $$($(KUBECTL) config current-context) = kind-$(CLUSTER_NAME) ];  then make kind-load; fi

.PHONY: load-example-vms
load-example-vms: check-local-context kubectl kind k3d ## Load the testing VM image to the kind/k3d cluster.
	@if [ $$($(KUBECTL) config current-context) = k3d-$(CLUSTER_NAME) ]; then $(K3D) image import $(E2E_TESTS_VM_IMG) --cluster $(CLUSTER_NAME) --mode direct; fi
	@if [ $$($(KUBECTL) config current-context) = kind-$(CLUSTER_NAME) ]; then $(KIND) load docker-image $(E2E_TESTS_VM_IMG) --name $(CLUSTER_NAME); fi

.PHONY: example-vms
example-vms: docker-build-examples load-example-vms ## Build and push the testing VM images to the kind/k3d cluster.

.PHONY: load-pg16-disk-test
load-pg16-disk-test: check-local-context kubectl kind k3d ## Load the pg16-disk-test VM image to the kind/k3d cluster.
	@if [ $$($(KUBECTL) config current-context) = k3d-$(CLUSTER_NAME) ]; then $(K3D) image import $(PG16_DISK_TEST_IMG) --cluster $(CLUSTER_NAME) --mode direct; fi
	@if [ $$($(KUBECTL) config current-context) = kind-$(CLUSTER_NAME) ]; then $(KIND) load docker-image $(PG16_DISK_TEST_IMG) --name $(CLUSTER_NAME); fi

.PHONY: pg16-disk-test
pg16-disk-test: docker-build-pg16-disk-test load-pg16-disk-test ## Build and push the pg16-disk-test VM test image to the kind/k3d cluster.

.PHONY: kind-load
kind-load: kind # Push docker images to the kind cluster.
	$(KIND) load docker-image \
		$(IMG_CONTROLLER) \
		$(IMG_RUNNER) \
		$(IMG_VXLAN_CONTROLLER) \
		$(IMG_SCHEDULER) \
		$(IMG_AUTOSCALER_AGENT) \
		--name $(CLUSTER_NAME)

.PHONY: k3d-load
k3d-load: k3d # Push docker images to the k3d cluster.
	$(K3D) image import \
		$(IMG_CONTROLLER) \
		$(IMG_RUNNER) \
		$(IMG_VXLAN_CONTROLLER) \
		$(IMG_SCHEDULER) \
		$(IMG_AUTOSCALER_AGENT) \
		--cluster $(CLUSTER_NAME) --mode direct

##@ End-to-End tests

.PHONE: e2e-tools
e2e-tools: k3d kind kubectl kuttl python-init ## Donwnload tools for e2e tests locally if necessary.

.PHONE: e2e
e2e: check-local-context e2e-tools ## Run e2e kuttl tests
	$(KUTTL) test --config tests/e2e/kuttl-test.yaml $(if $(CI),--skip-delete)
	rm -f kubeconfig

##@ Local kind cluster

.PHONY: kind-setup
kind-setup: kind kubectl ## Create local cluster by kind tool and prepared config
	$(KIND) create cluster --name $(CLUSTER_NAME) --config kind/config.yaml
	$(KUBECTL) --context kind-$(CLUSTER_NAME) apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
	$(KUBECTL) --context kind-$(CLUSTER_NAME) -n cert-manager rollout status deployment cert-manager
	$(KUBECTL) --context kind-$(CLUSTER_NAME) apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
	$(KUBECTL) --context kind-$(CLUSTER_NAME) patch -n kube-system deployment metrics-server --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
	$(KUBECTL) --context kind-$(CLUSTER_NAME) -n kube-system rollout status deployment metrics-server

.PHONY: kind-destroy
kind-destroy: kind ## Destroy local kind cluster
	$(KIND) delete cluster --name $(CLUSTER_NAME)

##@ Local k3d cluster

# K3D_FIX_MOUNTS=1 used to allow foreign CNI (cilium, multus and so on), https://github.com/k3d-io/k3d/pull/1268
.PHONY: k3d-setup
k3d-setup: k3d kubectl ## Create local cluster by k3d tool and prepared config
	K3D_FIX_MOUNTS=1 $(K3D) cluster create $(CLUSTER_NAME) --config k3d/config.yaml $(if $(USE_REGISTRIES_FILE),--registry-config=k3d/registries.yaml)
	$(KUBECTL) --context k3d-$(CLUSTER_NAME) apply -f k3d/cilium.yaml
	$(KUBECTL) --context k3d-$(CLUSTER_NAME) -n kube-system rollout status daemonset  cilium
	$(KUBECTL) --context k3d-$(CLUSTER_NAME) -n kube-system rollout status deployment cilium-operator
	$(KUBECTL) --context k3d-$(CLUSTER_NAME) apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
	$(KUBECTL) --context k3d-$(CLUSTER_NAME) -n cert-manager rollout status deployment cert-manager

.PHONY: k3d-destroy
k3d-destroy: k3d ## Destroy local k3d cluster
	$(K3D) cluster delete $(CLUSTER_NAME)

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tools
KUSTOMIZE ?= $(LOCALBIN)/kustomize
# same as used in kubectl v1.28.x ; see https://github.com/kubernetes-sigs/kustomize/tree/master?tab=readme-ov-file#kubectl-integration
KUSTOMIZE_VERSION ?= v5.1.1

ENVTEST ?= $(LOCALBIN)/setup-envtest
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
# List of available versions: https://storage.googleapis.com/kubebuilder-tools
ENVTEST_K8S_VERSION = 1.28.3

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
# We went ahead of k8s 1.28 with controller-tools v0.14.0 (which depends on k8s 1.29) to unblock go 1.22 upgrade.
# It should be *relatively* safe as the only changes from this controller-tools version are description in CRD.
# Once we upgrade to k8s 1.29, there's no need to change CONTROLLER_TOOLS_VERSION, and this 3 lines can be removed.
#
# k8s deps @ 1.29.0 https://github.com/kubernetes-sigs/controller-tools/blob/<version>/go.mod
CONTROLLER_TOOLS_VERSION ?= v0.14.0

CODE_GENERATOR_VERSION ?= v0.28.12

KUTTL ?= $(LOCALBIN)/kuttl
# k8s deps @ 1.28.3
KUTTL_VERSION ?= v0.16.0
ifeq ($(GOARCH), arm64)
    KUTTL_ARCH = arm64
else ifeq ($(GOARCH), amd64)
    KUTTL_ARCH = x86_64
else
    $(error Unsupported architecture: $(GOARCH))
endif
KUBECTL ?= $(LOCALBIN)/kubectl
KUBECTL_VERSION ?= v1.29.10

KIND ?= $(LOCALBIN)/kind
# https://github.com/kubernetes-sigs/kind/releases/tag/v0.23.0, supports k8s up to 1.30
KIND_VERSION ?= v0.23.0

K3D ?= $(LOCALBIN)/k3d
# k8s deps in go.mod @ v1.29.4 (nb: binary, separate from images)
K3D_VERSION ?= v5.7.4

## Install tools
KUSTOMIZE_INSTALL_SCRIPT ?= "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh"
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	@test -s $(LOCALBIN)/kustomize || { curl -Ss $(KUSTOMIZE_INSTALL_SCRIPT) | bash -s -- $(subst v,,$(KUSTOMIZE_VERSION)) $(LOCALBIN); }

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	@test -s $(LOCALBIN)/setup-envtest || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	@test -s $(LOCALBIN)/controller-gen || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): $(LOCALBIN)
	@test -s $(LOCALBIN)/kind || { curl -sfSLo $(KIND) https://kind.sigs.k8s.io/dl/$(KIND_VERSION)/kind-$(GOOS)-$(GOARCH) && chmod +x $(KIND); }

.PHONY: kubectl
kubectl: $(KUBECTL) ## Download kubectl locally if necessary.
$(KUBECTL): $(LOCALBIN)
	@test -s $(LOCALBIN)/kubectl || { curl -sfSLo $(KUBECTL) https://dl.k8s.io/release/$(KUBECTL_VERSION)/bin/$(GOOS)/$(GOARCH)/kubectl && chmod +x $(KUBECTL); }

.PHONY: kuttl
kuttl: $(KUTTL) ## Download kuttl locally if necessary.
$(KUTTL): $(LOCALBIN)
	test -s $(LOCALBIN)/kuttl || { curl -sfSLo $(KUTTL) https://github.com/kudobuilder/kuttl/releases/download/$(KUTTL_VERSION)/kubectl-kuttl_$(subst v,,$(KUTTL_VERSION))_$(GOOS)_$(KUTTL_ARCH) && chmod +x $(KUTTL); }

.PHONY: k3d
k3d: $(K3D) ## Download k3d locally if necessary.
$(K3D): $(LOCALBIN)
	@test -s $(LOCALBIN)/k3d || { curl -sfSLo $(K3D)  https://github.com/k3d-io/k3d/releases/download/$(K3D_VERSION)/k3d-$(GOOS)-$(GOARCH) && chmod +x $(K3D); }

.PHONY: cert-manager
cert-manager: check-local-context kubectl ## install cert-manager to cluster
	$(KUBECTL) apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml

.PHONY: python-init
python-init:
	python3 -m venv tests/e2e/.venv
	tests/e2e/.venv/bin/pip install -r requirements.txt
