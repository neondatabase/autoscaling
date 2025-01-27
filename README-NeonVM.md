# NeonVM: QEMU-based virtualization API and controller for Kubernetes

## Description

## Getting Started
You’ll need a Kubernetes cluster to run against. You can use [KIND](https://sigs.k8s.io/kind) to get a local cluster for testing, or run against a remote cluster.
**Note:** Your controller will automatically use the current context in your kubeconfig file (i.e. whatever cluster `kubectl cluster-info` shows).

### Install cert-manager

```console
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
```

### Install NeonVM with VXLAN-based overlay network

```console
kubectl apply -f https://github.com/neondatabase/autoscaling/releases/latest/download/multus.yaml
kubectl apply -f https://github.com/neondatabase/autoscaling/releases/latest/download/whereabouts.yaml
kubectl apply -f https://github.com/neondatabase/autoscaling/releases/latest/download/neonvm.yaml
kubectl apply -f https://github.com/neondatabase/autoscaling/releases/latest/download/neonvm-vxlan-controller.yaml
kubectl apply -f https://github.com/neondatabase/autoscaling/releases/latest/download/neonvm-controller.yaml
```

### Run virtual machine

```console
cat <<EOF | kubectl apply -f -
apiVersion: vm.neon.tech/v1
kind: VirtualMachine
metadata:
  name: vm-debian
spec:
  guest:
    rootDisk:
      image: neondatabase/vm-debian:11
    cpus: { min: 1, use: 1, max: 1 }
    memorySlots: { min: 1, use: 1, max: 1 }
EOF
```

### Check virtual machine running

```console
kubectl get neonvm

NAME        CPUS   MEMORY   POD               STATUS    AGE
vm-debian   1      1Gi      vm-debian-8rxp7   Running   3m13s
```

### Go inside virtual machine

#### SSH

```sh
kubectl exec -it $(kubectl get neonvm vm-debian -ojsonpath='{.status.podName}') -- ssh guest-vm
```

#### Pseudoterminal

The VM has two serials:

amd64:
/dev/ttyS0 : for console login, we run getty on it
/dev/ttyS1 : for log output, goes to QEMU stdout, and on to the kubernetes log in the host

console=/dev/ttyS1 is passed on the command line

arm64:
/dev/ttyAMA0 : for console login
/dev/hvc0 : for log output, goes to QEMU stdout, and on to the kubernetes log in the host

console=/dev/hvc0 is passed on the command line



```console
kubectl exec -it $(kubectl get neonvm vm-debian -ojsonpath='{.status.podName}') -- screen /dev/pts/0

<press ENTER>

root@neonvm:~#
root@neonvm:~# apt-get update >/dev/null && apt-get install -y curl >/dev/null
root@neonvm:~# curl -k https://kubernetes/version
{
  "major": "1",
  "minor": "25",
  "gitVersion": "v1.25.2",
  "gitCommit": "5835544ca568b757a8ecae5c153f317e5736700e",
  "gitTreeState": "clean",
  "buildDate": "2022-09-22T05:25:21Z",
  "goVersion": "go1.19.1",
  "compiler": "gc",
  "platform": "linux/amd64"
}

<press CTRL-a k to exit screen session>
```

### Delete virtual machine

```console
kubectl delete neonvm vm-debian
```

### Clock synchronization

We synchronize VM clocks to host using kvm_ptp. We enable PTP clock (and the KVM related directive) on the kernel and use chrony on the VM as a server. 
You can checkout chrony server log at `/var/log/chrony/chrony.log` and use chrony client as below:

```sh
/neonvm/bin/chronyc tracking
/neonvm/bin/chronyc sources
```

## Local development

### Run NeonVM locally

#### 1. Create local cluster (with 3 nodes)

```sh
make k3d-setup
```

#### 2. Build Linux kernel for Guests

```sh
make kernel
```

For more on the kernel, see [neonvm-kernel/](./neonvm-kernel/).

#### 3. Build and deploy controller and VXLAN overlay network to local cluster

```sh
make deploy
```

### Manage Virtual Machines

#### 0. Build & load example images into the k8s cluster

```
make example-vms
```

#### 1. Run virtual machine

```console
kubectl apply -f samples/vm-example.yaml
```

NB: on machines without `/dev/kvm` (e.g., on EC2 non-bare-metal), set `.spec.enableAcceleration = false`.

#### 2. Check VM running

```sh
kubectl get neonvm example

kubectl get neonvm example -owide

kubectl describe neonvm example
```

#### 3. Check logs

```sh
VM_POD=$(kubectl get neonvm example -ojsonpath='{.status.podName}')
kubectl logs $VM_POD
```

#### 4. Connect to the VM

##### SSH

```sh
kubectl exec -it $VM_POD -- ssh guest-vm
```

##### Console

```sh
kubectl exec -it $VM_POD -- screen /dev/pts/0

<press Enter to see output>
```
to exit from console press `CTRL-a k` (see manual for `screen` tool)

#### 5. Plug/Unplug CPUs in VM

edit `.spec.guest.cpus.use` field by editor

```sh
kubectl edit neonvm example
```

or apply patch (set `cpus.use` to `2`)

```sh
kubectl patch neonvm example --type='json' -p='[{"op": "replace", "path": "/spec/guest/cpus/use", "value":2}]'
```

and then check status by `kubectl get neonvm example` and inspect `/sys/devices/system/cpu` folder inside VM

#### 6. Plug/Unplug Memory in VM

edit `.spec.guest.memorySlots.use` field by editor

```sh
kubectl edit neonvm example
```

or apply patch (as example set `memorySlots.use` to `4`)

```sh
kubectl patch neonvm example --type='json' -p='[{"op": "replace", "path": "/spec/guest/memorySlots/use", "value":4}]'
```

and then check status by `kubectl get neonvm example` and inspect memory inside VM by `free -h` command

#### 7. Do live migration

inspect VM details to see on what node it running

```sh
$ kubectl get neonvm -owide
NAME      CPUS   MEMORY   POD             STATUS    AGE   NODE          IMAGE
example   1      2Gi      example-xdw4s   Running   31s   kind-worker   vm-postgres:15-bullseye
```

trigger live migration

```sh
kubectl apply -f samples/vm-example-migration.yaml
```

inspect migration details

```sh
$ kubectl get neonvmm -owide
NAME      VM        SOURCE          SOURCEIP     TARGET          TARGETIP     STATUS      AGE
example   example   example-xdw4s   10.244.1.7   example-7ztb2   10.244.2.6   Succeeded   33s
```

inspect VM details again (look at pod name and node)

```sh
$ kubectl get neonvm -owide
NAME      CPUS   MEMORY   POD             STATUS    AGE     NODE           IMAGE
example   1      2Gi      example-7ztb2   Running   4m12s   kind-worker2   vm-postgres:15-bullseye
```

inspect migration details

```sh
$ kubectl get neonvmm example -ojsonpath='{.status.info}' | jq
{
  "compression": {
    "compressedSize": 44687045
  },
  "downtimeMs": 39,
  "ram": {
    "total": 2148278272,
    "transferred": 76324646
  },
  "setupTimeMs": 2,
  "status": "completed",
  "totalTimeMs": 3861
}
```

### Uninstall CRDs
To delete the CRDs from the cluster:

```sh
make uninstall
```

### Undeploy controller
UnDeploy the controller to the cluster:

```sh
make undeploy
```

## How it works
This project aims to follow the Kubernetes [Operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)

It uses [Controllers](https://kubernetes.io/docs/concepts/architecture/controller/)
which provides a reconcile function responsible for synchronizing resources untile the desired state is reached on the cluster

## Roadmap

- [x] Implement Webhooks for mutation and validation
- [x] Multus CNI support
- [x] Hot[un]plug CPUs and Memory (via resource patch)
- [x] Live migration CRDs
- [x] Simplify VM disk image creation from any docker image
- [ ] ARM64 support

