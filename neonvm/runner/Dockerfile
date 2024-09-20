ARG GO_BASE_IMG=autoscaling-go-base:dev
FROM $GO_BASE_IMG AS builder

COPY . .
# Build
RUN CGO_ENABLED=0 go build -o /runner neonvm/runner/main.go
RUN CGO_ENABLED=0 go build -o /container-mgr neonvm/runner/container-mgr/*.go

FROM alpine:3.19 AS crictl

RUN apk add --no-cache \
    curl
WORKDIR /workspace
# FIXME: There's non-overlapping version support for <1.27 and >=1.27.
# We should carefully consider how we go about future-proofing this (or not).
ENV VERSION="v1.27.1"
RUN curl -L "https://github.com/kubernetes-sigs/cri-tools/releases/download/$VERSION/crictl-$VERSION-linux-arm64.tar.gz" -o crictl.tar.gz \
	&& tar zxvf crictl.tar.gz -C /

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

COPY --from=builder /runner /usr/bin/runner
COPY --from=builder /container-mgr /usr/bin/container-mgr
COPY --from=crictl /crictl /usr/bin/crictl
COPY neonvm/hack/kernel/vmlinuz /vm/kernel/vmlinuz
COPY neonvm/runner/ssh_config /etc/ssh/ssh_config

ENTRYPOINT ["/sbin/tini", "--", "runner"]
