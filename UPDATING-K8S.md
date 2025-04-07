# Updating Kubernetes

Because of our heavy integration with Kubernetes, we have a handful of steps involved in upgrading
our the version of Kubernetes we rely on.

Broadly, this is separated into three parts:

1. Updating our direct dependencies in `go.mod`
2. Updating our tools in `Makefile`
3. Updating our patch for `cluster-autoscaler`

> [!NOTE]
> There's a checklist available at [`UPDATING-K8S-CHECKLIST.md`](./UPDATING-K8S-CHECKLIST.md).
> Once you've made a some changes, you can create a PR with that checklist via the Github CLI:
> ```sh
> gh pr create --web --draft --title "Update to Kubernetes 1.XX" --body-file 'UPDATING-K8S-CHECKLIST.md'
> ```

## go.mod

#### k8s.io/ replace directives

At the top of `go.mod`, we have a lot of replace directives for `k8s.io/` dependencies. These must
all be updated to the new Kubernetes version. Most are `v0.X.y`; `k8s.io/kubernetes` is `v1.X.y`.

Also, update `k8s.io/kube-openapi` based on the version used in other Kubernetes dependencies. You
can check that with:
```sh
go mod graph | grep ' k8s.io/kube-openapi' | grep '^k8s.io/'
```

#### Other dependencies

There's a couple other dependencies that must be updated to use the same k8s version as we have. You
can check these by looking at their `go.mod` in relevant releases:

- `github.com/cert-manager/cert-manager`
- `sigs.k8s.io/controller-runtime` (may be updated automatically; make sure it's the latest!)

#### Checking your work

Once you've done the above, repeatedly run `go mod tidy` and search for occurrences of the old
version in `go.mod` and `go.sum`.

To figure out why something is required, seach for it in the output of `go mod graph` — it gives
more accurate information than `go mod why`.

## Makefile

We have a lot of tool versions defined as variables in `Makefile`. These should all be updated to
match the new Kubernetes version!

There should be some brief comment above each tool explaining how to match the version.

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
