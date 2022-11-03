# NeonVM: QEMU-based virtualization API and controlller for Kubernetes

## Description

## Getting Started
You’ll need a Kubernetes cluster to run against. You can use [KIND](https://sigs.k8s.io/kind) to get a local cluster for testing, or run against a remote cluster.
**Note:** Your controller will automatically use the current context in your kubeconfig file (i.e. whatever cluster `kubectl cluster-info` shows).

### Running on the cluster
1. Create local cluster

```sh
kind create cluster
```

2. Install cert-manager

```sh
make cert-manager
```

3. Build Linux kernel for Guests

```sh
make kernel
```

4. Build and deploy controller  to local cluster

```sh
make deploy
```

### Manage Virtual Machines

1. Run virtual machine

```console
$ kubectl apply -f samples/vm-example.yaml
virtualmachine.vm.neon.tech/example created
```

2. Check VM running

```sh
kubectl get neonvm example

kubectl get neonvm example -owide

kubectl describe neonvm example
```

3. Check logs

```sh
VM_POD=$(kubectl get neonvm example -ojsonpath='{.status.podName}')
kubectl logs $VM_POD
```

4. Connect to console inside VM

```sh
kubectl exec -it $VM_POD -- screen /dev/pts/0

<press Enter to see output>
```
to exit from console presss `CTRL-A K` (see manual for `screen` tool)

5. Plug/Unplug CPUs in VM

```sh
...soon
```

6. Plug/Unplug Memory in VM

```sh
...soon
```

7. Do live migration

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

### How it works
This project aims to follow the Kubernetes [Operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)

It uses [Controllers](https://kubernetes.io/docs/concepts/architecture/controller/) 
which provides a reconcile function responsible for synchronizing resources untile the desired state is reached on the cluster

### Test It Out
1. Install the CRDs into the cluster:

```sh
make install
```

2. Run your controller (this will run in the foreground, so switch to a new terminal if you want to leave it running):

```sh
make run
```

**NOTE:** You can also run this in one step by running: `make install run`

### Modifying the API definitions
If you are editing the API definitions, generate the manifests such as CRs or CRDs using:

```sh
make manifests
```

**NOTE:** Run `make --help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

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

