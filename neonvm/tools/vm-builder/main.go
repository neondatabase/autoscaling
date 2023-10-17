package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"text/template"

	"github.com/alessio/shellescape"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// vm-builder --src alpine:3.16 --dst vm-alpine:dev --file vm-alpine.qcow2

const (
	dockerfileVmBuilder = `
FROM {{.MonitorImage}} as monitor

# Build cgroup-tools
#
# At time of writing (2023-03-14), debian bullseye has a version of cgroup-tools (technically
# libcgroup) that doesn't support cgroup v2 (version 0.41-11). Unfortunately, the vm-monitor
# requires cgroup v2, so we'll build cgroup-tools ourselves.
FROM debian:bullseye-slim as libcgroup-builder
ENV LIBCGROUP_VERSION v2.0.3

RUN set -exu \
	&& apt update \
	&& apt install --no-install-recommends -y \
		git \
		ca-certificates \
		automake \
		cmake \
		make \
		gcc \
		byacc \
		flex \
		libtool \
		libpam0g-dev \
	&& git clone --depth 1 -b $LIBCGROUP_VERSION https://github.com/libcgroup/libcgroup \
	&& INSTALL_DIR="/libcgroup-install" \
	&& mkdir -p "$INSTALL_DIR/bin" "$INSTALL_DIR/include" \
	&& cd libcgroup \
	# extracted from bootstrap.sh, with modified flags:
	&& (test -d m4 || mkdir m4) \
	&& autoreconf -fi \
	&& rm -rf autom4te.cache \
	&& CFLAGS="-O3" ./configure --prefix="$INSTALL_DIR" --sysconfdir=/etc --localstatedir=/var --enable-opaque-hierarchy="name=systemd" \
	# actually build the thing...
	&& make install
RUN ls $INSTALL_DIR/bin

FROM quay.io/prometheuscommunity/postgres-exporter:v0.12.0 AS postgres-exporter

# Build pgbouncer
#
FROM debian:bullseye-slim AS pgbouncer
RUN set -e \
	&& apt-get update \
	&& apt-get install -y \
		curl \
		build-essential \
		pkg-config \
		libevent-dev \
		libssl-dev

ENV PGBOUNCER_VERSION 1.21.0
ENV PGBOUNCER_GITPATH 1_21_0
RUN set -e \
	&& curl -sfSL https://github.com/pgbouncer/pgbouncer/releases/download/pgbouncer_${PGBOUNCER_GITPATH}/pgbouncer-${PGBOUNCER_VERSION}.tar.gz -o pgbouncer-${PGBOUNCER_VERSION}.tar.gz \
	&& tar xzvf pgbouncer-${PGBOUNCER_VERSION}.tar.gz \
	&& cd pgbouncer-${PGBOUNCER_VERSION} \
	&& LDFLAGS=-static ./configure --prefix=/usr/local/pgbouncer --without-openssl \
	&& make -j $(nproc) \
	&& make install

FROM {{.RootDiskImage}} AS rootdisk

USER root
RUN adduser --system --disabled-login --no-create-home --home /nonexistent --gecos "monitor user" --shell /bin/false vm-monitor

# tweak nofile limits
RUN set -e \
	&& echo 'fs.file-max = 1048576' >>/etc/sysctl.conf \
	&& test ! -e /etc/security || ( \
	   echo '*    - nofile 1048576' >>/etc/security/limits.conf \
	&& echo 'root - nofile 1048576' >>/etc/security/limits.conf \
	   )

COPY cgconfig.conf /etc/cgconfig.conf
COPY pgbouncer.ini /etc/pgbouncer.ini
RUN set -e \
	&& chown postgres:postgres /etc/pgbouncer.ini \
	&& chmod 0644 /etc/pgbouncer.ini \
	&& chmod 0644 /etc/cgconfig.conf

USER postgres

COPY --from=monitor           /usr/bin/vm-monitor /usr/local/bin/vm-monitor
COPY --from=libcgroup-builder /libcgroup-install/bin/*  /usr/bin/
COPY --from=libcgroup-builder /libcgroup-install/lib/*  /usr/lib/
COPY --from=libcgroup-builder /libcgroup-install/sbin/* /usr/sbin/
COPY --from=postgres-exporter /bin/postgres_exporter /bin/postgres_exporter
COPY --from=pgbouncer         /usr/local/pgbouncer/bin/pgbouncer /usr/local/bin/pgbouncer

FROM alpine:3.16 AS vm-runtime
# add busybox
ENV BUSYBOX_VERSION 1.35.0
RUN set -e \
	&& mkdir -p /neonvm/bin /neonvm/runtime /neonvm/config \
	&& wget -q https://busybox.net/downloads/binaries/${BUSYBOX_VERSION}-x86_64-linux-musl/busybox -O /neonvm/bin/busybox \
	&& chmod +x /neonvm/bin/busybox \
	&& /neonvm/bin/busybox --install -s /neonvm/bin

# add udevd and agetty (with shared libs)
RUN set -e \
	&& apk add --no-cache --no-progress --quiet \
		acpid \
		udev \
		agetty \
		su-exec \
		e2fsprogs-extra \
		blkid \
		flock \
	&& mv /sbin/acpid         /neonvm/bin/ \
	&& mv /sbin/udevd         /neonvm/bin/ \
	&& mv /sbin/agetty        /neonvm/bin/ \
	&& mv /sbin/su-exec       /neonvm/bin/ \
	&& mv /usr/sbin/resize2fs /neonvm/bin/resize2fs \
	&& mv /sbin/blkid         /neonvm/bin/blkid \
	&& mv /usr/bin/flock	  /neonvm/bin/flock \
	&& mkdir -p /neonvm/lib \
	&& cp -f /lib/ld-musl-x86_64.so.1  /neonvm/lib/ \
	&& cp -f /lib/libblkid.so.1.1.0    /neonvm/lib/libblkid.so.1 \
	&& cp -f /lib/libcrypto.so.1.1     /neonvm/lib/ \
	&& cp -f /lib/libkmod.so.2.3.7     /neonvm/lib/libkmod.so.2 \
	&& cp -f /lib/libudev.so.1.6.3     /neonvm/lib/libudev.so.1 \
	&& cp -f /lib/libz.so.1.2.12       /neonvm/lib/libz.so.1 \
	&& cp -f /usr/lib/liblzma.so.5.2.5 /neonvm/lib/liblzma.so.5 \
	&& cp -f /usr/lib/libzstd.so.1.5.2 /neonvm/lib/libzstd.so.1 \
	&& cp -f /lib/libe2p.so.2          /neonvm/lib/libe2p.so.2 \
	&& cp -f /lib/libext2fs.so.2       /neonvm/lib/libext2fs.so.2 \
	&& cp -f /lib/libcom_err.so.2      /neonvm/lib/libcom_err.so.2 \
	&& cp -f /lib/libblkid.so.1        /neonvm/lib/libblkid.so.1 \
	&& mv /usr/share/udhcpc/default.script /neonvm/bin/udhcpc.script \
	&& sed -i 's/#!\/bin\/sh/#!\/neonvm\/bin\/sh/' /neonvm/bin/udhcpc.script \
	&& sed -i 's/export PATH=.*/export PATH=\/neonvm\/bin/' /neonvm/bin/udhcpc.script

# tools for qemu disk creation
RUN set -e \
	&& apk add --no-cache --no-progress --quiet \
		qemu-img \
		e2fsprogs

# Install vector.dev binary
RUN set -e \
    && wget https://packages.timber.io/vector/0.26.0/vector-0.26.0-x86_64-unknown-linux-musl.tar.gz -O - \
    | tar xzvf - --strip-components 3 -C /neonvm/bin/ ./vector-x86_64-unknown-linux-musl/bin/vector

# init scripts
ADD inittab   /neonvm/bin/inittab
ADD vminit    /neonvm/bin/vminit
ADD vmstart   /neonvm/bin/vmstart
ADD vmshutdown /neonvm/bin/vmshutdown
ADD vmacpi    /neonvm/acpi/vmacpi
ADD vector.yaml /neonvm/config/vector.yaml
RUN chmod +rx /neonvm/bin/vminit /neonvm/bin/vmstart /neonvm/bin/vmshutdown

FROM vm-runtime AS builder
ARG DISK_SIZE
COPY --from=rootdisk / /rootdisk
COPY --from=vm-runtime /neonvm /rootdisk/neonvm
RUN set -e \
    && mkdir -p /rootdisk/etc \
    && mkdir -p /rootdisk/etc/vector \
    && cp -f /rootdisk/neonvm/bin/inittab /rootdisk/etc/inittab \
    && mkfs.ext4 -L vmroot -d /rootdisk /disk.raw ${DISK_SIZE} \
    && qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on /disk.raw /disk.qcow2

FROM alpine:3.16
RUN apk add --no-cache --no-progress --quiet qemu-img
COPY --from=builder /disk.qcow2 /
`
	scriptVmStart = `#!/neonvm/bin/sh

/neonvm/bin/cat <<'EOF' >/neonvm/bin/vmstarter.sh
{{ range .Env }}
export {{.}}
{{- end }}
EOF

if /neonvm/bin/test -f /neonvm/runtime/env.sh; then
    /neonvm/bin/cat /neonvm/runtime/env.sh >>/neonvm/bin/vmstarter.sh
fi

{{if or .Entrypoint .Cmd | not}}
# If we have no arguments *at all*, then emit an error. This matches docker's behavior.
if /neonvm/bin/test \( ! -f /neonvm/runtime/command.sh \) -a \( ! -f /neonvm/runtime/args.sh \); then
	/neonvm/bin/echo 'Error: No command specified' >&2
	exit 1
fi
{{end}}

{{/* command.sh is set by the runner with the contents of the VM's spec.guest.command, if it's set */}}
if /neonvm/bin/test -f /neonvm/runtime/command.sh; then
    /neonvm/bin/cat /neonvm/runtime/command.sh >>/neonvm/bin/vmstarter.sh
else
    {{/*
	A couple notes:
	  - .Entrypoint is already shell-escaped twice (everything is quoted)
	  - the shell-escaping isn't perfect. In particular, it doesn't handle backslashes well.
	  - It's good enough for now
	*/}}
    /neonvm/bin/echo -n {{range .Entrypoint}}' '{{.}}{{end}} >> /neonvm/bin/vmstarter.sh
fi

{{/* args.sh is set by the runner with the contents of the VM's spec.guest.args, if it's set */}}
if /neonvm/bin/test -f /neonvm/runtime/args.sh; then
    /neonvm/bin/echo -n ' ' >>/neonvm/bin/vmstarter.sh
    /neonvm/bin/cat /neonvm/runtime/args.sh >>/neonvm/bin/vmstarter.sh
else
    {{/* Same as with .Entrypoint; refer there. We don't have '-n' because we want a trailing newline */}}
    /neonvm/bin/echo -n {{range .Cmd}}' '{{.}}{{end}} >> /neonvm/bin/vmstarter.sh
fi

/neonvm/bin/chmod +x /neonvm/bin/vmstarter.sh

/neonvm/bin/flock -o /neonvm/vmstart.lock -c 'test -e /neonvm/vmstart.allowed && /neonvm/bin/su-exec {{.User}} /neonvm/bin/sh /neonvm/bin/vmstarter.sh'
`

	scriptInitTab = `
::sysinit:/neonvm/bin/vminit
::sysinit:cgconfigparser -l /etc/cgconfig.conf -s 1664
::once:/neonvm/bin/touch /neonvm/vmstart.allowed
::respawn:/neonvm/bin/udhcpc -t 1 -T 1 -A 1 -f -i eth0 -O 121 -O 119 -s /neonvm/bin/udhcpc.script
::respawn:/neonvm/bin/udevd
::respawn:/neonvm/bin/acpid -f -c /neonvm/acpi
::respawn:/neonvm/bin/vector -c /neonvm/config/vector.yaml --config-dir /etc/vector
::respawn:/neonvm/bin/vmstart
{{if .EnableMonitor}}
::respawn:su -p vm-monitor -c 'RUST_LOG=info /usr/local/bin/vm-monitor --addr "0.0.0.0:10301" --cgroup=neon-postgres{{if .FileCache}} --pgconnstr="host=localhost port=5432 dbname=postgres user=cloud_admin sslmode=disable"{{end}}'
{{end}}
::respawn:su -p nobody -c '/usr/local/bin/pgbouncer /etc/pgbouncer.ini'
::respawn:su -p nobody -c 'DATA_SOURCE_NAME="user=cloud_admin sslmode=disable dbname=postgres" /bin/postgres_exporter --auto-discover-databases --exclude-databases=template0,template1'
ttyS0::respawn:/neonvm/bin/agetty --8bits --local-line --noissue --noclear --noreset --host console --login-program /neonvm/bin/login --login-pause --autologin root 115200 ttyS0 linux
::shutdown:/neonvm/bin/vmshutdown
`

	scriptVmAcpi = `
event=button/power
action=/neonvm/bin/poweroff
`

	scriptVmShutdown = `#!/neonvm/bin/sh
rm /neonvm/vmstart.allowed
if [ -e /neonvm/vmstart.allowed ]; then
	echo "Error: could not remove vmstart.allowed marker, might hang indefinitely during shutdown" 1>&2
fi
# we inhibited new command starts, but there may still be a command running
while ! /neonvm/bin/flock -n /neonvm/vmstart.lock true; do
	su -p postgres --session-command '/usr/local/bin/pg_ctl stop -D /var/db/postgres/compute/pgdata -m fast --wait -t 10'
done
echo "vmstart workload shut down cleanly" 1>&2
`

	scriptVmInit = `#!/neonvm/bin/sh

export PATH=/neonvm/bin

# set links to libs
mkdir -p /lib
for l in $(find /neonvm/lib -type f); do
    lib="$(basename $l)"
    [ ! -e "/lib/${lib}" -a ! -e "/usr/lib/${lib}" ] && ln -s "${l}" "/lib/${lib}"
done

# udev rule for auto-online hotplugged CPUs
mkdir -p /lib/udev/rules.d
echo 'SUBSYSTEM=="cpu", ACTION=="add", TEST=="online", ATTR{online}=="0", ATTR{online}="1"' > /lib/udev/rules.d/99-hotplug-cpu.rules

# system mounts
mkdir -p /dev/pts /dev/shm
chmod 0755 /dev/pts
chmod 1777 /dev/shm
mount -t proc  proc  /proc
mount -t sysfs sysfs /sys
mount -t cgroup2 cgroup2 /sys/fs/cgroup

# Allow all users to move processes to/from the root cgroup.
#
# This is required in order to be able to 'cgexec' anything, if the entrypoint is not being run as
# root, because moving tasks between one cgroup and another *requires write access to the
# cgroup.procs file of the common ancestor*, and because the entrypoint isn't already in a cgroup,
# any new tasks are automatically placed in the top-level cgroup.
#
# This *would* be bad for security, if we relied on cgroups for security; but instead because they
# are just used for cooperative signaling, this should be mostly ok.
chmod go+w /sys/fs/cgroup/cgroup.procs

mount -t devpts -o noexec,nosuid       devpts    /dev/pts
mount -t tmpfs  -o noexec,nosuid,nodev shm-tmpfs /dev/shm

# neonvm runtime params mounted as iso9660 disk
mount -o ro,mode=0644 /dev/vdb /neonvm/runtime

# mount virtual machine .spec.disks
test -f /neonvm/runtime/mounts.sh && /neonvm/bin/sh /neonvm/runtime/mounts.sh

# try resize filesystem
resize2fs /dev/vda

# networking
ip link set up dev lo
ip link set up dev eth0
`
	configVector = `---
data_dir: /tmp
api:
  enabled: true
  address: "0.0.0.0:8686"
  playground: false
sources:
  host_metrics:
    filesystem:
      devices:
        excludes: [binfmt_misc]
      filesystems:
        excludes: [binfmt_misc]
      mountPoints:
        excludes: ["*/proc/sys/fs/binfmt_misc"]
    type: host_metrics
sinks:
  prom_exporter:
    type: prometheus_exporter
    inputs:
      - host_metrics
    address: "0.0.0.0:9100"
`
	// cgconfig.conf
	configCgroup = `# Configuration for cgroups in VM compute nodes
group neon-postgres {
    perm {
        admin {
            uid = {{.CgroupUID}};
        }
        task {
            gid = users;
        }
    }
    memory {}
}
`

	// pgbouncer.ini
	configPgbouncer = `
[databases]
*=host=localhost port=5432 auth_user=cloud_admin
[pgbouncer]
listen_port=6432
listen_addr=0.0.0.0
auth_type=scram-sha-256
auth_user=cloud_admin
client_tls_sslmode=disable
server_tls_sslmode=disable
pool_mode=transaction
max_client_conn=10000
default_pool_size=16
max_prepared_statements=100
`
)

