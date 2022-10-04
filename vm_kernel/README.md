## Linux kernel in docker image for VMs

Linux kernel compatible with cloud-hypervisor https://github.com/cloud-hypervisor/linux.git but with some settings overriden

- mak changes in [kernel_config_override](kernel_config_override)
- inspect bash script [build_kernel.sh](build_kernel.sh) (probably change variable `DOCKER_IMAGE="cicdteam/clh-kernel"` if you want store kernel in your own docker registry)
- run script
- use kernel in virtink VM templates:


```
spec:
  instance:
    kernel:
      image: cicdteam/clh-kernel:5.15.12
      imagePullPolicy: IfNotPresent
      cmdline: "console=ttyS0 root=/dev/vda rw"
```
