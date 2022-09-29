FROM postgres:14-alpine AS rootfs

RUN apk add --no-cache dhclient

RUN rm -f    /sbin/init
ADD init     /sbin/init
RUN chmod +x /sbin/init

ADD postgresql.conf /etc/postgresql/postgresql.conf

FROM smartxworks/virtink-container-rootfs-base

COPY --from=rootfs / /rootfs
