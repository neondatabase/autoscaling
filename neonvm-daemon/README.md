# NeonVM Daemon

The NeonVM daemon (aka. neonvm-daemon or neonvmd) is a small daemon that runs in
the VM and performs a few privileged actions on behalf of other parts of the
system that cannot easily perform.

It currently has two interfaces:

## External-facing HTTP interface

If the VM needs to be resized, it sets CPUs offline/online. Resizing is
requested by the autoscaling agent which runs outside the VM. The agent cannot
directly switch CPUs offline/online from the outside, so it needs something
inside the VM to perform that. (This is platform-dependent; on amd64 platforms,
CPUs can be hotplugged on the fly, but on arm64 that's not currently possible,
so we have to merely offline/online them instead. But we use the same mechanism
on all platforms.)

The neonvm-daemon listens on an TCP port, for these CPU-scaling requests. The
port is exposed from the VM because the autoscaling-agent needs to connect to
it. (Access is restricted to just the autoscaling-agent by k8s network policies
and/or iptables rules.)

The VM is expected to obey CPU offlining requests. If the VM does not obey and
continues uses more CPUs than it's allowed after it has been downscaled, it can
be detected from the outside and terminated.

## Internal-facing control socket

The daemon also provides an internal interface that is only accessible from
within the VM. It's exposed as a "control socket", which is UNIX domain socket
at /neonvm/run/neonvm-daemon-socket. The protocol is HTTP over that socket.

Via the control socket, the VM payload can do two things:

1. Resize swap.

2. Set disk quota

With root privileges, the payload could do both of these actions by
itself. However, the payload would need to a) have root privileges, and b) know
intimate details of the disk volumes, which we'd like to hide from the payload.
