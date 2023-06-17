# Image URL to use all building/pushing image targets
IMG_CONTROLLER ?= controller:dev
IMG_RUNNER ?= runner:dev
IMG_VXLAN ?= vxlan-controller:dev

# Autoscaler related images
AUTOSCALER_SCHEDULER_IMG ?= autoscale-scheduler:dev
AUTOSCALER_AGENT_IMG ?= autoscaler-agent:dev
VM_INFORMANT_IMG ?= vm-informant:dev
E2E_TESTS_VM_IMG ?= vm-postgres:15-bullseye
PG14_DISK_TEST_IMG ?= pg14-disk-test:dev

# kernel for guests
VM_KERNEL_VERSION ?= "5.15.80"

## Golang details
GOARCH ?= $(shell go env GOARCH)
GOOS ?= $(shell go env GOOS)
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

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

GIT_INFO := $(shell git describe --long --dirty)

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
	# Use uid and gid of current user to avoid mismatched permissions
	iidfile=$$(mktemp /tmp/iid-XXXXXX) && \
	docker build \
		--build-arg USER_ID=$(shell id -u $(USER)) \
		--build-arg GROUP_ID=$(shell id -g $(USER)) \
		--build-arg CONTROLLER_TOOLS_VERSION=$(CONTROLLER_TOOLS_VERSION) \
		--build-arg CODE_GENERATOR_VERSION=$(CODE_GENERATOR_VERSION) \
		--file neonvm/hack/Dockerfile.generate \
		--iidfile $$iidfile . && \
	docker run --rm \
		--volume $$PWD:/go/src/github.com/neondatabase/autoscaling \
		--workdir /go/src/github.com/neondatabase/autoscaling \
		--user $(shell id -u $(USER)):$(shell id -g $(USER)) \
		$$(cat $$iidfile) \
		./neonvm/hack/generate.sh && \
	docker rmi $$(cat $$iidfile)
	rm -rf $$iidfile
	go fmt ./...

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./neonvm/..." \
		output:crd:artifacts:config=neonvm/config/common/crd/bases \
		output:rbac:artifacts:config=neonvm/config/common/rbac \
		output:webhook:artifacts:config=neonvm/config/common/webhook

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	# `go vet` requires gcc
	# ref https://github.com/golang/go/issues/56755
	CGO_ENABLED=0 go vet ./...


.PHONY: test
test: fmt vet envtest ## Run tests.
	# chmodding KUBEBUILDER_ASSETS dir to make it deletable by owner,
	# otherwise it fails with actions/checkout on self-hosted GitHub runners
	# 	ref: https://github.com/kubernetes-sigs/controller-runtime/pull/2245
	export KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)"; \
	find $(KUBEBUILDER_ASSETS) -type d -exec chmod 0755 {} \; ; \
	CGO_ENABLED=0 \
		go test ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: fmt vet bin/vm-builder bin/vm-builder-generic ## Build all neonvm binaries.
	go build -o bin/controller       neonvm/main.go
	go build -o bin/vxlan-controller neonvm/tools/vxlan/controller/main.go
	go build -o bin/runner           neonvm/runner/main.go

.PHONY: bin/vm-builder
bin/vm-builder: ## Build vm-builder binary.
	CGO_ENABLED=0 go build -o bin/vm-builder -ldflags "-X main.Version=${GIT_INFO} -X main.VMInformant=${VM_INFORMANT_IMG}" neonvm/tools/vm-builder/main.go

.PHONY: bin/vm-builder-generic
bin/vm-builder-generic: ## Build vm-builder-generic binary.
	CGO_ENABLED=0 go build -o bin/vm-builder-generic  neonvm/tools/vm-builder-generic/main.go

.PHONY: run
run: fmt vet ## Run a controller from your host.
	go run ./neonvm/main.go

.PHONY: vm-informant
vm-informant: ## Build vm-informant image
	docker buildx build \
		--tag $(VM_INFORMANT_IMG) \
		--load \
		--build-arg GIT_INFO=$(GIT_INFO) \
		--file build/vm-informant/Dockerfile \
		.

