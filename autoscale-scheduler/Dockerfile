ARG GO_BASE_IMG=autoscaling-go-base:dev
ARG ALPINE_IMG_TAG=replaceme
ARG ALPINE_IMG_SHA=replaceme
FROM $GO_BASE_IMG AS builder

COPY . .
# NOTE: Build flags here must be the same as in the base image, otherwise we'll rebuild
# dependencies. See /go-base.Dockerfile for detail on the "why".
RUN CGO_ENABLED=0 go build autoscale-scheduler/cmd/*.go

FROM alpine:$ALPINE_IMG_TAG$ALPINE_IMG_SHA
COPY --from=builder /workspace/main /usr/bin/kube-scheduler
ENTRYPOINT ["/usr/bin/kube-scheduler"]
