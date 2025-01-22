# NeonVM guest - payload separation

The neonvm-guest image is booted in the VM. Its purpose is to provide basic
services like metrics, SSH for debugging, and services related to autoscaling
(neonvm-daemon). It launches the payload image in a container.

The rule of thumb for what belongs in neonvm-guest and what belongs in
neonvm-payload is that the payload image should be runnable on its own, e.g. as
a docker container. Neonvm-guest provides facilities that are specific to
running in a NeonVM.

Here's an overview picture showing the important processes running in the runner
pod, the guest VM, and in the payload container:

    +- K8s compute pod ---------------------------------------------+
    |                                                               |
    |  neonvm-runner (pid 1)                                        |
    |  \                                                            |
    |   ## VM booted from neonvm-guest ############################ |
    |   #                                                         # |
    |   # systemd (pid 1)                                         # |
    |   #  \                                                      # |
    |   #   + ssh server                                          # |
    |   #   |                                                     # |
    |   #   + vector                                              # |
    |   #   |                                                     # |
    |   #   + neonvm-daemon                                       # |
    |   #   |                                                     # |
    |   #   + systemd-nspawn                                      # |
    |   #     \                                                   # |
    |   #      +- neonvm-payload container ---------------------+ # |
    |   #      |                                                | # |
    |   #      | systemd (pid 1)                                | # |
    |   #      | \                                              | # |
    |   #      |  + pgbouncer                                   | # |
    |   #      |  |                                             | # |
    |   #      |  + sql_exporter                                | # |
    |   #      |  |                                             | # |
    |   #      |  + compute_ctl                                 | # |
    |   #      |   \                                            | # |
    |   #      |    + postgres                                  | # |
    |   #      |     \                                          | # |
    |   #      |      ...                                       | # |
    |   #      |                                                | # |
    |   #      +------------------------------------------------+ # |
    |   #                                                         # |
    |   ########################################################### |
    |                                                               |
    +---------------------------------------------------------------+

Neonvm-guest launches the payload in a container, but we don't rely on the
container separation for security purposes. It's purpose is to allow the
neonvm-guest and payload to use different Linux distributions, for example,
without interfering with each other,

The neonvm-guest image is included in the neonvm-runner image. It should be
considered to be part of the runner.


## Guest environment

The neonvm-guest uses systemd to manage all the services. (The payload is
expected to also run systemd, but it is not a strict requirement.)

The payload is mounted in the guest as /neonvm/payload. systemd-nspawn is used
to launch it as a container.

## Guest - payload interface

The payload is supposed to be runnable in a docker container on its own. When
running standalone without neonvm-guest, services provided by neonvm-guest will
naturally be unavailable.

- disk mounts
- neonvm-daemon

### /neonvm

The /neonvm directory is shared by the guest and the payload

/neonvm/run: Contains transient files, similar to /var/run or /run. Currently, this is only used to hold the neonvm-daemon control socket.

/neonvm/runtime: Contains configuration options passed in the NeonVM spec. This is made available in both the guest and the payload.

/neonvm/payload: Contains the payload container's filesystem. Inside the payload container, it's the root.

Additional disks may be mounted here, depending on the VirtualMachine spec. See
Disks.

### Neonvm-daemon

A NeonVM specific daemon process runs in the guest, which provides some services
to the payload. (It also provides a different services to the runner/autoscaling
agent, but those should not be accessed from the payload). The services are
exposed as an HTTP server listening on a UNIX domain socket:
/neonvm/run/neonvmd-ctl-socket. Two commands are available:

- resize-swap
- set-disk-quota

These are actions that are specific to running in a VM and require intimate
knowledge of how the disks are set up.

If the control socket is not present, the payload can assume that it's not
running in a VM.

### Disks

In addition to the root disk, neonvm-guest makes two additional disks available
to the payload:

/neonvm/cache: Intended for the Local File Cache. The size is chosen based on the VM memory size.

/neonvm/pgdata: Intended for local files, in particular the Postgres data directory when running a compute Postgres instance. For compatibility with old VM images not using neonvm-guest, this is mounted in the payload container as /var/db/compute/pgdata.

Both of these disks are empty at VM startup, but if the payload is
restarted for some reason, they are not wiped.

TODO: The VirtualMachine spec allows specifying different disks. That's not
currently supported; neonvm-guest assumes the above two disks, and no other
disks. I believe this could improved to support the full flexibility we have in
the spec files, but I wonder if that's a good idea. I feel that the control
plane is not the right place make decisions about disks and how they're
mounted. On one side, the autoscaling components are responsible for managing
resources across the cluster, and that should include local disk resouces. And
on the other side, the compute - or other payload - needs to know where things
are mounted, so the payload image should have control over that. The control
plane doesn't really have any stake in either of those things.

### Swap

The VM starts with no swap enabled. The payload may use the neonvm-daemon
resize-swap command to enable it.

### UID ranges

Neonvm-guest uses IDs (UIDs) and group IDs (GIDs) < 1000. This is not otherwise
visible to the payload, but the payload disk is bound inside the payload with
the "idmap" option. TODO: more detail

### Logging

The payload is spawned with the systemd-nspawn --link-journal=try-guest option,
which means that if the payload also runs runs systemd, its journal files are
made visible to the container's host system (= neonvm-guest).

In neonvm-guest, the logs directed to a TTY that is connected to the host qemu's
stdout, so that the logs are visible e.g. with "kubectl logs".
