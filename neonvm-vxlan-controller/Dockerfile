ARG GO_BASE_IMG=autoscaling-go-base:dev
FROM $GO_BASE_IMG AS builder

COPY . .
RUN CGO_ENABLED=0 go build -o /vxlan-controller neonvm-vxlan-controller/cmd/*.go

FROM alpine:3.19

ARG TARGET_ARCH
ENV TARGET_ARCH=$TARGET_ARCH

RUN apk add --no-cache \
    tini \
    dnsmasq \
    iptables \
    iproute2

# add CNI plugins
RUN set -e \
    && wget -q https://github.com/containernetworking/plugins/releases/download/v1.2.0/cni-plugins-linux-$TARGET_ARCH-v1.2.0.tgz -O cni.tgz \
    && mkdir -p /opt/cni/bin \
    && tar -xzf cni.tgz -C /opt/cni/bin \
    && rm -f cni.tgz

RUN set -e \
    && wget -q https://github.com/grpc-ecosystem/grpc-health-probe/releases/latest/download/grpc_health_probe-linux-$TARGET_ARCH -O /usr/bin/grpc_health_probe \
    && chmod +x /usr/bin/grpc_health_probe

COPY --from=builder /vxlan-controller /usr/bin/vxlan-controller

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["vxlan-controller"]
