FROM ubuntu:jammy AS rootfs

RUN apt-get update -y && \
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd-sysv udev lsb-release cloud-init sudo openssh-server vim postgresql prometheus-node-exporter && \
    rm -rf /var/lib/apt/lists/*

ADD hotplug.sh /sbin/hotplug
RUN chmod +x /sbin/hotplug

RUN echo 'host    all             all             10.0.0.0/8        trust' >> /etc/postgresql/14/main/pg_hba.conf
ADD postgresql.conf /etc/postgresql/14/main/postgresql.conf

#FROM smartxworks/virtink-container-rootfs-base
FROM smartxworks/virtink-container-rootfs-ubuntu

COPY --from=rootfs / /rootfs
