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

COPY --from=builder /runner /usr/bin/runner
COPY neonvm-kernel/vmlinuz /vm/kernel/vmlinuz
COPY neonvm-runner/ssh_config /etc/ssh/ssh_config
# QEMU_EFI used only by runner running on the arm architecture
RUN wget https://releases.linaro.org/components/kernel/uefi-linaro/16.02/release/qemu64/QEMU_EFI.fd -O /vm/QEMU_EFI_ARM.fd

ENTRYPOINT ["/sbin/tini", "--", "runner"]
