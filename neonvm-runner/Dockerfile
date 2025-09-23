ARG GO_BASE_IMG=autoscaling-go-base:dev
ARG ALPINE_IMG_TAG=replaceme
ARG ALPINE_IMG_SHA=replaceme
FROM $GO_BASE_IMG AS builder

COPY . .
# Build
RUN CGO_ENABLED=0 go build -o /runner neonvm-runner/cmd/*.go

FROM alpine:$ALPINE_IMG_TAG$ALPINE_IMG_SHA

RUN apk add --no-cache \
    bpftrace \
    busybox-extras \
    cgroup-tools \
    conntrack-tools \
    coreutils \
    curl \
    dnsmasq \
    e2fsprogs \
    iproute2 \
    iptables \
    jq \
    lsof \
    openssh \
    qemu-img \
    qemu-system-aarch64 \
    qemu-system-x86_64 \
    screen \
    socat \
    tcpdump \
    tini

COPY neonvm-kernel/tools.img /vm/tools.img
COPY neonvm-runner/ssh_config /etc/ssh/ssh_config

ARG TARGET_ARCH
COPY neonvm-kernel/vmlinuz-$TARGET_ARCH /vm/kernel/vmlinuz

# Specialized firmware is only required for ARM, so we copy the directory, which will be empty if
# there is no firmware needed. If not empty, there will be a QEMU_EFI.fd file.
ARG FIRMWARE_ARCH
ARG FIRMWARE_OS
COPY neonvm-runner/external-firmware/$FIRMWARE_ARCH-$FIRMWARE_OS /vm/firmware

COPY --from=builder /runner /usr/bin/runner

ENTRYPOINT ["/sbin/tini", "--", "runner"]