# If you wish built the controller image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64 ). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: docker-build-controller docker-build-runner docker-build-vxlan-controller docker-build-autoscaler-agent docker-build-scheduler vm-informant ## Build docker images for NeonVM controllers, NeonVM runner, autoscaler-agent, and scheduler

.PHONY: docker-push
docker-push: docker-build ## Push docker images to docker registry
	docker push -q $(IMG_CONTROLLER)
	docker push -q $(IMG_RUNNER)
	docker push -q $(IMG_VXLAN)
	docker push -q $(AUTOSCALER_SCHEDULER_IMG)
	docker push -q $(AUTOSCALER_AGENT_IMG)
	docker push -q $(VM_INFORMANT_IMG)

.PHONY: docker-build-controller
docker-build-controller: ## Build docker image for NeonVM controller
	docker build --build-arg VM_RUNNER_IMAGE=$(IMG_RUNNER) -t $(IMG_CONTROLLER) -f neonvm/Dockerfile .

.PHONY: docker-build-runner
docker-build-runner: ## Build docker image for NeonVM runner
	docker build -t $(IMG_RUNNER) -f neonvm/runner/Dockerfile .

.PHONY: docker-build-vxlan-controller
docker-build-vxlan-controller: ## Build docker image for NeonVM vxlan controller
	docker build -t $(IMG_VXLAN) -f neonvm/tools/vxlan/Dockerfile .

.PHONY: docker-build-autoscaler-agent
docker-build-autoscaler-agent: ## Build docker image for autoscaler-agent
	docker buildx build \
		--tag $(AUTOSCALER_AGENT_IMG) \
		--load \
		--build-arg "GIT_INFO=$(GIT_INFO)" \
		--file build/autoscaler-agent/Dockerfile \
		.

.PHONY: docker-build-scheduler
docker-build-scheduler: ## Build docker image for (autoscaling) scheduler
	docker buildx build \
		--tag $(AUTOSCALER_SCHEDULER_IMG) \
		--load \
		--build-arg "GIT_INFO=$(GIT_INFO)" \
		--file build/autoscale-scheduler/Dockerfile \
		.

.PHONY: docker-build-examples
docker-build-examples: vm-informant bin/vm-builder ## Build docker images for testing VMs
	./bin/vm-builder -src postgres:15-bullseye -dst $(E2E_TESTS_VM_IMG)

.PHONY: docker-build-pg14-disk-test
docker-build-pg14-disk-test: vm-informant bin/vm-builder-generic ## Build a VM image for testing
	if [ -a 'vm-examples/pg14-disk-test/ssh_id_rsa' ]; then \
	    echo "Skipping keygen because 'ssh_id_rsa' already exists"; \
	else \
	    echo "Generating new keypair with empty passphrase ..."; \
	    ssh-keygen -t rsa -N '' -f 'vm-examples/pg14-disk-test/ssh_id_rsa'; \
	    chmod uga+rw 'vm-examples/pg14-disk-test/ssh_id_rsa' 'vm-examples/pg14-disk-test/ssh_id_rsa.pub'; \
	fi

	docker buildx build \
		--tag tmp-$(PG14_DISK_TEST_IMG) \
		--load \
		--file vm-examples/pg14-disk-test/Dockerfile.vmdata \
		vm-examples/pg14-disk-test/
	./bin/vm-builder-generic -src tmp-$(PG14_DISK_TEST_IMG) -use-inittab -dst $(PG14_DISK_TEST_IMG)

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

.PHONY: kernel
kernel: ## Build linux kernel.
	rm -f neonvm/hack/vmlinuz
	docker buildx build \
		--build-arg KERNEL_VERSION=$(VM_KERNEL_VERSION) \
		--output type=local,dest=neonvm/hack/ \
		--platform linux/amd64 \
		--pull \
		--no-cache \
		--file neonvm/hack/Dockerfile.kernel-builder \
		neonvm/hack

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build neonvm/config/common/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build neonvm/config/common/crd | kubectl delete --ignore-not-found=$(ignore-not-found) -f -

