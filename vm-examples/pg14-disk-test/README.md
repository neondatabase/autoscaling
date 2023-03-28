# pg14-disk-test

Tools for creating new local VM images. There's a few things to note here:

1. About `Dockerfile.vmdata`:
   * This is used to build the container that _superficially_ provides the contents of the VM. The
     actual VM image is created by the NeonVM image builder, stored in `neonvm-builder`.
   * It expects to have `ssh_id_rsa.pub` (which is generated as a part of `make pg14-disk-test` command)
2. About `init`:
   * It's used as the entrypoint of the VM, and provides the various pieces of userspace
     startup that we need (i.e. starting node\_exporter, postgres, and sshd).
