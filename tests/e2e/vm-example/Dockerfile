FROM vm-informant:dev as informant
# ^ don't do anything with this; we just want it around for later use.

FROM postgres:15-alpine

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

# Add the vm-informant
COPY --from=informant /usr/bin/vm-informant /bin/vm-informant
RUN adduser vm-informant --disabled-password --no-create-home
# note: Use 'respawn' and '--auto-restart' so that the logs are noisy if the arguments are bad,
# but we still have proper handling around cgroups, etc.
RUN echo "::respawn:su vm-informant -c '/bin/vm-informant --auto-restart --cgroup=neon-test'" >> /etc/inittab
