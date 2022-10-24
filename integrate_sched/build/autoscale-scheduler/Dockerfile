FROM golang:1.19-alpine AS builder
WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY pkg/ pkg/
COPY cmd/ cmd/

RUN apk add gcc musl-dev # required for gcc-go
RUN go mod download
RUN --mount=type=cache,target=/root/.cache/go-build go build -a cmd/autoscale-scheduler/main.go

FROM alpine
COPY --from=builder /workspace/main /usr/bin/kube-scheduler
ENTRYPOINT ["/usr/bin/kube-scheduler"]
