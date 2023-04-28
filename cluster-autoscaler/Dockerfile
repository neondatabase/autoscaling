# NOTE: This must match CA's builder/Dockerfile:
# https://github.com/kubernetes/autoscaler/blob/<GIT_TAG>/builder/Dockerfile
FROM golang:1.19.7 AS builder

WORKDIR /workspace

ARG CA_GIT_TAG

# Download the repo. It's... quite large, but thankfuly this should get cached
RUN git clone --depth=1 -b $CA_GIT_TAG https://github.com/kubernetes/autoscaler

# Only ADD the patch after downloading, to avoid wrecking the cache
ADD ca.patch ca.patch
RUN git -C autoscaler apply ../ca.patch

RUN cd autoscaler/cluster-autoscaler \
    && go build -o ../../cluster-autoscaler main.go

# We're done buiding. Copy the binary over into the final product.
#
# This is adapted from CA's Dockerfile.amd64, here:
# https://github.com/kubernetes/autoscaler/blob/cluster-autoscaler-1.24.1/cluster-autoscaler/Dockerfile.amd64

FROM gcr.io/distroless/static:nonroot-amd64

WORKDIR /
COPY --from=builder /workspace/cluster-autoscaler .
CMD ["/cluster-autoscaler"]
