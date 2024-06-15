# Build the Go binary
FROM golang:1.21 as builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY neonvm/main.go      neonvm/main.go
COPY neonvm/apis/        neonvm/apis/
COPY neonvm/controllers/ neonvm/controllers/
COPY neonvm/runner/      neonvm/runner/
COPY pkg/api/ pkg/api
COPY pkg/util pkg/util

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o /runner neonvm/runner/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o /container-mgr neonvm/runner/container-mgr/*.go

FROM alpine:3.18 as crictl

RUN apk add --no-cache \
    curl
WORKDIR /workspace
# FIXME: There's non-overlapping version support for <1.27 and >=1.27.
# We should carefully consider how we go about future-proofing this (or not).
ENV VERSION="v1.25.0"
RUN curl -L "https://github.com/kubernetes-sigs/cri-tools/releases/download/$VERSION/crictl-$VERSION-linux-amd64.tar.gz" -o crictl.tar.gz \
	&& tar zxvf crictl.tar.gz -C /

# Also build virtiofsd
FROM rust:1.78-alpine as virtiofsd
WORKDIR /workspace

RUN apk add musl-dev git libcap-ng-static libseccomp-static

# == 83057321d7920ab04c324981318ad62ee1a6d986
# Latest release (2024-06-06) as of 2024-06-15. First release including live migration.
ENV BRANCH v1.11.0

RUN git clone --depth 1 --branch $BRANCH https://gitlab.com/virtio-fs/virtiofsd
RUN env LIBCAPNG_LINK_TYPE=static LIBCAPNG_LIB_PATH=/usr/lib/ \
        LIBSECCOMP_LINK_TYPE=static LIBSECCOMP_LIB_PATH=/usr/lib \
        cargo install --path virtiofsd \
    && mv /usr/local/cargo/bin/virtiofsd /virtiofsd

# Put it together
FROM alpine:3.16

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

COPY --from=builder   /runner        /usr/bin/runner
COPY --from=builder   /container-mgr /usr/bin/container-mgr
COPY --from=crictl    /crictl        /usr/bin/crictl
COPY --from=virtiofsd /virtiofsd     /usr/bin/virtiofsd
COPY neonvm/hack/kernel/vmlinuz /vm/kernel/vmlinuz
COPY neonvm/runner/ssh_config /etc/ssh/ssh_config

ENTRYPOINT ["/sbin/tini", "--", "runner"]