BUILDTS := $(shell date +%s)
RENDERED ?= $(shell pwd)/rendered_manifests
$(RENDERED):
	mkdir -p $(RENDERED)
.PHONY: render-manifests
render-manifests: $(RENDERED) kustomize
	# Prepare:
	cd neonvm/config/common/controller && $(KUSTOMIZE) edit set image controller=$(IMG_CONTROLLER) && $(KUSTOMIZE) edit add annotation buildtime:$(BUILDTS) --force
	cd neonvm/config/default-vxlan/vxlan-controller && $(KUSTOMIZE) edit set image vxlan-controller=$(IMG_VXLAN) && $(KUSTOMIZE) edit add annotation buildtime:$(BUILDTS) --force
	cd deploy/scheduler && $(KUSTOMIZE) edit set image autoscale-scheduler=$(AUTOSCALER_SCHEDULER_IMG) && $(KUSTOMIZE) edit add annotation buildtime:$(BUILDTS) --force
	cd deploy/agent && $(KUSTOMIZE) edit set image autoscaler-agent=$(AUTOSCALER_AGENT_IMG) && $(KUSTOMIZE) edit add annotation buildtime:$(BUILDTS) --force
	# Build:
	$(KUSTOMIZE) build neonvm/config/default-vxlan/whereabouts > $(RENDERED)/whereabouts.yaml
	$(KUSTOMIZE) build neonvm/config/default-vxlan/multus-eks > $(RENDERED)/multus-eks.yaml
	$(KUSTOMIZE) build neonvm/config/default-vxlan/multus > $(RENDERED)/multus.yaml
	$(KUSTOMIZE) build neonvm/config/default-vxlan > $(RENDERED)/neonvm.yaml
	$(KUSTOMIZE) build deploy/scheduler > $(RENDERED)/autoscale-scheduler.yaml
	$(KUSTOMIZE) build deploy/agent > $(RENDERED)/autoscaler-agent.yaml
	# Cleanup:
	cd neonvm/config/common/controller && $(KUSTOMIZE) edit set image controller=controller:dev && $(KUSTOMIZE) edit remove annotation buildtime --ignore-non-existence
	cd neonvm/config/default-vxlan/vxlan-controller && $(KUSTOMIZE) edit set image vxlan-controller=vxlan-controller:dev && $(KUSTOMIZE) edit remove annotation buildtime --ignore-non-existence
	cd deploy/scheduler && $(KUSTOMIZE) edit set image autoscale-scheduler=autoscale-scheduler:dev && $(KUSTOMIZE) edit remove annotation buildtime --ignore-non-existence
	cd deploy/agent && $(KUSTOMIZE) edit set image autoscaler-agent=autoscaler-agent:dev && $(KUSTOMIZE) edit remove annotation buildtime --ignore-non-existence

render-release: $(RENDERED) kustomize
	# Prepare:
	cd neonvm/config/common/controller && $(KUSTOMIZE) edit set image controller=$(IMG_CONTROLLER)
	cd neonvm/config/default-vxlan/vxlan-controller && $(KUSTOMIZE) edit set image vxlan-controller=$(IMG_VXLAN)
	cd deploy/scheduler && $(KUSTOMIZE) edit set image autoscale-scheduler=$(AUTOSCALER_SCHEDULER_IMG)
	cd deploy/agent && $(KUSTOMIZE) edit set image autoscaler-agent=$(AUTOSCALER_AGENT_IMG)
	# Build:
	$(KUSTOMIZE) build neonvm/config/default-vxlan/whereabouts > $(RENDERED)/whereabouts.yaml
	$(KUSTOMIZE) build neonvm/config/default-vxlan/multus-eks > $(RENDERED)/multus-eks.yaml
	$(KUSTOMIZE) build neonvm/config/default-vxlan/multus > $(RENDERED)/multus.yaml
	$(KUSTOMIZE) build neonvm/config/default-vxlan > $(RENDERED)/neonvm.yaml
	$(KUSTOMIZE) build deploy/scheduler > $(RENDERED)/autoscale-scheduler.yaml
	$(KUSTOMIZE) build deploy/agent > $(RENDERED)/autoscaler-agent.yaml
	# Cleanup:
	cd neonvm/config/common/controller && $(KUSTOMIZE) edit set image controller=controller:dev
	cd neonvm/config/default-vxlan/vxlan-controller && $(KUSTOMIZE) edit set image vxlan-controller=vxlan-controller:dev
	cd deploy/scheduler && $(KUSTOMIZE) edit set image autoscale-scheduler=autoscale-scheduler:dev
	cd deploy/agent && $(KUSTOMIZE) edit set image autoscaler-agent=autoscaler-agent:dev

