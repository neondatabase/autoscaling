FROM golang:1.19-alpine AS builder
WORKDIR /workspace

RUN apk add gcc musl-dev curl # gcc and musl-dev required for gcc-go

# Download the ch-remote binary
RUN set -eux; \
    mkdir /var/lib/cloud-hypervisor; \
    case "$(uname -m)" in \
        'x86_64') \
            curl -sLo /usr/bin/ch-remote https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v26.0/ch-remote-static; \
            ;; \
        'aarch64') \
            curl -sLo /usr/bin/ch-remote https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v26.0/ch-remote-static-aarch64; \
            ;; \
        *) echo >&2 "error: unsupported architecture '$(uname -m)'"; exit 1 ;; \
    esac; \
    chmod +x /usr/bin/ch-remote

COPY . .
RUN go mod download
RUN --mount=type=cache,target=/root/.cache/go-build go build -a main.go

FROM alpine
COPY --from=builder /usr/bin/ch-remote /usr/bin/ch-remote
COPY --from=builder /workspace/main /usr/bin/autoscaler-agent
ENTRYPOINT ["/usr/bin/autoscaler-agent"]
