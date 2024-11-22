# NOTE: This must match CA's builder/Dockerfile:
# https://github.com/kubernetes/autoscaler/blob/<GIT_TAG>/builder/Dockerfile
FROM golang:1.21.6 AS builder

WORKDIR /workspace

COPY ca.branch ca.branch
COPY ca.commit ca.commit

# Download the repo. It's... quite large, but thankfully this should get cached
RUN git clone -b `cat ca.branch` https://github.com/kubernetes/autoscaler
RUN CA_GIT_TAG=`cat ca.commit` cd autoscaler && git reset --hard $CA_GIT_TAG

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

FROM gcr.io/distroless/static:nonroot-amd64

WORKDIR /
COPY --from=builder /workspace/cluster-autoscaler .
CMD ["/cluster-autoscaler"]
