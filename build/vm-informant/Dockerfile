FROM golang:1.19-alpine AS builder
WORKDIR /workspace

RUN apk add gcc musl-dev # gcc (and therefore musl-dev) is required for cgo extensions

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY pkg/ pkg/
COPY cmd/ cmd/
COPY neonvm/apis neonvm/apis
COPY neonvm/client neonvm/client

ARG GIT_INFO

# Do an intermediate build step so that there's a shared base for the various autoscaling
# components, which allows quicker "build everything" at the cost of slower "build one thing"
#
# -mod readonly is for future compat: don't modify go.mod if we have a vendor directory
RUN go build -mod readonly ./...

RUN go build -a \
	# future compat: don't modify go.mod if we have a vendor directory \
	-mod readonly \
    # -ldflags "-X ..." allows us to overwrite the value of a variable in a package \
    -ldflags "-X 'github.com/neondatabase/autoscaling/pkg/util.BuildGitInfo=$GIT_INFO'" \
    cmd/vm-informant/main.go

FROM alpine
COPY --from=builder /workspace/main /usr/bin/vm-informant
ENTRYPOINT ["/usr/bin/vm-informant"]
