<!-- Checklist for updating Kubernetes -->

### go.mod

- [ ] Update `k8s.io/` replace directives from `v0.X.y` to the new version
- [ ] Update `k8s.io/kube-openapi` replace directive
- [ ] Update `github.com/cert-manager/cert-manager` to match the new k8s version
- [ ] Update `sigs.k8s.io/controller-runtime` to match the new k8s version, if needed
- [ ] Validate there's no dependencies on the old version

### Makefile

- [ ] Update `KUSTOMIZE_VERSION`
- [ ] Update `ENVTEST_K8S_VERSION`
- [ ] Update `CONTROLLER_TOOLS_VERSION`
- [ ] Update `CODE_GENERATOR_VERSION`
- [ ] Update `KUTTL_VERSION`
- [ ] Update `KUBECTL_VERSION`
- [ ] Update `ETCD_VERSION`
- [ ] Update `KIND_VERSION`
    - [ ] Update `kind/config.yaml` with new images
- [ ] Update `K3D_VERSION`
    - [ ] Update `k3d/config.yaml` with new images

### cluster-autoscaler

- [ ] Update `ca.branch`
- [ ] Update `ca.commit`
- [ ] Update `ca.patch`
- [ ] Update the golang builder image in `cluster-autoscaler/Dockerfile`
