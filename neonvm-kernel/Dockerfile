ARG UBUNTU_IMG_TAG=replaceme
ARG UBUNTU_IMG_SHA=replaceme
ARG ALPINE_IMG_TAG=replaceme
ARG ALPINE_IMG_SHA_AMD64=replaceme
ARG ALPINE_IMG_SHA_ARM64=replaceme
FROM ubuntu:$UBUNTU_IMG_TAG$UBUNTU_IMG_SHA AS check-arg
ARG KERNEL_VERSION
WORKDIR /build

# Force initial check that KERNEL_VERSION is set appropriately
RUN set -e \
    && echo "Build linux kernel ${KERNEL_VERSION}" \
    && test -n "${KERNEL_VERSION}" \
    && echo "force this as a requirement for build-kernel-deps" > /build/arg-check-succeeded

FROM ubuntu:$UBUNTU_IMG_TAG$UBUNTU_IMG_SHA AS build-deps
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
    rsync \
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

COPY echo-source-url.sh echo-source-url.sh

RUN set -e \
    && rm arg-check-succeeded \
    && mkdir -p linux-${KERNEL_VERSION} \
    && echo "downloading linux-${KERNEL_VERSION}.tar.xz" \
    && curl -sfL "$(./echo-source-url.sh "${KERNEL_VERSION}")" -o "linux-${KERNEL_VERSION}.tar.xz" \
    && echo "unpacking kernel archive" \
    && tar --strip-components=1 -C linux-${KERNEL_VERSION} -xf linux-${KERNEL_VERSION}.tar.xz

# Apply any patches
COPY patches patches
RUN set -e \
    && cd linux-${KERNEL_VERSION} \
    && for f in $(ls -1 ../patches | sort); do echo "Applying $f..."; patch -p1 < ../patches/$f; done

FROM build-kernel-deps AS build-tools-deps
RUN apt-get update && apt-get install -y \
    clang \
    llvm-dev \
    libdw-dev \
    libslang2-dev \
    libbfd-dev \
    libiberty-dev \
    libcap-dev \
    libunwind-dev \
    zlib1g-dev \
    libzstd-dev \
    liblzma-dev \
    libaio-dev \
    libtraceevent-dev \
    libpfm4-dev \
    systemtap-sdt-dev \
    musl \
    musl-dev \
    musl-tools \
    pkg-config

### Cross-compilation related steps

# Build the kernel on amd64
FROM build-kernel-deps AS build_kernel_amd64
ARG KERNEL_VERSION
ADD linux-config-amd64-${KERNEL_VERSION} linux-${KERNEL_VERSION}/.config
ARG VERSION_SUFFIX
RUN cd linux-${KERNEL_VERSION} \
    && make ARCH=x86_64 CROSS_COMPILE=x86_64-linux-gnu- EXTRAVERSION="$VERSION_SUFFIX" -j `nproc` \
    && make ARCH=x86_64 CROSS_COMPILE=x86_64-linux-gnu- modules_prepare -j `nproc` \
    && make ARCH=x86_64 CROSS_COMPILE=x86_64-linux-gnu- modules -j `nproc` \
    && ls -l && ls -l kernel \
    && zstd -19 kernel/kheaders.ko -o kheaders.ko.zst \
    && mkdir -p /build/linux-headers/build \
    && mkdir -p /build/linux-headers/kernel/kernel \
    && mkdir -p /build/linux-headers/build/arch/x86 \
    && make headers_install INSTALL_HDR_PATH=/build/linux-headers/build \
    && rsync -a arch/x86/include /build/linux-headers/build/arch/x86/ \
    && cp kheaders.ko.zst /build/linux-headers/kernel/kernel/kheaders.ko.zst \
    && cp -a include arch/x86/include scripts Module.symvers .config Makefile /build/linux-headers/build

