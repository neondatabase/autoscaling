FROM alpine:3.16 AS rootdisk

RUN set -e \
	&& apk add --no-cache \
		jq \
		curl \
		ca-certificates \
		util-linux-misc \
		agetty \
		bind-tools \
		tcpdump \
		coreutils \
	&& passwd -d root

ADD motd     /etc/motd
RUN rm -f    /sbin/init
ADD init     /sbin/init
RUN chmod +x /sbin/init

FROM alpine:3.16 AS image

ARG DISK_SIZE=2G

COPY --from=rootdisk / /rootdisk
RUN set -e \
    && apk add --no-cache qemu-img e2fsprogs \
    && mkfs.ext4 -d /rootdisk /disk.raw ${DISK_SIZE} \
    && qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on /disk.raw /disk.qcow2 \
    && rm -f /disk.raw \
    && qemu-img info /disk.qcow2

FROM alpine:3.16

COPY --from=image /disk.qcow2 /