var (
	Version   string
	VMMonitor string

	srcImage      = flag.String("src", "", `Docker image used as source for virtual machine disk image: --src=alpine:3.16`)
	dstImage      = flag.String("dst", "", `Docker image with resulting disk image: --dst=vm-alpine:3.16`)
	size          = flag.String("size", "1G", `Size for disk image: --size=1G`)
	outFile       = flag.String("file", "", `Save disk image as file: --file=vm-alpine.qcow2`)
	quiet         = flag.Bool("quiet", false, `Show less output from the docker build process`)
	forcePull     = flag.Bool("pull", false, `Pull src image even if already present locally`)
	monitor       = flag.String("monitor", VMMonitor, `vm-monitor docker image`)
	enableMonitor = flag.Bool("enable-monitor", false, `start the vm-monitor during VM startup`)
	fileCache     = flag.Bool("enable-file-cache", false, `enables the vm-monitor's file cache integration`)
	cgroupUID     = flag.String("cgroup-uid", "vm-monitor", `specifies the user that owns the neon-postgres cgroup`)
	version       = flag.Bool("version", false, `Print vm-builder version`)
)

type dockerMessage struct {
	Stream string `json:"stream"`
}

func printReader(reader io.ReadCloser) error {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		candidateJSON := scanner.Bytes()
		var msg dockerMessage
		if err := json.Unmarshal(candidateJSON, &msg); err != nil || msg.Stream == "" {
			log.Println(string(candidateJSON))
			continue
		}

		log.Print(msg.Stream)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func AddTemplatedFileToTar(tw *tar.Writer, tmplArgs any, filename string, tmplString string) error {
	tmpl, err := template.New(filename).Parse(tmplString)
	if err != nil {
		return fmt.Errorf("failed to parse template for %q: %w", filename, err)
	}

	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, tmplArgs); err != nil {
		return fmt.Errorf("failed to execute template for %q: %w", filename, err)
	}

	tarHeader := &tar.Header{
		Name: filename,
		Size: int64(buf.Len()),
	}
	if err = tw.WriteHeader(tarHeader); err != nil {
		return fmt.Errorf("failed to write tar header for %q: %w", filename, err)
	}
	if _, err = tw.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("failed to write file content for %q: %w", filename, err)
	}

	return nil
}

