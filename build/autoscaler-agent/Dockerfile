FROM golang:1.23-alpine AS builder
WORKDIR /workspace

RUN apk add gcc musl-dev # gcc (and therefore musl-dev) is required for cgo extensions

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY neonvm/apis          neonvm/apis
COPY neonvm/client        neonvm/client
COPY pkg/agent            pkg/agent
COPY pkg/api              pkg/api
COPY pkg/billing          pkg/billing
COPY pkg/util             pkg/util
COPY cmd/autoscaler-agent cmd/autoscaler-agent

ARG GIT_INFO

RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -a \
	# future compat: don't modify go.mod if we have a vendor directory \
	-mod readonly \
    # -ldflags "-X ..." allows us to overwrite the value of a variable in a package \
    -ldflags "-X 'github.com/neondatabase/autoscaling/pkg/util.BuildGitInfo=$GIT_INFO'" \
    cmd/autoscaler-agent/main.go

FROM alpine:3.19
COPY --from=builder /workspace/main /usr/bin/autoscaler-agent
ENTRYPOINT ["/usr/bin/autoscaler-agent"]
