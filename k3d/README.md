## Cilium CNI

Create manifest for local k3d cluster

```console
helm repo add cilium https://helm.cilium.io/
helm template cilium cilium/cilium \
    --version 1.13.4 \
    --set operator.replicas=1 \
    --namespace kube-system >cilium.yaml
```
