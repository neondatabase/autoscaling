ARG GO_BASE_IMG=autoscaling-go-base:dev
FROM $GO_BASE_IMG AS builder

ARG GIT_INFO

COPY . .
# NOTE: Build env vars here must be the same as in the base image, otherwise we'll rebuild
# dependencies.
RUN CGO_ENABLED=0 go build \
    -ldflags "-X 'github.com/neondatabase/autoscaling/pkg/util.BuildGitInfo=$GIT_INFO'" \
    cmd/autoscaler-agent/main.go

FROM alpine:3.19
COPY --from=builder /workspace/main /usr/bin/autoscaler-agent
ENTRYPOINT ["/usr/bin/autoscaler-agent"]
