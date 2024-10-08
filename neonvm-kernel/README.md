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

Assuming a plain upgrade (i.e. no additional features to enable), upgrading the kernel can be done
with the following sequence of actions:

1. On the host, run:
   ```sh
   cd neonvm-kernel # this directory
   docker build --build-arg KERNEL_VERSION=$NEW_VERSION --platform linux/x86_64 --target build-deps -t kernel-build-deps -f Dockerfile.kernel-builder .
   docker run --rm -v $PWD:/host --name kernel-build -it kernel-build-deps bash
   ```
2. Then, inside the container, run:
   ```sh
   cd linux-$NEW_VERSION
   cp /host/linux-config-6.1.92 .config # Copy current config in
   make menuconfig
   # do nothing; just save and exit, overwriting .config
   cp .config /host/linux-config-$NEW_VERSION # NOTE: Different from existing!
   ```
3. Back on the host, finish with:
   ```sh
   # compare the two versions
   diff linux-config-6.1.92 linux-config-$NEW_VERSION
   # If all looks good, delete the old version. This is required so auto-selection works.
   rm linux-config-6.1.92
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
