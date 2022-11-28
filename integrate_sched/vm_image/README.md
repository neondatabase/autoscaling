# vm_image

Tools for creating new local VM images. There's a few things to note here:

1. About `build.sh`:
   1. It requires `start-local-registry.sh` to have been run beforehand.
   2. It generates an ssh keypair for *accessing* the VM. The public key (`ssh_id_rsa.pub`) gets
      built into the vm. The private key (`ssh_id_rsa`) MUST NOT be inculded in ANY dockerfile. The
      keypair exists only so that if *we* mess up, we don't make it easier to access the VM.
   3. It builds and stores `neonvm-builder` (from NeonVM's `vm-builder`) to generate VM images
2. About `start-local-registry.sh`:
   * This ensures that the local docker regsitry `kind-registry` is running at `localhost:5001`. It's
     supposed to be available within the cluster as `kind-registry:5000`, although I'm not sure how
     to check that -- `kubectl apply -f <FILE>` requires `localhost:5001`.
3. About `Dockerfile.vmdata`:
   * This is used to build the container that _superficially_ provides the contents of the VM. The
     actual VM image is created by the NeonVM image builder, stored in `neonvm-builder`.
4. About `init`:
   * It's used as the entrypoint of the VM, and provides the various pieces of userspace
     startup that we need (i.e. starting node\_exporter, postgres, and sshd).
