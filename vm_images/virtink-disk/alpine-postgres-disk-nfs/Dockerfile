FROM alpine:3.16

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

# postgresql stuff
RUN set -e \
	&& apk add --no-cache \
		nfs-utils \
		su-exec \
		postgresql14
ADD postgresql.conf /etc/postgresql.conf