.PHONY: deploy
deploy: load-images manifests render-manifests kubectl ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	$(KUBECTL) apply -f $(RENDERED)/multus.yaml
	$(KUBECTL) -n kube-system rollout status daemonset kube-multus-ds
	$(KUBECTL) apply -f $(RENDERED)/whereabouts.yaml
	$(KUBECTL) -n kube-system rollout status daemonset whereabouts
	$(KUBECTL) apply -f $(RENDERED)/neonvm.yaml
	$(KUBECTL) -n neonvm-system rollout status daemonset  neonvm-device-plugin
	$(KUBECTL) -n neonvm-system rollout status daemonset  neonvm-vxlan-controller
	$(KUBECTL) -n neonvm-system rollout status deployment neonvm-controller
	# NB: typical upgrade path requires updated scheduler before autoscaler-agents.
	$(KUBECTL) apply -f $(RENDERED)/autoscale-scheduler.yaml
	$(KUBECTL) -n kube-system rollout status deployment autoscale-scheduler
	$(KUBECTL) apply -f $(RENDERED)/autoscaler-agent.yaml
	$(KUBECTL) -n kube-system rollout status daemonset autoscaler-agent

.PHONY: load-images
load-images: kubectl kind k3d docker-build ## Push docker images to the local kind/k3d cluster
	@if [ $$($(KUBECTL) config current-context) = k3d-neonvm ]; then make k3d-load;  fi
	@if [ $$($(KUBECTL) config current-context) = kind-kind ];  then make kind-load; fi

.PHONY: example-vms
example-vms: docker-build-examples kind k3d kubectl ## Build and push the testing VM images to the kind/k3d cluster.
	@if [ $$($(KUBECTL) config current-context) = k3d-neonvm ]; then $(K3D) image import $(E2E_TESTS_VM_IMG) --cluster neonvm --mode direct; fi
	@if [ $$($(KUBECTL) config current-context) = kind-kind ]; then $(KIND) load docker-image $(E2E_TESTS_VM_IMG) --name kind; fi

.PHONY: pg14-disk-test
pg14-disk-test: docker-build-pg14-disk-test kind k3d kubectl ## Build and push the pg14-disk-test VM test image to the kind/k3d cluster.
	$(KUBECTL) create secret generic vm-ssh --from-file=private-key=vm-examples/pg14-disk-test/ssh_id_rsa || true
	@if [ $$($(KUBECTL) config current-context) = k3d-neonvm ]; then $(K3D) image import $(PG14_DISK_TEST_IMG) --cluster neonvm --mode direct; fi
	@if [ $$($(KUBECTL) config current-context) = kind-kind ]; then $(KIND) load docker-image $(PG14_DISK_TEST_IMG) --name kind; fi

.PHONY: kind-load
kind-load: kind # Push docker images to the kind cluster.
	$(KIND) load docker-image \
		$(IMG_CONTROLLER) \
		$(IMG_RUNNER) \
		$(IMG_VXLAN) \
		$(AUTOSCALER_SCHEDULER_IMG) \
		$(AUTOSCALER_AGENT_IMG) \
		--name kind

