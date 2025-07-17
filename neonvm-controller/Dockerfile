ARG GO_BASE_IMG=autoscaling-go-base:dev
ARG DISTROLESS_IMG_TAG=replaceme
ARG DISTROLESS_IMG_SHA=replaceme
FROM $GO_BASE_IMG AS builder

# Build the manager binary
COPY . .
RUN CGO_ENABLED=0 go build -o manager neonvm-controller/cmd/*.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:$DISTROLESS_IMG_TAG$DISTROLESS_IMG_SHA

WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
