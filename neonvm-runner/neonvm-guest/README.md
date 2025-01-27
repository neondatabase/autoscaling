# neonvm-guest: Runs in the VM

neonvm-guest is the image that runs in a VM started by neonvm-runner. It runs
the basic services needed to interact with the host pod.

## Building

The neonvm-guest image is built by the Dockerfile in the parent directory. (XXX:
The build instructions for neonvm-guest must be embedded in the neonvm-runner
Dockerfile because there's no include mechanism Dockerfile; consider using a
docker "bake file" though once that feature matures)


## NeonVM Guest services

- vector, needed for autoscaling
- neonvmd

- Launch the payload container


In the VM, you can see all the processes with 'ps ax'. To see the view from
within container, use "machinectl shell neonvm-payload"


## NeonVM Payload

The actual services are run in a container within the guest. The guest payload
image is mounted under /payload/rootfs in the VM, and is run in a container
using systemd-nspawn.

The payload is expected to be a full system image with 'init'. The payload
itself can use systemd to launch multiple services inside the container. Running
systemd inside a docker container is generally not recommended and many consider
it to be a bad practice, but we're not using docker, and nested systemd
containers work just fine.


## Namespaces

Containers are implemented by namespaces in Linux. There are different
namespaces for users, network, filesystems etc. For a maximally isolated
container, you'd want the container to have an isolated namespace for each of
those namespace kinds, but that's not necessarily the goal with NeonVM. The VM
is the main security barrier to isolate tenants. Namespaces within the VM can
provide extra hygiene, but should not be solely relied on for security.

The payload container runs in the root network namespace. That is, there is no
network isolation between the NeonVM Guest parent and the payload container.
