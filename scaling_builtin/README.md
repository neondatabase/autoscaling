# Scaling (builtin)

Autoscaling demo (from [`kind_setup`]) with a few modifications:

[`kind_setup`]: ../kind_setup

 1. `autoscaler.py` running as a sidecar alongside `cloud-hypervisor` in the VM's pod. This requires:
    1. Running a custom build of virtink to allow sidecars in the pod (at least commit `280fc9f5`,
       from branch `cpu-scaling`) -- must available on the local registry from
       [`kind_setup`](../kind_setup)
    2. Mounting `/var/run/virtink/ch.sock` within the autoscaler container

## List of commands

```sh
./download-cni.sh
kind create cluster -n scalingtest --config=kind-config.yaml
kubectl config set-context kind-scalingtest
kubectl apply -f flannel.yaml \
    -f multus-daemonset.yaml \
    -f cert-manager.yaml
# (wait for all pods to become ready)
kubectl apply -f virtink_localhost:5001.yaml
# (wait for vm-postgres14-... to become ready)
./run-bench.sh
```
