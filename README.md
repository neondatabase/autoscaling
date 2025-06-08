# Autoscaling

Vertical autoscaling for a fleet of postgres instances running in a Kubernetes cluster.

For more on how Neon's Autoscaling works, check out <https://neon.tech/docs/introduction/autoscaling>.

## Development status

Autoscaling is used internally within Neon, and makes some minor assumptions about Neon-specifics.

We do not officially support use of autoscaling externally — in other words, you're welcome to try
it out yourself, submit bugs, fork the code, etc., but we make no guarantees about timely responses
to issues from running locally.

For help from the community, check out our Discord: <https://neon.tech/discord>.

## Quick access

The deployment files and a vm-builder binary are attached to each release.

Check out [Building and running](#building-and-running) below for local development.

## How it works

We want to dynamically change the amount of CPUs and memory of running postgres instances, _without
breaking TCP connections to postgres_.

This relatively easy when there's already spare resources on the physical (Kubernetes) node, but it
takes careful coordination to move postgres instances from one node to another when the original
node doesn't have the room.

We've tried a bunch of existing tools and settled on the following:

* Use [VM live migration](https://www.qemu.org/docs/master/devel/migration/index.html) to move running
  postgres instances between physical nodes
* QEMU is used as our hypervisor
* [NeonVM](./README-NeonVM.md) orchestrates NeonVM VMs as custom resources in
  K8s, and is responsible for scaling allocated resources (CPU and memory)
* A modified K8s scheduler ensures that we don't overcommit resources and triggers migrations when
  demand is above a pre-configured threshold
* Each K8s node has an `autoscaler-agent` pod that triggers scaling decisions and makes resource
  requests to the K8s scheduler on the VMs' behalf to reserve additional resources for them
* Each compute node runs the `vm-monitor` binary, which communicates to the autoscaler-agent so that it can
  immediately respond to memory pressure by scaling up (among other things).
* For Neon's postgres instances, we also track cache usage and potentially scale based on the
  heuristically determined working set size, which dramatically speeds up OLTP workloads.

Networking is preserved across migrations by giving each VM an additional IP address on a bridge
network spanning the cluster with a flat topology; the L2 network figures out "by itself" where to
send the packets after migration.

For more information, refer to [ARCHITECTURE.md](./ARCHITECTURE.md).

## Building and running

> [!NOTE]
> NeonVM and Autoscaling are not expected to work outside Linux x86.

### Install dependencies

To run autoscaling locally you need to install dependencies:
- [`kubectl`]
- [`kind`]/[`k3d`]
- [`kuttl`] (for e2e tests)

[`kubectl`]: https://kubernetes.io/docs/tasks/tools/#kubectl
[`kind`]: https://kubernetes.io/docs/tasks/tools/#kind
[`kuttl`]: https://kuttl.dev/
[`k3d`]: https://k3d.io

### Running locally

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
make vm-examples
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
VM_NAME=postgres16-disk-test scripts/run-bench.sh
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

### E2E tests

To run the end-to-end tests, you need to have [`kuttl`] installed. You can run the tests with:
```sh
make e2e
```
