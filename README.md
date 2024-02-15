# Autoscaling

Vertical autoscaling for a fleet of postgres instances running in a Kubernetes cluster.

## Quick access

Images are available as:

| Component name | Image name |
|----------------|------------|
| scheduler (and plugin) | `neondatabase/autoscale-scheduler` |
| autoscaler-agent | `neondatabase/autoscaler-agent` |

The deployment files and a vm-builder binary are attached to each release.

For information on inter-version compatibility, see
[`pkg/api/VERSIONING.md`](./pkg/api/VERSIONING.md).

For now, the currently deployed configuration on staging is manually replicated
in the [`staging` branch](https://github.com/neondatabase/autoscaling/tree/staging).

## Overview

We want to dynamically change the amount of CPUs and memory of running postgres instances, _without
breaking TCP connections to postgres_.

This relatively easy when there's already spare resources on the physical (Kubernetes) node, but it
takes careful coordination to move postgres instances from one node to another when the original
node doesn't have the room.

We've [tried a bunch](https://github.com/neondatabase/cloud/issues/1651) of existing tools and
settled on the following:

* Use [VM live migration](https://www.qemu.org/docs/master/devel/migration.html) to move running
  postgres instances between physical nodes
* QEMU is used as our hypervisor
* [NeonVM](https://github.com/neondatabase/autoscaling/tree/main/neonvm) orchestrates NeonVM VMs as custom resources in
  K8s, and is responsible for scaling allocated resources (CPU and memory _slots_)
* A modified K8s scheduler ensures that we don't overcommit resources and triggers migrations when
  demand is above a pre-configured threshold
* Each K8s node has an `autoscaler-agent` pod that triggers scaling decisions and makes resource
  requests to the K8s scheduler on the VMs' behalf to reserve additional resources for them
* Each compute node runs the _VM monitor_ binary, which communicates to the autoscaler-agent so that it can
  immediately respond to memory pressure by allocating more (among other things).

Networking is preserved across migrations by giving each VM an additional IP address on a bridge
network spanning the cluster with a flat topology; the L2 network figures out "by itself" where to
send the packets after migration.

For more information, refer to [ARCHITECTURE.md](./ARCHITECTURE.md).

## Building and running

> [!NOTE]
> NeonVM and Autoscaling are not expected to work outside Linux x86.

Build NeonVM Linux kernel (it takes time, can be run only once)

```sh
make kernel
```

Build docker images:

```sh
make docker-build
```

Start local cluster with [`kind`] or [`k3d`]:

```sh
make kind-setup # or make k3d-setup
```

Deploy NeonVM and Autoscaling components

```sh
make deploy
```

Build and load the test VM:

```sh
make pg16-disk-test
```

Start the test VM:

```sh
kubectl apply -f vm-deploy.yaml
```

### Running pgbench

Broadly, the `run-bench.sh` script just exists to be expensive on CPU, so that more vCPU will be
allocated to the vm. You can run it with:

```sh
scripts/run-bench.sh
# or:
VM_NAME=postgres14-disk-test scripts/run-bench.sh
```

### Running `allocate-loop`

To test on-demand memory reservation, the [`allocate-loop`] binary is built into the test VM, and
can be used to slowly increasing memory allocations of arbitrary size. For example:

```sh
# After ssh-ing into the VM:
cgexec -g memory:neon-test allocate-loop 256 2280
#^^^^^^^^^^^^^^^^^^^^^^^^^               ^^^ ^^^^
# run it in the neon-test cgroup  ;  use 256 <-> 2280 MiB
```

[`allocate-loop`]: vm-examples/pg16-disk-test/allocate-loop.c

### Testing

To run e2e tests you need to install dependencies:
- [`kubectl`]
- [`kind`]/[`k3d`]
- [`kuttl`]

You can either download them from their websites or install using Homebrew: `brew install kubectl kind k3d kuttl`

```sh
make kind-setup # or make k3d-setup, if you'd like to use k3d
make kernel
make deploy
make example-vms
make e2e
```

[`kubectl`]: https://kubernetes.io/docs/tasks/tools/#kubectl
[`kind`]: https://kubernetes.io/docs/tasks/tools/#kind
[`kuttl`]: https://kuttl.dev/
[`k3d`]: https://k3d.io
