FROM alpine:3.20 AS rootdisk

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
		udev \
	&& passwd -d root \
	&& echo 'SUBSYSTEM=="cpu", ACTION=="add", TEST=="online", ATTR{online}=="0", ATTR{online}="1"' | tee /lib/udev/rules.d/80-hotplug-cpu.rules

ADD motd     /etc/motd
RUN rm -f    /sbin/init
ADD init     /sbin/init
RUN chmod +x /sbin/init

# postgresql stuff
RUN set -e \
	&& apk add --no-cache \
		su-exec \
		postgresql14
# init postgres
ENV PGDATA=/var/lib/postgresql
RUN set -e \
    && mkdir -p ${PGDATA} /run/postgresql \
    && chown -R postgres:postgres ${PGDATA} /run/postgresql \
    && su-exec postgres pg_ctl init

ADD postgresql.conf /etc/postgresql/
ADD pg_hba.conf     /etc/postgresql/


FROM alpine:3.20 AS image

ARG DISK_SIZE=2G

COPY --from=rootdisk / /rootdisk
RUN set -e \
    && apk add --no-cache qemu-img e2fsprogs \
    && mkfs.ext4 -d /rootdisk /disk.raw ${DISK_SIZE} \
    && qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on /disk.raw /disk.qcow2 \
    && rm -f /disk.raw \
    && qemu-img info /disk.qcow2

FROM alpine:3.20

COPY --from=image /disk.qcow2 /
