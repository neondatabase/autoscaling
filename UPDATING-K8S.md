# Updating Kubernetes

## go.mod

- Update `k8s.io/` replace directives
  - Most are `v0.X.y`, but `k8s.io/kubernetes` is `v1.X.y`
  - Also update `k8s.io/kube-openapi` based on `go mod graph | grep ' k8s.io/kube-openapi' | grep '^k8s.io/'`

- Update all of the following so that the version in their go.mod matches the k8s version we want:
  - `github.com/cert-manager/cert-manager`
  - `sigs.k8s.io/controller-runtime` (may be done automatically; make sure it's the latest!)

Repeatedly `go mod tidy` and search for occurrences of the old version in `go.mod` and `go.sum`.
To figure out why something is required, search for it in the output of `go mod graph` — it gives
more accurate information than `go mod why`.

## Makefile

Update the following tool versions in `Makefile` to match the new kubernetes version:

- `KUSTOMIZE_VERSION`
- `ENVTEST_K8S_VERSION`
- `CONTROLLER_TOOLS_VERSION`
- `CODE_GENERATOR_VERSION`
- `KUTTL_VERSION`
- `KUBECTL_VERSION`
- `ETCD_VERSION`
- `KIND_VERSION`
  - *Also* update `kind/config.yaml` so that the image SHAs match the version in the release.
- `K3D_VERSION`
  - *Also* update the `image` in `k3d/config.yaml`, something like `rancher/k3s:<version>+k3s1`

## cluster-autoscaler

Follow the updating instructions in `cluster-autoscaler/README.md`.
