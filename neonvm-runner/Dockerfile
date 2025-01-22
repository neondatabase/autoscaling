ARG GO_BASE_IMG=autoscaling-go-base:dev
ARG IMG_DAEMON=daemon:dev

FROM $IMG_DAEMON AS neonvm-daemon-loader

#########################################################################################
#
# neonvm-guest: Build 'vector' into /usr/local/bin in layer 'vector-build'
#
#########################################################################################
FROM debian:bookworm-slim AS vector-build
# Temporarily
RUN apt update
RUN apt install --no-install-recommends --no-install-suggests -y \
  ca-certificates \
  wget

# Install vector.dev binary
RUN set -e \
    && ARCH=$( [ "$TARGET_ARCH" = "linux/arm64" ] && echo "aarch64" || echo "x86_64") \
    && wget https://packages.timber.io/vector/0.26.0/vector-${ARCH}-unknown-linux-musl.tar.gz -O - \
    | tar xzvf - --strip-components 3 -C /usr/local/bin/ ./vector-${ARCH}-unknown-linux-musl/bin/vector

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
    systemd-resolved \
    libnss-resolve \
    udev

# Convenience
RUN apt install --no-install-recommends --no-install-suggests -y \
    nano \
    less

# neonvm-runner/neonvm-guest/files contains various configuration files
COPY neonvm-runner/neonvm-guest/files/etc/ /etc/
COPY neonvm-runner/neonvm-guest/files/lib/ /lib/
RUN mkdir -p /neonvm/run

RUN UID_MIN=900 GID_MIN=900 useradd vector
COPY --from=vector-build /usr/local/bin/vector /usr/local/bin/
COPY --from=neonvm-daemon-loader /neonvmd /usr/local/bin/neonvmd

RUN systemctl enable \
    systemd-networkd.service \
    mnt-ssh.mount \
    neonvm-runtime.mount \
    ssh \
    vector \
    neonvmd.service \
    neonvm-payload.mount \
    systemd-nspawn@neonvm-payload

# FIXME: on arm it's different tty, not ttyS0
RUN systemctl enable serial-getty@ttyS0.service

# Disable services we don't need
RUN systemctl disable \
    dpkg-db-backup.timer \
    e2scrub_all.timer \
    systemd-tmpfiles-clean.timer \
    apt-daily.timer \
    apt-daily-upgrade.timer

# This is where machinectl looks for images
RUN ln -s /neonvm/payload /var/lib/machines/neonvm-payload

# A tiny quality-of-life thing: add "machinectl shell neonvm-payload" to bash history,
# so after ssh'ing in, you just need to hit arrow-up +enter
RUN echo "machinectl shell neonvm-payload" > /root/.bash_history

RUN echo "ForwardToConsole=yes" >> /etc/systemd/journald.conf

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

# Remove the DNS configuration that came with the 'bookworm-slim' base
# image. We have configured systemd to configure these things properly
# at startup.
RUN rm /rootdisk/etc/resolv.conf /rootdisk/etc/hostname

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
