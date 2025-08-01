ARG DISTROLESS_IMG_TAG=replaceme
ARG DISTROLESS_IMG_SHA=replaceme

# NOTE: This must match CA's builder/Dockerfile:
# https://github.com/kubernetes/autoscaler/blob/<GIT_TAG>/builder/Dockerfile
FROM golang:1.22.2@sha256:d5302d40dc5fbbf38ec472d1848a9d2391a13f93293a6a5b0b87c99dc0eaa6ae AS builder

WORKDIR /workspace

COPY ca.branch ca.branch
COPY ca.commit ca.commit

# Download the repo. It's... quite large, but thankfully this should get cached
RUN git clone -b `cat ca.branch` https://github.com/kubernetes/autoscaler
RUN CA_GIT_TAG=`cat ca.commit` && cd autoscaler && git reset --hard $CA_GIT_TAG

# Only ADD the patch after downloading, to avoid wrecking the cache
COPY ca.patch ca.patch
RUN git -C autoscaler apply ../ca.patch

RUN cd autoscaler/cluster-autoscaler \
    && CGO_ENABLED=0 go build -o ../../cluster-autoscaler main.go
#      ^^^^^^^^^^^^^
# Note: If we don't disable cgo, the final binary will be dynamically linked. This doesn't work with
# the final distroless base image we use, because the binary will require `ld` or some equivalent,
# which doesn't exist.
#
# There are other ways of solving this (see: https://www.arp242.net/static-go.html), but the easiest
# thing is to just disable cgo.

# We're done building. Copy the binary over into the final product.
#
# This is adapted from CA's Dockerfile.amd64, here:
# https://github.com/kubernetes/autoscaler/blob/cluster-autoscaler-1.24.1/cluster-autoscaler/Dockerfile.amd64
# modified in a way to use gcr.io/distroless/static:nonroot without specifying the architecture.

FROM gcr.io/distroless/static:$DISTROLESS_IMG_TAG$DISTROLESS_IMG_SHA

WORKDIR /
COPY --from=builder /workspace/cluster-autoscaler .
CMD ["/cluster-autoscaler"]
