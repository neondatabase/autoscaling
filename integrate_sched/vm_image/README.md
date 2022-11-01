# vm_image

Tools for creating new local VM images. There's a few things to note here:

1. About `build.sh`:
   1. It requires `start-local-registry.sh` to have been run beforehand.
   2. It generates an ssh keypair for *accessing* the VM. The public key (`ssh_id_rsa.pub`) gets
      built into the vm. The private key (`ssh_id_rsa`) MUST NOT be inculded in ANY dockerfile. The
      keypair exists only so that if *we* mess up, we don't make it easier to access the VM.
2. About `start-local-registry.sh`:
   * This ensures that the local docker regsitry `kind-registry` is running at `localhost:5001`. It's
     supposed to be available within the cluster as `kind-registry:5000`, although I'm not sure how
     to check that -- `kubectl apply -f <FILE>` requires `localhost:5001`.
3. About the dockerfiles -- there's two of them:
   1. `Dockerfile.vmdata` is the actual content of the VM -- in the sense that it's where we set all
      of the initial data for the container that gets turned into the VM. It gets built in `build.sh`
      and the raw container contents are extracted to form the VM image.
   2. `Dockerfile.img` is 
4. About `init`:
   * It runs on startup, which `Dockerfile.vmdata` guarantees by putting it into `/sbin/init`
   * The important bits around starting Postgres and sshd are both in there

## Differences from `kind_setup/vm_image`

This directory is almost entirely the same, except the VM stores its state in a tmpfs (for now), to
allow migrations without block storage devices.
