FROM golang:1.21-alpine as chef
RUN go install github.com/sharnoff/go-chef@ee64ced765864b0b55c9afc33435529681673553
WORKDIR /workspace

FROM chef as planner
COPY . .
RUN go-chef --prepare recipe.json

# Build the Go binary
FROM chef as builder

# Build dependencies
COPY --from=planner /workspace/recipe.json recipe.json
RUN CGO_ENABLED=0 go-chef --cook recipe.json

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY neonvm/tools/vxlan/controller/ neonvm/tools/vxlan/controller/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 go build -o /vxlan-controller neonvm/tools/vxlan/controller/main.go

FROM alpine:3.16

RUN apk add --no-cache \
    tini \
    dnsmasq \
    iptables \
    iproute2

# add CNI plugins
RUN set -e \
    && wget -q https://github.com/containernetworking/plugins/releases/download/v1.2.0/cni-plugins-linux-amd64-v1.2.0.tgz -O cni.tgz \
    && mkdir -p /opt/cni/bin \
    && tar -xzf cni.tgz -C /opt/cni/bin \
    && rm -f cni.tgz

RUN set -e \
    && wget -q https://github.com/grpc-ecosystem/grpc-health-probe/releases/latest/download/grpc_health_probe-linux-amd64 -O /usr/bin/grpc_health_probe \
    && chmod +x /usr/bin/grpc_health_probe

COPY --from=builder /vxlan-controller /usr/bin/vxlan-controller

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["vxlan-controller"]
