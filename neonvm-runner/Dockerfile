ARG GO_BASE_IMG=autoscaling-go-base:dev
FROM $GO_BASE_IMG AS builder

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
    qemu-img \
    cgroup-tools \
    openssh

COPY --from=builder /runner /usr/bin/runner
COPY neonvm-kernel/vmlinuz /vm/kernel/vmlinuz
COPY neonvm-runner/ssh_config /etc/ssh/ssh_config

ENTRYPOINT ["/sbin/tini", "--", "runner"]
