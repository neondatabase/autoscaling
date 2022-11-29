# NeonVM: QEMU-based virtualization API and controlller for Kubernetes

## Description

## Getting Started
You’ll need a Kubernetes cluster to run against. You can use [KIND](https://sigs.k8s.io/kind) to get a local cluster for testing, or run against a remote cluster.
**Note:** Your controller will automatically use the current context in your kubeconfig file (i.e. whatever cluster `kubectl cluster-info` shows).

### Install cert-manager

```console
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
```

### Install NeonVM

```console
kubectl apply -f https://github.com/neondatabase/neonvm/releases/latest/download/neonvm.yaml
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
EOF
```

### Check virtual machine running

```console
kubectl get neonvm

NAME        CPUS   MEMORY   POD               STATUS    AGE
vm-debian   1      1Gi      vm-debian-8rxp7   Running   3m13s
```

### Go inside virtual machine

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


## Local development

### Run NeonVM locally

#### 1. Create local cluster

```sh
kind create cluster
```

#### 2. Install cert-manager

```sh
make cert-manager
```

#### 3. Build Linux kernel for Guests

```sh
make kernel
```

#### 4. Build and deploy controller  to local cluster

```sh
make deploy
```

### Manage Virtual Machines

#### 1. Run virtual machine

```console
kubectl apply -f samples/vm-example.yaml
```

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

#### 4. Connect to console inside VM

```sh
kubectl exec -it $VM_POD -- screen /dev/pts/0

<press Enter to see output>
```
to exit from console presss `CTRL-a k` (see manual for `screen` tool)

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

```sh
...soon
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
- [ ] Multus CNI support
- [x] Hot[un]plug CPUs and Memory (via resource patch)
- [ ] Live migration CRDs
- [x] Simplify VM disk image creation from any docker image
- [ ] ARM64 support


## License

Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

