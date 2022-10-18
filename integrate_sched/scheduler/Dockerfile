FROM golang:1.19-alpine AS builder
WORKDIR /workspace

COPY . .
RUN apk add gcc musl-dev # required for gcc-go
RUN go mod download
RUN --mount=type=cache,target=/root/.cache/go-build go build -a main.go

FROM alpine
COPY --from=builder /workspace/main /usr/bin/kube-scheduler
ENTRYPOINT ["/usr/bin/kube-scheduler"]