.PHONY: k3d-load
k3d-load: k3d # Push docker images to the k3d cluster.
	$(K3D) image import \
		$(IMG_CONTROLLER) \
		$(IMG_RUNNER) \
		$(IMG_VXLAN) \
		$(AUTOSCALER_SCHEDULER_IMG) \
		$(AUTOSCALER_AGENT_IMG) \
		--cluster neonvm --mode direct

##@ End-to-End tests

.PHONE: e2e-tools
e2e-tools: k3d kind kubectl kuttl ## Donwnload tools for e2e tests locally if necessary.

.PHONE: e2e
e2e: e2e-tools ## Run e2e kuttl tests
	$(KUTTL) test --config tests/e2e/kuttl-test.yaml
	rm -f kubeconfig

##@ Local kind cluster

.PHONY: kind-setup
kind-setup: kind kubectl ## Create local cluster by kind tool and prepared config
	$(KIND) create cluster --config kind/config.yaml
	$(KUBECTL) apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
	$(KUBECTL) -n cert-manager rollout status deployment cert-manager
	$(KUBECTL) apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
	$(KUBECTL) patch -n kube-system deployment metrics-server --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
	$(KUBECTL) -n kube-system rollout status deployment metrics-server

.PHONY: kind-destroy
kind-destroy: kind ## Destroy local kind cluster
	$(KIND) delete cluster --name kind

##@ Local k3d cluster

# K3D_FIX_MOUNTS=1 used to allow foreign CNI (cilium, multus and so on), https://github.com/k3d-io/k3d/pull/1268
.PHONY: k3d-setup
k3d-setup: k3d kubectl ## Create local cluster by k3d tool and prepared config
	K3D_FIX_MOUNTS=1 $(K3D) cluster create --config k3d/config.yaml
	$(KUBECTL) apply -f k3d/cilium.yaml
	$(KUBECTL) -n kube-system rollout status daemonset  cilium
	$(KUBECTL) -n kube-system rollout status deployment cilium-operator
	$(KUBECTL) apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
	$(KUBECTL) -n cert-manager rollout status deployment cert-manager

.PHONY: k3d-destroy
k3d-destroy: k3d ## Destroy local k3d cluster
	$(K3D) cluster delete --config k3d/config.yaml

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tools
KUSTOMIZE ?= $(LOCALBIN)/kustomize
KUSTOMIZE_VERSION ?= v4.5.7

ENVTEST ?= $(LOCALBIN)/setup-envtest
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
# List of available versions: https://storage.googleapis.com/kubebuilder-tools
ENVTEST_K8S_VERSION = 1.25.0

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
CONTROLLER_TOOLS_VERSION ?= v0.10.0
CODE_GENERATOR_VERSION ?= v0.25.10

KUTTL ?= $(LOCALBIN)/kuttl
KUTTL_VERSION ?= v0.15.0

KUBECTL ?= $(LOCALBIN)/kubectl
KUBECTL_VERSION ?= v1.25.11

KIND ?= $(LOCALBIN)/kind
KIND_VERSION ?= v0.18.0

K3D ?= $(LOCALBIN)/k3d
K3D_VERSION ?= v5.5.1

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
	@test -s $(LOCALBIN)/kuttl || { curl -sfSLo $(KUTTL) https://github.com/kudobuilder/kuttl/releases/download/$(KUTTL_VERSION)/kubectl-kuttl_$(subst v,,$(KUTTL_VERSION))_$(GOOS)_$(shell uname -m) && chmod +x $(KUTTL); }

.PHONY: k3d
k3d: $(K3D) ## Download k3d locally if necessary.
$(K3D): $(LOCALBIN)
	@test -s $(LOCALBIN)/k3d || { curl -sfSLo $(K3D)  https://github.com/k3d-io/k3d/releases/download/$(K3D_VERSION)/k3d-$(GOOS)-$(GOARCH) && chmod +x $(K3D); }

.PHONY: cert-manager
cert-manager: kubectl ## install cert-manager to cluster
	$(KUBECTL) apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
