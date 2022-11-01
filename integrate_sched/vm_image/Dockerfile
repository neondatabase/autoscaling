FROM alpine:3.16 as rootfs

# prometheus-node-exporter provides /metrics on :9001 (once we start it)
RUN set -e \
	&& apk add --no-cache \
		jq \
		curl \
		ca-certificates \
		util-linux-misc \
		prometheus-node-exporter \
	&& passwd -d root

RUN rm -f    /sbin/init
ADD init     /sbin/init
RUN chmod +x /sbin/init

# virtink uses /sbin/hotplug
ADD hotplug.sh /sbin/hotplug
RUN chmod +x /sbin/hotplug

# postgresql stuff
RUN set -e \
	&& apk add --no-cache \
		su-exec \
		postgresql14
ADD postgresql.conf /etc/postgresql.conf

# ssh stuff
RUN set -e \
    && apk add --no-cache \
        openssh-server
ADD sshd_config /etc/ssh/sshd_config
ADD ssh_id_rsa.pub /etc/ssh/authorized_keys
# 'authorized_keys' needs to have the right mode bits, otherwise it'll get ignored by sshd
RUN set -e \
    && chmod 600 /etc/ssh/authorized_keys

# set the greeting message on ssh logins
RUN echo -e 'Welcome to Alpine!\n ~ This is the VM :) ~' >/etc/motd


FROM alpine:3.16
RUN set -e \
    && apk add --no-cache \
        e2fsprogs # required for mkfs.ext4 in rootfs-entrypoint.sh

ADD rootfs-entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
COPY --from=rootfs / /rootfs

# dhcp client hack for read-only root fs
RUN sed -i 's/^RESOLV_CONF=.*/RESOLV_CONF=\/run\/resolv\.conf/' /rootfs/usr/share/udhcpc/default.script
RUN ln -sf ../run/resolv.conf /rootfs/etc/resolv.conf
