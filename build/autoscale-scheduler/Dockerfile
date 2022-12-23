FROM golang:1.19-alpine AS builder
WORKDIR /workspace

RUN apk add gcc musl-dev # required for gcc-go

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY pkg/ pkg/
COPY cmd/ cmd/

ARG GIT_INFO

RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -a \
    # -ldflags "-X ..." allows us to overwrite the value of a variable in a package \
    -ldflags "-X 'github.com/neondatabase/autoscaling/pkg/util.BuildGitInfo=$GIT_INFO'" \
    cmd/autoscale-scheduler/main.go

FROM alpine
COPY --from=builder /workspace/main /usr/bin/kube-scheduler
ENTRYPOINT ["/usr/bin/kube-scheduler"]
