# NeonVM kernel

We build a custom kernel in order to:

1. Have support for unusual features that are only required for VMs (e.g. hotplugging, kvm-ptp clock
   synchronization, etc.)
2. Avoid including features we don't need

Kernel images are all at `neondatabase/vm-kernel:$tag`, built by the
[vm-kernel](../.github/workflows/vm-kernel.yaml) github workflow.

Kernel images are typically assigned to a VM based on what was bundled in with the `neonvm-runner`
in use, although this can be overridden on an individual VM basis using the
`.spec.guest.kernelImage` field.

## Upgrading the kernel

NB: upgrading kernel config should be done for both supported architectures.
Assuming a plain upgrade (i.e. no additional features to enable), upgrading the kernel can be done
with the following sequence of actions:

1. On the host, run:
   ```sh
   export OLD_VERSION=<replace with version>
   export NEW_VERSION=<replace with version>
   export KERNEL_ARCH=amd64 # replace with the wanted arch, x86_64 or arm64
   cd neonvm-kernel # this directory
   docker build --build-arg KERNEL_VERSION=$NEW_VERSION --platform linux/x86_64 --target build-deps -t kernel-build-deps -f Dockerfile.kernel-builder .
   docker run --rm -e OLD_VERSION=$OLD_VERSION -e NEW_VERSION=$NEW_VERSION -e ARCH=$KERNEL_ARCH -v $PWD:/host --name kernel-build -it kernel-build-deps bash
   ```
2. Then, inside the container, run:
   ```sh
   cd linux-$NEW_VERSION
   cp /host/linux-config-$ARCH-$OLD_VERSION .config # Copy current config in
   make ARCH=$KERNEL_ARCH menuconfig
   # do nothing; just save and exit, overwriting .config
   cp .config /host/linux-config-$ARCH-$NEW_VERSION # NOTE: Different from existing!
   ```
3. Back on the host, finish with:
   ```sh
   # compare the two versions
   diff linux-config-$ARCH-$OLD_VERSION linux-config-$ARCH-$NEW_VERSION
   # If all looks good, delete the old version. This is required so auto-selection works.
   rm linux-config-$ARCH-$OLD_VERSION
   ```

Afterwards, it's probably also good to do a search-and-replace repo-wide to update all places that
mention the old kernel version.

## Adjusting the config

To adjust the kernel config, try the following from this directory:

```sh
docker build --build-arg KERNEL_VERSION=6.1.92 --platform linux/x86_64 --target build-deps -t kernel-build-deps -f Dockerfile.kernel-builder .
docker run --rm -v $PWD:/host --name kernel-build -it kernel-build-deps bash
# inside that bash shell, do the menuconfig, then copy-out the config to /host
```
