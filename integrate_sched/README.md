# Integrated scheduler

This builds on [`scaling_builtin`], which in turn builds on [`kind_setup`]. For more detail, refer
to their respective READMEs (particularly [`kind_setup`]).

[`scaling_builtin`]: ../scaling_builtin
[`kind_setup`]: ../kind_setup

Steps:

Build everything:

```sh
vm_image/start-local-registry.sh # required for everything below. Does nothing on repeat
vm_image/build.sh
scheduler/build.sh
autoscaler-agent/build.sh
```

Download kubernetes dependencies:

```sh
./download-cni.sh
curl -sS https://raw.githubusercontent.com/flannel-io/flannel/v0.19.2/Documentation/kube-flannel.yml \
    -o flannel.yaml
curl -sSL https://github.com/cert-manager/cert-manager/releases/download/v1.8.2/cert-manager.yaml \
    -o cert-manager.yaml
curl -sS https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/master/deployments/multus-daemonset.yml \
    -o multus-daemonset.yaml
```

Set up the cluster:

```sh
kind create cluster -n autoscale-sched --config=kind-config.yaml
kubectl apply -f flannel.yaml -f cert-manager.yaml -f multus-daemonset.yaml \
    -f scheduler-deploy.yaml -f autoscaler-agent-deploy.yaml
# (wait until cert manager has been ready for a bit)
kubectl apply -f virtink_localhost:5001.yaml
```

Run the VM(s):

```sh
kubectl apply -f vm-deploy.yaml
# or:
kubectl apply -f vm-double-deploy.yaml
```

Run pgbench and watch the vCPU allocation grow:

```sh
./run-bench.sh
# or:
VM_NAME=postgres14-disk-1 ./run-bench.sh
VM_NAME=postgres14-disk-2 ./run-bench.sh
```

