{{.SpecBuild}}

FROM {{.RootDiskImage}} AS rootdisk

# Temporarily set to root in order to do the "merge" step, so that it's possible to make changes in
# the final VM to files owned by root, even if the source image sets the user to something else.
USER root
{{.SpecMerge}}

FROM {{.NeonvmDaemonImage}} AS neonvm-daemon-loader

FROM busybox:1.35.0-musl AS busybox-loader

FROM alpine:3.19 AS vm-runtime
ARG TARGET_ARCH
RUN set -e && mkdir -p /neonvm/bin /neonvm/runtime /neonvm/config 
# add busybox
COPY --from=busybox-loader /bin/busybox /neonvm/bin/busybox

RUN set -e \
    chmod +x /neonvm/bin/busybox \
    && /neonvm/bin/busybox --install -s /neonvm/bin 

COPY helper.move-bins.sh /helper.move-bins.sh

# add udevd and agetty (with shared libs)
RUN set -e \
	&& apk add --no-cache --no-progress --quiet \
		acpid \
		udev \
		agetty \
		su-exec \
        util-linux-misc \
        cgroup-tools \
		e2fsprogs-extra \
		blkid \
		flock \
	&& mkdir -p /neonvm/lib \
	&& /helper.move-bins.sh \
		acpid \
		udevd \
		udevadm \
		agetty \
		su-exec \
        cgexec \
		resize2fs \
		blkid \
		flock \
	&& mv /usr/share/udhcpc/default.script /neonvm/bin/udhcpc.script \
	&& sed -i 's/#!\/bin\/sh/#!\/neonvm\/bin\/sh/' /neonvm/bin/udhcpc.script \
	&& sed -i 's/export PATH=.*/export PATH=\/neonvm\/bin/' /neonvm/bin/udhcpc.script

# Install vector.dev binary
RUN set -e \
    && ARCH=$( [ "$TARGET_ARCH" = "linux/arm64" ] && echo "aarch64" || echo "x86_64") \
    && wget https://packages.timber.io/vector/0.26.0/vector-${ARCH}-unknown-linux-musl.tar.gz -O - \
    | tar xzvf - --strip-components 3 -C /neonvm/bin/ ./vector-${ARCH}-unknown-linux-musl/bin/vector


# chrony
RUN set -e \
       && apk add --no-cache --no-progress --quiet \
               chrony \
       && /helper.move-bins.sh chronyd chronyc

# ssh server
RUN set -e \
	&& apk add --no-cache --no-progress --quiet \
		openssh-server \
	&& /helper.move-bins.sh sshd ssh-keygen

# quota tools
RUN set -e \
	&& apk add --no-cache --no-progress --quiet \
		quota-tools \
	&& /helper.move-bins.sh quota edquota quotacheck quotaoff quotaon quotastats setquota repquota tune2fs

COPY --from=neonvm-daemon-loader /neonvmd /neonvm/bin/neonvmd

# init scripts & configs
COPY inittab     /neonvm/bin/inittab
COPY vminit      /neonvm/bin/vminit
COPY vmstart     /neonvm/bin/vmstart
COPY vmshutdown  /neonvm/bin/vmshutdown
COPY vmacpi      /neonvm/acpi/vmacpi
COPY vector.yaml /neonvm/config/vector.yaml
COPY chrony.conf /neonvm/config/chrony.conf
COPY sshd_config /neonvm/config/sshd_config
RUN chmod +rx /neonvm/bin/vminit /neonvm/bin/vmstart /neonvm/bin/vmshutdown
COPY udev-init.sh /neonvm/bin/udev-init.sh
RUN chmod +rx /neonvm/bin/udev-init.sh
COPY resize-swap.sh /neonvm/bin/resize-swap
RUN chmod +rx /neonvm/bin/resize-swap
COPY set-disk-quota.sh /neonvm/bin/set-disk-quota
RUN chmod +rx /neonvm/bin/set-disk-quota

# rootdisk modification
FROM rootdisk AS rootdisk-mod
COPY --from=vm-runtime /neonvm /neonvm
# setup chrony
RUN set -e \
    && /neonvm/bin/id -g chrony > /dev/null 2>&1 || /neonvm/bin/addgroup chrony \
    && /neonvm/bin/id -u chrony > /dev/null 2>&1 || /neonvm/bin/adduser -D -H -G chrony -g 'chrony' -s /neonvm/bin/nologin chrony \
    && /neonvm/bin/mkdir -p /var/lib/chrony \
    && /neonvm/bin/chown chrony:chrony /var/lib/chrony \
    && /neonvm/bin/mkdir -p /var/log/chrony
# setup sshd user and group to support sshd UsePrivilegeSeparation
RUN set -e \
    && /neonvm/bin/id -g sshd > /dev/null 2>&1 || /neonvm/bin/addgroup sshd \
    && /neonvm/bin/id -u sshd > /dev/null 2>&1 || /neonvm/bin/adduser -D -H -G sshd -g 'sshd privsep' -s /neonvm/bin/nologin sshd

FROM alpine:3.19 AS builder
ARG DISK_SIZE
COPY --from=rootdisk-mod / /rootdisk

# tools for qemu disk creation
RUN set -e \
	&& apk add --no-cache --no-progress --quiet \
		qemu-img \
		e2fsprogs

RUN set -e \
    && mkdir -p /rootdisk/etc \
    && mkdir -p /rootdisk/etc/vector \
    && mkdir -p /rootdisk/etc/ssh \
    && mkdir -p /rootdisk/var/empty \
    && cp -f /rootdisk/neonvm/bin/inittab /rootdisk/etc/inittab \
    && mkfs.ext4 -L vmroot -d /rootdisk /disk.raw ${DISK_SIZE} \
    && qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on /disk.raw /disk.qcow2

FROM alpine:3.19
COPY --from=builder /disk.qcow2 /
