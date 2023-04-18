# Build the Go binary
FROM golang:1.20 as builder
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

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o /runner neonvm/runner/main.go

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
    qemu-img

COPY --from=builder /runner /usr/bin/runner
COPY neonvm/hack/vmlinuz /vm/kernel/vmlinuz

ENTRYPOINT ["/sbin/tini", "--", "runner"]
