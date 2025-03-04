# Force initial check that KERNEL_VERSION is set appropriately
FROM ubuntu:24.04 AS check-arg
ARG KERNEL_VERSION
WORKDIR /build

RUN set -e \
    && echo "Build linux kernel ${KERNEL_VERSION}" \
    && test -n "${KERNEL_VERSION}" \
    && echo "force this as a requirement for build-deps" > /build/arg-check-succeeded

FROM ubuntu:24.04 AS build-deps
WORKDIR /build

RUN apt-get update && apt-get -y install \
    curl \
    ca-certificates \
    build-essential \
    gcc-x86-64-linux-gnu \
    gcc-aarch64-linux-gnu \
    flex \
    bison \
    libelf-dev \
    bc \
    libssl-dev \
    python3 \
    cpio \
    zstd \
    libncurses-dev

# Only require check-arg at this point, so the 'apt-get install' above is definitely cached.
# We need to copy something in, otherwise 'docker buildx build' will just completely drop the
# check-arg stage.
COPY --from=check-arg /build/arg-check-succeeded arg-check-succeeded

# the ARG must be down here (specifically: not before 'apt-get install'), else docker seems to
# assume that KERNEL_VERSION could influence the 'apt-get install' and refuses to cache it.
# (maybe they're just exposed as environment variables to RUN?)
# This all as of Docker 26.1.4 - 2024-06-07.
ARG KERNEL_VERSION

RUN set -e \
    && rm arg-check-succeeded \
    && mkdir -p linux-${KERNEL_VERSION} \
    && echo "downloading linux-${KERNEL_VERSION}.tar.xz" \
    && MAJOR=`echo ${KERNEL_VERSION} | sed -E 's/^([0-9]+)\.[0-9]+\.[0-9]+$/\1/'` \
    && curl -sfL https://cdn.kernel.org/pub/linux/kernel/v${MAJOR}.x/linux-${KERNEL_VERSION}.tar.xz -o linux-${KERNEL_VERSION}.tar.xz \
    && echo "unpacking kernel archive" \
    && tar --strip-components=1 -C linux-${KERNEL_VERSION} -xf linux-${KERNEL_VERSION}.tar.xz



### Cross-compilation related steps

# Build the kernel on amd64
FROM build-deps AS build_amd64
ARG KERNEL_VERSION
ADD linux-config-amd64-${KERNEL_VERSION} linux-${KERNEL_VERSION}/.config
RUN cd linux-${KERNEL_VERSION} && make ARCH=x86_64 CROSS_COMPILE=x86_64-linux-gnu- -j `nproc`

# Copy the kernel image to a separate step
# Use alpine so that `cp` is available when loading custom kernels for the runner pod.
# See the neonvm controller's pod creation logic for more detail.
FROM --platform=linux/amd64 alpine:3.19 AS kernel_amd64
ARG KERNEL_VERSION
COPY --from=build_amd64 /build/linux-${KERNEL_VERSION}/arch/x86/boot/bzImage /vmlinuz

# Build the kernel on arm64
FROM build-deps AS build_arm64
ARG KERNEL_VERSION
ADD linux-config-aarch64-${KERNEL_VERSION} linux-${KERNEL_VERSION}/.config
RUN cd linux-${KERNEL_VERSION} && make ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- -j `nproc`

# Copy the kernel image to a separate step
# Use alpine so that `cp` is available when loading custom kernels for the runner pod.
# See the neonvm controller's pod creation logic for more detail.
FROM --platform=linux/arm64 alpine:3.19 AS kernel_arm64
ARG KERNEL_VERSION
COPY --from=build_arm64 /build/linux-${KERNEL_VERSION}/arch/arm64/boot/Image /vmlinuz

# Dummy default target without target architecture
FROM alpine:3.19
RUN echo "No target architecture specified, can't build kernel image" && exit 1
