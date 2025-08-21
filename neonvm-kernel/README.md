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

After copying your old `.config` into the new kernel directory, you can:

```sh
# Quick upgrade using automatic defaults for new config options (non-interactive)
make olddefconfig ARCH=x86_64  # or ARCH=arm64

# OR

# Interactively review all new config options
make oldconfig ARCH=x86_64  # or ARCH=arm64
```

### On amd64 (x64)

1. On the host, run:
   ```sh
   cd neonvm-kernel # this directory
   docker build --build-arg KERNEL_VERSION=$NEW_VERSION --target build-deps -t kernel-build-deps .
   docker run --rm -v $PWD:/host --name kernel-build -it kernel-build-deps bash
   ```
2. Then, inside the container, run:
   ```sh
   cd linux-$NEW_VERSION
   cp /host/linux-config-amd64-6.6.64 .config # Copy current config in
   make menuconfig ARCH=x86_64
   # do nothing; just save and exit, overwriting .config
   cp .config /host/linux-config-amd64-$NEW_VERSION # NOTE: Different from existing!
   ```
3. Back on the host, finish with:
   ```sh
   # compare the two versions
   diff linux-config-amd64-6.6.64 linux-config-amd64-$NEW_VERSION
   # If all looks good, delete the old version. This is required so auto-selection works.
   rm linux-config-amd64-6.6.64
   ```

### On arm64 (aarch64 ARM)

1. On the host, run:
   ```sh
   cd neonvm-kernel # this directory
   docker build --build-arg KERNEL_VERSION=$NEW_VERSION --target build-deps -t kernel-build-deps .
   docker run --rm -v $PWD:/host --name kernel-build -it kernel-build-deps bash
   ```
2. Then, inside the container, run:
   ```sh
   cd linux-$NEW_VERSION
   cp /host/linux-config-aarch64-6.6.64 .config # Copy current config in
   make menuconfig ARCH=arm64
   # do nothing; just save and exit, overwriting .config
   cp .config /host/linux-config-aarch64-$NEW_VERSION # NOTE: Different from existing!
   ```
3. Back on the host, finish with:
   ```sh
   # compare the two versions
   diff linux-config-aarch64-6.6.64 linux-config-aarch64-$NEW_VERSION
   # If all looks good, delete the old version. This is required so auto-selection works.
   rm linux-config-aarch64-6.6.64
   ```

Afterwards, it's probably also good to do a search-and-replace repo-wide to update all places that
mention the old kernel version.

## Adjusting the config

To adjust the kernel config, try the following from this directory:

```sh
docker build --build-arg KERNEL_VERSION=6.6.64 --platform linux/x86_64 --target build-deps -t kernel-build-deps .
docker run --rm -v $PWD:/host --name kernel-build -it kernel-build-deps bash
# inside that bash shell, do the menuconfig, then copy-out the config to /host
```

## Tools

The docker image also contains the **tools** - useful stuff which can
**ONLY** be provided at the same time as the kernel. That means, either
those are such tools that have a direct dependency on the kernel
version, or the tools built from the same repository as the kernel
(like `perf`), or anything else dependent on the kernel.

The `tools` is a **root** directory for this environment which provides
them. Its structure is the same as if the internals were installed onto
the host the usual way, so `<tools>/bin` is the path for the binaries,
`<tools>/include` for the include files, `<tools>/lib` for the libraries
and so on.

To make it easier to deploy it inside the VM, the tools are packaged
into an ext4 filesystem and then put into a disk image file with one
partition with this filesystem. Later it can be simply attached to the
environment (`qemu`, for example), or mounted to the local filesystem.

The image file is called `tools.img` and is labelled `vm-tools`.
