FROM debian:11-slim AS rootfs

RUN set -e \
	&& apt-get update \
	&& apt-get install -y --no-install-recommends \
		curl \
		iputils-ping \
		netcat \
		iproute2 \
		procps \
		lsb-release \
		isc-dhcp-client \
	&& passwd -d root

RUN rm -f    /sbin/init
ADD init     /sbin/init
RUN chmod +x /sbin/init

FROM smartxworks/virtink-container-rootfs-base

COPY --from=rootfs / /rootfs
