FROM golang:1.20-alpine AS builder
WORKDIR /workspace

RUN apk add gcc musl-dev # gcc (and therefore musl-dev) is required for cgo extensions

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY neonvm/apis      neonvm/apis
COPY pkg/api          pkg/api
COPY pkg/informant    pkg/informant
COPY pkg/util         pkg/util
COPY cmd/vm-informant cmd/vm-informant

ARG GIT_INFO

RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -a \
	# future compat: don't modify go.mod if we have a vendor directory \
	-mod readonly \
    # -ldflags "-X ..." allows us to overwrite the value of a variable in a package \
    -ldflags "-X 'github.com/neondatabase/autoscaling/pkg/util.BuildGitInfo=$GIT_INFO'" \
    cmd/vm-informant/main.go

FROM alpine
COPY --from=builder /workspace/main /usr/bin/vm-informant
ENTRYPOINT ["/usr/bin/vm-informant"]
