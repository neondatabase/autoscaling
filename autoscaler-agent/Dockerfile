ARG GO_BASE_IMG=autoscaling-go-base:dev
ARG ALPINE_IMG_TAG=replaceme
ARG ALPINE_IMG_SHA=replaceme
FROM $GO_BASE_IMG AS builder

COPY . .
# NOTE: Build env vars here must be the same as in the base image, otherwise we'll rebuild
# dependencies.
RUN CGO_ENABLED=0 go build autoscaler-agent/cmd/*.go

FROM alpine:$ALPINE_IMG_TAG$ALPINE_IMG_SHA
COPY --from=builder /workspace/main /usr/bin/autoscaler-agent
ENTRYPOINT ["/usr/bin/autoscaler-agent"]
