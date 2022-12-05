FROM golang:1.19-alpine AS builder
WORKDIR /workspace

RUN apk add gcc musl-dev curl # gcc and musl-dev required for gcc-go

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY pkg/ pkg/
COPY cmd/ cmd/

RUN --mount=type=cache,target=/root/.cache/go-build go build -a cmd/autoscaler-agent/main.go

FROM alpine
COPY --from=builder /workspace/main /usr/bin/autoscaler-agent
ENTRYPOINT ["/usr/bin/autoscaler-agent"]
