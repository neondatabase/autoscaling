package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"text/template"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// vm-builder --src alpine:3.16 --dst vm-alpine:dev --file vm-alpine.qcow2

const (
	dockerfileVmBuilder = `
FROM {{.InformantImage}} as informant

# Build cgroup-tools
#
# At time of writing (2023-03-14), debian bullseye has a version of cgroup-tools (technically
# libcgroup) that doesn't support cgroup v2 (version 0.41-11). Unfortunately, the vm-informant
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

ENV PGBOUNCER_VERSION 1.18.0
ENV PGBOUNCER_GITPATH 1_18_0
RUN set -e \
	&& curl -sfSL https://github.com/pgbouncer/pgbouncer/releases/download/pgbouncer_${PGBOUNCER_GITPATH}/pgbouncer-${PGBOUNCER_VERSION}.tar.gz -o pgbouncer-${PGBOUNCER_VERSION}.tar.gz \
	&& tar xzvf pgbouncer-${PGBOUNCER_VERSION}.tar.gz \
	&& cd pgbouncer-${PGBOUNCER_VERSION} \
	&& ./configure --prefix=/usr/local/pgbouncer \
	&& make -j $(nproc) \
	&& make install

FROM {{.RootDiskImage}} AS rootdisk

USER root
RUN adduser --system --disabled-login --no-create-home --home /nonexistent --gecos "informant user" --shell /bin/false vm-informant

# tweak nofile limits
RUN set -e \
	&& echo 'fs.file-max = 1048576' >>/etc/sysctl.conf \
	&& echo '*    - nofile 1048576' >>/etc/security/limits.conf \
	&& echo 'root - nofile 1048576' >>/etc/security/limits.conf

COPY cgconfig.conf /etc/cgconfig.conf
COPY pgbouncer.ini /etc/pgbouncer.ini
RUN set -e \
	&& chown postgres:postgres /etc/pgbouncer.ini \
	&& chmod 0644 /etc/pgbouncer.ini \
	&& chmod 0644 /etc/cgconfig.conf

# deps for pgbouncer
RUN set -e \
	&& apt-get update \
	&& apt-get install -y --no-install-recommends libevent-2.1-7 \
	&& rm -rf /var/lib/apt/lists/*

USER postgres

COPY --from=informant         /usr/bin/vm-informant /usr/local/bin/vm-informant
COPY --from=libcgroup-builder /libcgroup-install/bin/*  /usr/bin/
COPY --from=libcgroup-builder /libcgroup-install/lib/*  /usr/lib/
COPY --from=libcgroup-builder /libcgroup-install/sbin/* /usr/sbin/
COPY --from=postgres-exporter /bin/postgres_exporter /bin/postgres_exporter
COPY --from=pgbouncer         /usr/local/pgbouncer/bin/pgbouncer /usr/local/bin/pgbouncer

ENTRYPOINT ["/usr/sbin/cgexec", "-g", "*:neon-postgres", "/usr/local/bin/compute_ctl"]

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
	&& mv /sbin/acpid         /neonvm/bin/ \
	&& mv /sbin/udevd         /neonvm/bin/ \
	&& mv /sbin/agetty        /neonvm/bin/ \
	&& mv /sbin/su-exec       /neonvm/bin/ \
	&& mv /usr/sbin/resize2fs /neonvm/bin/resize2fs \
	&& mv /sbin/blkid         /neonvm/bin/blkid \
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
ADD vmacpi    /neonvm/acpi/vmacpi
ADD vector.yaml /neonvm/config/vector.yaml
ADD powerdown /neonvm/bin/powerdown
RUN chmod +rx /neonvm/bin/vminit /neonvm/bin/vmstart /neonvm/bin/powerdown

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

if /neonvm/bin/test -f /neonvm/runtime/command.sh; then
    /neonvm/bin/cat /neonvm/runtime/command.sh >>/neonvm/bin/vmstarter.sh
else
    /neonvm/bin/echo -n '{{- range .Entrypoint}}{{.}} {{- end }}' >>/neonvm/bin/vmstarter.sh
fi

echo -n ' ' >>/neonvm/bin/vmstarter.sh

if /neonvm/bin/test -f /neonvm/runtime/args.sh; then
    /neonvm/bin/cat /neonvm/runtime/args.sh >>/neonvm/bin/vmstarter.sh
else
    /neonvm/bin/echo '{{- range .Cmd }}{{.}} {{- end }}' >>/neonvm/bin/vmstarter.sh
fi

/neonvm/bin/chmod +x /neonvm/bin/vmstarter.sh

/neonvm/bin/su-exec {{.User}} /neonvm/bin/sh /neonvm/bin/vmstarter.sh
`

	scriptInitTab = `
::sysinit:/neonvm/bin/vminit
::sysinit:cgconfigparser -l /etc/cgconfig.conf -s 1664
::respawn:/neonvm/bin/udevd
::respawn:/neonvm/bin/acpid -f -c /neonvm/acpi
::respawn:/neonvm/bin/vector -c /neonvm/config/vector.yaml --config-dir /etc/vector
::respawn:/neonvm/bin/vmstart
::respawn:su -p vm-informant --session-command '/usr/local/bin/vm-informant --auto-restart --cgroup=neon-postgres --pgconnstr="dbname=neondb user=cloud_admin sslmode=disable"'
::respawn:su -p nobody --session-command '/usr/local/bin/pgbouncer /etc/pgbouncer.ini'
::respawn:su -p nobody --session-command 'DATA_SOURCE_NAME="user=cloud_admin sslmode=disable dbname=postgres" /bin/postgres_exporter --auto-discover-databases --exclude-databases=template0,template1'
ttyS0::respawn:/neonvm/bin/agetty --8bits --local-line --noissue --noclear --noreset --host console --login-program /neonvm/bin/login --login-pause --autologin root 115200 ttyS0 linux
`

	scriptVmAcpi = `
event=button/power
action=/neonvm/bin/powerdown
`

	scriptPowerDown = `#!/neonvm/bin/sh

su -p postgres --session-command '/usr/local/bin/pg_ctl stop -D /var/db/postgres/compute/pgdata -m fast --wait -t 10'
/neonvm/bin/poweroff
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
ETH_LIST=$(find /sys/class/net -mindepth 1 -maxdepth 1 -name "eth*")
for i in ${ETH_LIST}; do
    iface=$(basename $i)
    ip link set up dev $iface
    udhcpc -t 1 -T 1 -A 1 -b -q -i $iface -O 121 -O 119 -s /neonvm/bin/udhcpc.script
done
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
            uid = vm-informant;
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
`
)

