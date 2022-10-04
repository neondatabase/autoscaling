FROM alpine:3.16 AS rootfs

RUN set -e \
	&& apk add --no-cache \
		jq \
		curl \
		ca-certificates \
		util-linux-misc \
		agetty \
	&& passwd -d root

RUN rm -f    /sbin/init
ADD init     /sbin/init
RUN chmod +x /sbin/init

FROM smartxworks/virtink-container-rootfs-base

COPY --from=rootfs / /rootfs

# dhcp client hack for read-only root fs
RUN sed -i 's/^RESOLV_CONF=.*/RESOLV_CONF=\/run\/resolv\.conf/' /rootfs/usr/share/udhcpc/default.script
RUN ln -sf ../run/resolv.conf /rootfs/etc/resolv.conf