type TemplatesContext struct {
	User          string
	Entrypoint    []string
	Cmd           []string
	Env           []string
	RootDiskImage string
	MonitorImage  string
	FileCache     bool
	EnableMonitor bool
	CgroupUID     string
}

func main() {
	flag.Parse()
	var dstIm string

	if *version {
		fmt.Println(Version)
		os.Exit(0)
	}

	if len(*srcImage) == 0 {
		log.Println("-src not set, see usage info:")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if len(*dstImage) == 0 {
		dstIm = fmt.Sprintf("vm-%s", *srcImage)
		log.Printf("-dst not set, using %s\n", dstIm)
	} else {
		dstIm = *dstImage
	}

	ctx := context.Background()

	log.Println("Setup docker connection")
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalln(err)
	}
	defer cli.Close()

	hostContainsSrcImage := false
	if !*forcePull {
		hostImages, err := cli.ImageList(ctx, types.ImageListOptions{})
		if err != nil {
			log.Fatalln(err)
		}

		for _, img := range hostImages {
			for _, name := range img.RepoTags {
				if name == *srcImage {
					hostContainsSrcImage = true
					break
				}
			}
			if hostContainsSrcImage {
				break
			}
		}
	}

	if !hostContainsSrcImage {
		// pull source image
		log.Printf("Pull source docker image: %s", *srcImage)
		pull, err := cli.ImagePull(ctx, *srcImage, types.ImagePullOptions{})
		if err != nil {
			log.Fatalln(err)
		}
		defer pull.Close()

		// do quiet pull - discard output
		io.Copy(io.Discard, pull)
	}

	log.Printf("Build docker image for virtual machine (disk size %s): %s\n", *size, dstIm)
	imageSpec, _, err := cli.ImageInspectWithRaw(ctx, *srcImage)
	if err != nil {
		log.Fatalln(err)
	}

	// Shell-escape all the command pieces, twice. We need to do it twice because we're generating
	// a shell script that appends these to a second shell script.
	for i := range imageSpec.Config.Entrypoint {
		imageSpec.Config.Entrypoint[i] = shellescape.Quote(shellescape.Quote(imageSpec.Config.Entrypoint[i]))
	}
	for i := range imageSpec.Config.Cmd {
		imageSpec.Config.Cmd[i] = shellescape.Quote(shellescape.Quote(imageSpec.Config.Cmd[i]))
	}

	tmplArgs := TemplatesContext{
		Entrypoint:    imageSpec.Config.Entrypoint,
		Cmd:           imageSpec.Config.Cmd,
		Env:           imageSpec.Config.Env,
		RootDiskImage: *srcImage,
		MonitorImage:  *monitor,
		FileCache:     *fileCache,
		EnableMonitor: *enableMonitor,
		CgroupUID:     *cgroupUID,
	}

	if len(imageSpec.Config.User) != 0 {
		tmplArgs.User = imageSpec.Config.User
	} else {
		tmplArgs.User = "root"
	}

	tarBuffer := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuffer)
	defer tw.Close()

	files := []struct {
		filename string
		tmpl     string
	}{
		{"Dockerfile", dockerfileVmBuilder},
		{"vmstart", scriptVmStart},
		{"vmshutdown", scriptVmShutdown},
		{"inittab", scriptInitTab},
		{"vmacpi", scriptVmAcpi},
		{"vminit", scriptVmInit},
		{"cgconfig.conf", configCgroup},
		{"vector.yaml", configVector},
		{"pgbouncer.ini", configPgbouncer},
	}

	for _, f := range files {
		if err := AddTemplatedFileToTar(tw, tmplArgs, f.filename, f.tmpl); err != nil {
			log.Fatalln(err)
		}
	}

	buildArgs := make(map[string]*string)
	buildArgs["DISK_SIZE"] = size
	opt := types.ImageBuildOptions{
		Tags: []string{
			dstIm,
		},
		BuildArgs:      buildArgs,
		SuppressOutput: *quiet,
		NoCache:        false,
		Context:        tarBuffer,
		Dockerfile:     "Dockerfile",
		Remove:         true,
		ForceRemove:    true,
	}
	buildResp, err := cli.ImageBuild(ctx, tarBuffer, opt)
	if err != nil {
		log.Fatalln(err)
	}

	// do quiet build - discard output
	//io.Copy(io.Discard, buildResp.Body)

	if err = printReader(buildResp.Body); err != nil {
		log.Fatalln(err)
	}

	if len(*outFile) != 0 {
		log.Printf("Save disk image as %s", *outFile)
		// create container from docker image we just built
		containerResp, err := cli.ContainerCreate(ctx, &container.Config{
			Image:      dstIm,
			Tty:        false,
			Entrypoint: imageSpec.Config.Entrypoint,
			Cmd:        imageSpec.Config.Cmd,
		}, nil, nil, nil, "")
		if err != nil {
			log.Fatalln(err)
		}
		if len(containerResp.Warnings) > 0 {
			log.Println(containerResp.Warnings)
		}

		// copy file from container as tar archive
		fromContainer, _, err := cli.CopyFromContainer(ctx, containerResp.ID, "/disk.qcow2")
		if err != nil {
			log.Fatalln(err)
		}

		// untar file from tar archive
		tarReader := tar.NewReader(fromContainer)
		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break
			} else if err != nil {
				log.Fatalln(err)
			}

			if header.Name != "disk.qcow2" {
				log.Printf("skip file %s", header.Name)
				continue
			}
			path := filepath.Join(*outFile)
			info := header.FileInfo()

			file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
			if err != nil {
				log.Fatalln(err)
			}
			defer file.Close()
			_, err = io.Copy(file, tarReader)
			if err != nil {
				log.Fatalln(err)
			}
		}
		// remove container
		if err = cli.ContainerRemove(ctx, containerResp.ID, types.ContainerRemoveOptions{}); err != nil {
			log.Println(err)
		}

	}

}