var (
	Version     string
	VMInformant string

	srcImage  = flag.String("src", "", `Docker image used as source for virtual machine disk image: --src=alpine:3.16`)
	dstImage  = flag.String("dst", "", `Docker image with resulting disk image: --dst=vm-alpine:3.16`)
	size      = flag.String("size", "1G", `Size for disk image: --size=1G`)
	outFile   = flag.String("file", "", `Save disk image as file: --file=vm-alpine.qcow2`)
	forcePull = flag.Bool("pull", false, `Pull src image even if already present locally`)
	informant = flag.String("informant", VMInformant, `vm-informant docker image`)
	version   = flag.Bool("version", false, `Print vm-builder version`)
)

func printReader(reader io.ReadCloser) error {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		log.Println(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func AddToTar(tw *tar.Writer, filename string, buf bytes.Buffer) error {
	tarHeader := &tar.Header{
		Name: filename,
		Size: int64(len(buf.String())),
	}
	err := tw.WriteHeader(tarHeader)
	if err != nil {
		return err
	}
	_, err = tw.Write(buf.Bytes())
	if err != nil {
		return err
	}
	return nil
}

type ImageSpec struct {
	User       string
	Entrypoint []string
	Cmd        []string
	Env        []string
}

type Images struct {
	RootDiskImage  string
	InformantImage string
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

	var SrcImageSpec ImageSpec

	if len(imageSpec.Config.User) != 0 {
		SrcImageSpec.User = imageSpec.Config.User
	} else {
		SrcImageSpec.User = "root"
	}

	SrcImageSpec.Entrypoint = imageSpec.Config.Entrypoint
	SrcImageSpec.Cmd = imageSpec.Config.Cmd
	// if no entrypoint and cmd in docker image then use sleep for 10 years as stub
	if len(SrcImageSpec.Entrypoint) == 0 && len(SrcImageSpec.Cmd) == 0 {
		SrcImageSpec.Cmd = []string{"/neonvm/bin/sleep", "3650d"}
	}

	SrcImageSpec.Env = imageSpec.Config.Env

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	defer tw.Close()

	// generate Dockerfile from template
	dockerfileVmBuilderTmpl, err := template.New("vm-builder").Parse(dockerfileVmBuilder)
	if err != nil {
		log.Fatal(err)
	}
	var dockerfileVmBuilderBuffer bytes.Buffer
	err = dockerfileVmBuilderTmpl.Execute(&dockerfileVmBuilderBuffer, &Images{RootDiskImage: *srcImage, InformantImage: *informant})
	if err != nil {
		log.Fatalln(err)
	}

	// generate vmstart script from template
	scriptVmStartTmpl, err := template.New("vmstart").Parse(scriptVmStart)
	if err != nil {
		log.Fatal(err)
	}
	var scriptVmStartBuffer bytes.Buffer
	err = scriptVmStartTmpl.Execute(&scriptVmStartBuffer, SrcImageSpec)
	if err != nil {
		log.Fatalln(err)
	}

	// add 'Dockerfile' file to docker build context
	if err = AddToTar(tw, "Dockerfile", dockerfileVmBuilderBuffer); err != nil {
		log.Fatalln(err)
	}
	// add 'vmstart' file to docker build context
	if err = AddToTar(tw, "vmstart", scriptVmStartBuffer); err != nil {
		log.Fatalln(err)
	}

	var b bytes.Buffer

	// add 'inittab' file to docker build context
	_, err = b.WriteString(scriptInitTab)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "inittab", b); err != nil {
		log.Fatalln(err)
	}

	// add 'vmacpi' file to docker build context
	b.Reset()
	_, err = b.WriteString(scriptVmAcpi)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "vmacpi", b); err != nil {
		log.Fatalln(err)
	}

	// add 'powerdown' file to docker build context
	b.Reset()
	_, err = b.WriteString(scriptPowerDown)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "powerdown", b); err != nil {
		log.Fatalln(err)
	}

	// add 'vminit' file to docker build context
	b.Reset()
	_, err = b.WriteString(scriptVmInit)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "vminit", b); err != nil {
		log.Fatalln(err)
	}

	// add 'cgconfig.conf' file to docker build context
	b.Reset()
	_, err = b.WriteString(configCgroup)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "cgconfig.conf", b); err != nil {
		log.Fatalln(err)
	}

	// add 'vector.yaml' file to docker build context
	b.Reset()
	_, err = b.WriteString(configVector)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "vector.yaml", b); err != nil {
		log.Fatalln(err)
	}

	// add 'pgbouncer.ini' file to docker build context
	b.Reset()
	_, err = b.WriteString(configPgbouncer)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "pgbouncer.ini", b); err != nil {
		log.Fatalln(err)
	}

	dockerFileTarReader := bytes.NewReader(buf.Bytes())

	buildArgs := make(map[string]*string)
	buildArgs["DISK_SIZE"] = size
	opt := types.ImageBuildOptions{
		Tags: []string{
			dstIm,
		},
		BuildArgs:      buildArgs,
		SuppressOutput: true,
		NoCache:        false,
		Context:        dockerFileTarReader,
		Dockerfile:     "Dockerfile",
		Remove:         true,
		ForceRemove:    true,
	}
	buildResp, err := cli.ImageBuild(ctx, dockerFileTarReader, opt)
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
