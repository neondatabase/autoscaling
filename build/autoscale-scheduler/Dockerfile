FROM golang:1.23-alpine AS builder
WORKDIR /workspace

RUN apk add gcc musl-dev # gcc (and therefore musl-dev) is required for cgo extensions

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY neonvm/apis             neonvm/apis
COPY neonvm/client           neonvm/client
COPY pkg/api                 pkg/api
COPY pkg/plugin              pkg/plugin
COPY pkg/util                pkg/util
COPY cmd/autoscale-scheduler cmd/autoscale-scheduler

ARG GIT_INFO

RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -a \
	# future compat: don't modify go.mod if we have a vendor directory \
	-mod readonly \
    # -ldflags "-X ..." allows us to overwrite the value of a variable in a package \
    -ldflags "-X 'github.com/neondatabase/autoscaling/pkg/util.BuildGitInfo=$GIT_INFO'" \
    cmd/autoscale-scheduler/main.go

FROM alpine
COPY --from=builder /workspace/main /usr/bin/kube-scheduler
ENTRYPOINT ["/usr/bin/kube-scheduler"]
