# Autoscaling

Vertical autoscaling for fleet of postgres instances running in k8s cluster.

## Overview

We want to dynamicly change amount of CPUs and memory on running postgres instances, maintaining the following principle:

```
change of resources shoud not break TCP connections to postgres
```

It is easy to follow this principle when there are spare resources on the current physical node. On the contrary, if there are no spare resources moving running proces between physical nodes without breaking TCP connections requires some amount of engineering.

We've (tried bunch)[https://github.com/neondatabase/cloud/issues/1651] of existing tools and settled up with following:

* we use VM live migration to move running postgreses between physical nodes
* (cloud-hypervisor)[https://github.com/cloud-hypervisor/cloud-hypervisor] is used as a virtual machine manager
* (virtink)[https://github.com/smartxworks/virtink] is used to orchestrate `cloud-hypervisor` VMs as custom resources in k8s

## Networking

To preserve active TCP connections during the migration we have several options:
* preserve IP address and let L2 network to fugure out where to send packets
* use tunnel inside of VM and re-establish tunnel connection after migration
* use QUIC protocol between proxy and postgres and update QUIC destination after migration

So far we are using the first option since it does not requires any additional steps after migration.

## Storage

`cloud-hypervisor` does not migrate disks, so we need to use some DFS. Latest tests used NFS from within the VM.
