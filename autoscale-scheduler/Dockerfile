ARG GO_BASE_IMG=autoscaling-go-base:dev
FROM $GO_BASE_IMG AS builder

COPY . .
# NOTE: Build flags here must be the same as in the base image, otherwise we'll rebuild
# dependencies. See /go-base.Dockerfile for detail on the "why".
RUN CGO_ENABLED=0 go build autoscale-scheduler/cmd/*.go

FROM alpine:3.19.7@sha256:e5d0aea7f7d2954678a9a6269ca2d06e06591881161961ea59e974dff3f12377
COPY --from=builder /workspace/main /usr/bin/kube-scheduler
ENTRYPOINT ["/usr/bin/kube-scheduler"]
