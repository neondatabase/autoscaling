FROM vm-monitor:dev as monitor
# ^ don't do anything with these; we just want it around for later use.

FROM postgres:15-bullseye

RUN set -e \
    && rm -f /etc/inittab \
    && touch /etc/inittab

RUN set -e \
	&& apk add --no-cache \
		ca-certificates \
		util-linux-misc \
        coreutils \
		cgroup-tools

# Configure cgroups on startup
ADD cgconfig.conf /etc/cgconfig.conf
RUN echo '::sysinit:/usr/sbin/cgconfigparser -l /etc/cgconfig.conf -s 1664' >> /etc/inittab

# Add the vm-monitor
COPY --from=monitor /usr/bin/vm-monitor /bin/vm-monitor 
RUN adduser vm-monitor --disabled-password --no-create-home
RUN echo "::respawn:su vm-monitor -c 'RUST_LOG=info /bin/vm-monitor --cgroup=neon-test'" >> /etc/inittab
