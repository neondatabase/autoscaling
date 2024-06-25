FROM golang:1.21-alpine AS chef
RUN go install github.com/sharnoff/go-chef@ee64ced765864b0b55c9afc33435529681673553
WORKDIR /workspace

FROM chef AS planner
COPY . .
RUN go-chef --prepare recipe.json

# Build the manager binary
FROM chef AS builder

# Build dependencies
COPY --from=planner /workspace/recipe.json recipe.json
RUN CGO_ENABLED=0 go-chef --cook recipe.json

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum

ARG BUILDTAGS

# Copy the go source
COPY pkg/util            pkg/util
COPY neonvm/main.go      neonvm/main.go
COPY neonvm/apis/        neonvm/apis/
COPY neonvm/controllers/ neonvm/controllers/
COPY neonvm/pkg/         neonvm/pkg/
COPY neonvm/client/      neonvm/client/
COPY pkg/api/ pkg/api
COPY pkg/util pkg/util

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 go build -tags=${BUILDTAGS} -o manager neonvm/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot

ARG VM_RUNNER_IMAGE=runner:dev
ENV VM_RUNNER_IMAGE=${VM_RUNNER_IMAGE}

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
