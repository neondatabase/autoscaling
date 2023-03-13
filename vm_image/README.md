# vm_image

Tools for creating new local VM images. There's a few things to note here:

1. About `build.sh`:
   1. It generates an ssh keypair for *accessing* the VM. The public key (`ssh_id_rsa.pub`) gets
      built into the vm. The private key (`ssh_id_rsa`) MUST NOT be included in ANY dockerfile. The
      keypair exists only so that if *we* mess up, we don't make it easier to access the VM.
   2. It builds and stores `neonvm-builder` (from NeonVM's `vm-builder`) to generate VM images
2. About `Dockerfile.vmdata`:
   * This is used to build the container that _superficially_ provides the contents of the VM. The
     actual VM image is created by the NeonVM image builder, stored in `neonvm-builder`.
3. About `init`:
   * It's used as the entrypoint of the VM, and provides the various pieces of userspace
     startup that we need (i.e. starting node\_exporter, postgres, and sshd).
