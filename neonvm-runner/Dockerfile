ARG GO_BASE_IMG=autoscaling-go-base:dev
FROM $GO_BASE_IMG AS builder

COPY . .
# Build
RUN CGO_ENABLED=0 go build -o /runner neonvm-runner/cmd/*.go

FROM alpine:3.19.7@sha256:e5d0aea7f7d2954678a9a6269ca2d06e06591881161961ea59e974dff3f12377

RUN apk add --no-cache \
    busybox-extras \
    cgroup-tools \
    coreutils \
    curl \
    dnsmasq \
    e2fsprogs \
    iproute2 \
    iptables \
    jq \
    openssh \
    qemu-img \
    qemu-system-aarch64 \
    qemu-system-x86_64 \
    screen \
    socat \
    tini

COPY --from=builder /runner /usr/bin/runner
COPY neonvm-kernel/vmlinuz /vm/kernel/vmlinuz
COPY neonvm-runner/ssh_config /etc/ssh/ssh_config
# QEMU_EFI used only by runner running on the arm architecture
RUN wget https://releases.linaro.org/components/kernel/uefi-linaro/16.02/release/qemu64/QEMU_EFI.fd -O /vm/QEMU_EFI_ARM.fd

ENTRYPOINT ["/sbin/tini", "--", "runner"]
