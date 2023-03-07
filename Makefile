AUTOSCALER_SCHEDULER_IMG ?= kube-autoscale-scheduler:dev
AUTOSCALER_AGENT_IMG ?= autoscaler-agent:dev
VM_INFORMANT_IMG ?= vm-informant:dev
EXAMPLE_VM_IMG ?= vm-example:dev

.PHONY: docker-build
docker-build:
	docker buildx build \
		--tag $(AUTOSCALER_SCHEDULER_IMG) \
		--build-arg "GIT_INFO=n/a" \
		--file build/autoscale-scheduler/Dockerfile \
		.
	docker buildx build \
		--tag $(AUTOSCALER_AGENT_IMG) \
		--build-arg "GIT_INFO=n/a" \
		--file build/autoscaler-agent/Dockerfile \
		.
	docker buildx build  \
		--tag $(VM_INFORMANT_IMG) \
		--build-arg "GIT_INFO=n/a" \
		--file build/vm-informant/Dockerfile \
		.

.PHONY: kind-load
kind-load: docker-build
	kind load docker-image $(AUTOSCALER_SCHEDULER_IMG)
	kind load docker-image $(AUTOSCALER_AGENT_IMG)
	kind load docker-image $(VM_INFORMANT_IMG)

.PHONE: deploy
deploy: kind-load

.PHONE: vm-example
vm-example: neonvm
	docker buildx build  \
		--tag tmp-$(EXAMPLE_VM_IMG) \
		--file tests/e2e/vm-example/Dockerfile \
		tests/e2e/vm-example/
	./neonvm/bin/vm-builder -src tmp-$(EXAMPLE_VM_IMG) -use-inittab -dst $(EXAMPLE_VM_IMG)
	kind load docker-image $(EXAMPLE_VM_IMG)

.PHONE: e2e
e2e:
	kubectl kuttl test --config tests/e2e/kuttl-test.yaml