FROM build-tools-deps AS build_tools_amd64
ARG KERNEL_VERSION
ADD linux-config-amd64-${KERNEL_VERSION} linux-${KERNEL_VERSION}/.config
RUN cd linux-${KERNEL_VERSION}/tools/perf \
    && make ARCH=x86_64 CROSS_COMPILE=x86_64-linux-gnu- LDFLAGS="-static -L/usr/lib/x86_64-linux-gnu -Wl,--whole-archive -lzstd -Wl,--no-whole-archive" NO_LIBPERL=1 NO_GTK2=1 NO_SDT=1 NO_JEVENTS=1

# Copy the kernel image to a separate step
# Use alpine so that `cp` is available when loading custom kernels for the runner pod.
# See the neonvm controller's pod creation logic for more detail.
FROM --platform=linux/amd64 alpine:$ALPINE_IMG_TAG$ALPINE_IMG_SHA_AMD64 AS kernel_amd64
ARG KERNEL_VERSION
ARG VERSION_SUFFIX
COPY --from=build_kernel_amd64 /build/linux-${KERNEL_VERSION}/arch/x86/boot/bzImage /vmlinuz
COPY --from=build_kernel_amd64 /build/linux-headers /tools/lib/modules/${KERNEL_VERSION}${VERSION_SUFFIX}
COPY --from=build_tools_amd64 /build/linux-${KERNEL_VERSION}/tools/perf/perf /tools/bin/perf

# Build the kernel on arm64
FROM build-kernel-deps AS build_kernel_arm64
ARG KERNEL_VERSION
ADD linux-config-aarch64-${KERNEL_VERSION} linux-${KERNEL_VERSION}/.config
RUN cd linux-${KERNEL_VERSION} \
    && make ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- EXTRAVERSION="$VERSION_SUFFIX" -j `nproc` \
    && make ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- modules_prepare -j `nproc` \
    && make ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- modules -j `nproc` \
    && ls -l && ls -l kernel \
    && zstd -19 kernel/kheaders.ko -o kheaders.ko.zst \
    && mkdir -p /build/linux-headers/build \
    && mkdir -p /build/linux-headers/kernel/kernel \
    && mkdir -p /build/linux-headers/build/arch/arm64 \
    && make headers_install INSTALL_HDR_PATH=/build/linux-headers/build \
    && rsync -a arch/arm64/include /build/linux-headers/build/arch/arm64/ \
    && cp kheaders.ko.zst /build/linux-headers/kernel/kernel/kheaders.ko.zst \
    && cp -a include arch/arm64/include scripts Module.symvers .config Makefile /build/linux-headers/build

FROM build-tools-deps AS build_tools_arm64
ARG KERNEL_VERSION
RUN cd linux-${KERNEL_VERSION}/tools/perf \
    && make LDFLAGS="-static -L/usr/lib/aarch64-linux-gnu -Wl,--whole-archive -lzstd -Wl,--no-whole-archive" NO_LIBPERL=1 NO_GTK2=1 NO_SDT=1 NO_JEVENTS=1

# Copy the kernel image to a separate step
# Use alpine so that `cp` is available when loading custom kernels for the runner pod.
# See the neonvm controller's pod creation logic for more detail.
FROM --platform=linux/arm64 alpine:$ALPINE_IMG_TAG$ALPINE_IMG_SHA_ARM64 AS kernel_arm64
ARG KERNEL_VERSION
ARG VERSION_SUFFIX
COPY --from=build_kernel_arm64 /build/linux-${KERNEL_VERSION}/arch/arm64/boot/Image /vmlinuz
COPY --from=build_kernel_arm64 /build/linux-headers /tools/lib/modules/${KERNEL_VERSION}${VERSION_SUFFIX}
COPY --from=build_tools_arm64 /build/linux-${KERNEL_VERSION}/tools/perf/perf /tools/bin/perf

# Dummy default target without target architecture
FROM ubuntu:$UBUNTU_IMG_TAG$UBUNTU_IMG_SHA
RUN echo "No target architecture specified, can't build kernel image" && exit 1
