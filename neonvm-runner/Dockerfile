ARG GO_BASE_IMG=autoscaling-go-base:dev
ARG IMG_DAEMON=daemon:dev

FROM $IMG_DAEMON AS neonvm-daemon-loader

#########################################################################################
#
# neonvm-guest: Build 'vector' into /neonvm/bin in layer 'vector-build'
#
#########################################################################################
FROM debian:bookworm-slim AS vector-build
# Temporarily
RUN apt update
RUN apt install --no-install-recommends --no-install-suggests -y \
  ca-certificates \
  wget

# Install vector.dev binary
RUN mkdir -p /neonvm/bin
RUN set -e \
    && ARCH=$( [ "$TARGET_ARCH" = "linux/arm64" ] && echo "aarch64" || echo "x86_64") \
    && wget https://packages.timber.io/vector/0.26.0/vector-${ARCH}-unknown-linux-musl.tar.gz -O - \
    | tar xzvf - --strip-components 3 -C /neonvm/bin/ ./vector-${ARCH}-unknown-linux-musl/bin/vector

#########################################################################################
#
# neonvm-guest: Build final layer
#
#########################################################################################

FROM debian:bookworm-slim AS neonvm-guest-rootfs

# Temporarily
RUN apt update
RUN apt install --no-install-recommends --no-install-suggests -y \
    systemd \
    cgroup-tools \
    chrony \
    openssh-server \
    quota \
    systemd-container \
    systemd-sysv \
    udev

# Convenience
RUN apt install --no-install-recommends --no-install-suggests -y \
    nano \
    less

# FIXME: on arm it's different tty
COPY neonvm-runner/neonvm-guest/files/serial-getty-autologin.conf /etc/systemd/system/serial-getty@ttyS0.service.d/autologin.conf
RUN systemctl enable   serial-getty@ttyS0.service

COPY neonvm-runner/neonvm-guest/files/50-dhcp.network /etc/systemd/network/
RUN systemctl enable systemd-networkd.service

# udev rule for auto-online hotplugged CPUs
COPY neonvm-runner/neonvm-guest/files/99-hotplug-cpu.rules /lib/udev/rules.d/99-hotplug-cpu.rules

RUN useradd vector
COPY --from=vector-build /neonvm/ /neonvm/
RUN mkdir /etc/vector
COPY neonvm-runner/neonvm-guest/files/vector.yaml /etc/vector/vector.yaml
COPY neonvm-runner/neonvm-guest/files/vector.service /etc/systemd/system/vector.service
RUN systemctl enable vector

COPY --from=neonvm-daemon-loader /neonvmd /neonvm/bin/neonvmd
COPY neonvm-runner/neonvm-guest/files/neonvmd.service /etc/systemd/system
RUN systemctl enable neonvmd.service

# FIXME: the default debian config configures to use debian's pool. Disable it?
COPY neonvm-runner/neonvm-guest/files/chrony.conf /etc/chrony/conf.d/neonvm-chrony.conf

COPY neonvm-runner/neonvm-guest/files/mnt-ssh.mount /etc/systemd/system/
# FIXME: or should we replace the config wholesale?
COPY neonvm-runner/neonvm-guest/files/sshd_config /etc/ssh/sshd_config.d/neonvm-sshd.conf
COPY neonvm-runner/neonvm-guest/files/neonvm-banner /etc/ssh/neonvm-banner
RUN systemctl enable ssh mnt-ssh.mount

# Mount runtime image under /neonvm/runtime. It contains configuration files, generated
# dynamically by neonvm-runner
COPY neonvm-runner/neonvm-guest/files/neonvm-runtime.mount /etc/systemd/system/
RUN systemctl enable neonvm-runtime.mount

# Disable services we don't need
RUN systemctl disable \
    dpkg-db-backup.timer \
    e2scrub_all.timer \
    systemd-tmpfiles-clean.timer \
    apt-daily.timer \
    apt-daily-upgrade.timer

# This is where machinectl looks for images
RUN ln -s /neonvm/payload /var/lib/machines/neonvm-payload
COPY neonvm-runner/neonvm-guest/files/neonvm-payload.mount /etc/systemd/system/

COPY neonvm-runner/neonvm-guest/files/neonvm-payload-10-netns.conf /etc/systemd/system/systemd-nspawn@neonvm-payload.service.d/10-netns.conf
COPY neonvm-runner/neonvm-guest/files/neonvm-payload.nspawn /etc/systemd/nspawn/neonvm-payload.nspawn
RUN systemctl enable neonvm-payload.mount
RUN systemctl enable systemd-nspawn@neonvm-payload


#########################################################################################
#
# neonvm-guest: Build QCOW2 layer
#
#########################################################################################

FROM debian:bookworm-slim AS neonvm-guest-qcow2-img
ARG DISK_SIZE=2G

# tools for qemu disk creation
RUN apt update
RUN apt install --no-install-recommends --no-install-suggests -y \
    qemu-utils \
    e2fsprogs

COPY --from=neonvm-guest-rootfs / /rootdisk/

RUN set -e \
    && mkfs.ext4 -L neonvm-guest -d /rootdisk /disk.raw ${DISK_SIZE} \
    && qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on /disk.raw /neonvm-guest.qcow2


#########################################################################################
#
# Build the final 'neonvm-runner' image
#
#########################################################################################
FROM $GO_BASE_IMG AS neonvm-runner-builder

COPY . .
# Build
RUN CGO_ENABLED=0 go build -o /runner neonvm-runner/cmd/*.go

FROM alpine:3.19

RUN apk add --no-cache \
    tini \
    curl \
    screen \
    dnsmasq \
    iptables \
    iproute2 \
    coreutils \
    socat \
    jq \
    busybox-extras \
    e2fsprogs \
    qemu-system-x86_64 \
    qemu-system-aarch64 \
    qemu-img \
    cgroup-tools \
    openssh


COPY --from=neonvm-runner-builder /runner /usr/bin/runner
COPY neonvm-kernel/vmlinuz /vm/kernel/vmlinuz
COPY neonvm-runner/ssh_config /etc/ssh/ssh_config
# QEMU_EFI used only by runner running on the arm architecture
RUN wget https://releases.linaro.org/components/kernel/uefi-linaro/16.02/release/qemu64/QEMU_EFI.fd -O /vm/QEMU_EFI_ARM.fd

COPY --from=neonvm-guest-qcow2-img /neonvm-guest.qcow2 /

ENTRYPOINT ["/sbin/tini", "--", "runner"]
