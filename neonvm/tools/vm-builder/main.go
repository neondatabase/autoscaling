package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"text/template"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// vm-builder --src alpine:3.16 -e VAR1=value1 -e VAR2=value2 --dst vm-alpine:dev --file vm-alpine.qcow2

const (
	dockerfileVmBuilder = `
FROM {{.}} AS rootdisk

FROM alpine:3.16 AS vm-runtime
# add busybox
ENV BUSYBOX_VERSION 1.35.0
RUN set -e \
	&& mkdir -p /neonvm/bin \
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
	&& mv /sbin/acpid         /neonvm/bin/ \
	&& mv /sbin/udevd         /neonvm/bin/ \
	&& mv /sbin/agetty        /neonvm/bin/ \
	&& mv /sbin/su-exec       /neonvm/bin/ \
	&& mv /usr/sbin/resize2fs /neonvm/bin/resize2fs \
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
	&& cp -f /lib/libcom_err.so.2      /neonvm/lib/libcom_err.so.2

# tools for qemu disk creation
RUN set -e \
	&& apk add --no-cache --no-progress --quiet \
		qemu-img \
		e2fsprogs

# init scripts
ADD inittab  /neonvm/bin/inittab
ADD vminit   /neonvm/bin/vminit
ADD vmacpi   /neonvm/acpi/vmacpi
RUN chmod +x /neonvm/bin/vminit

FROM vm-runtime AS builder
ARG DISK_SIZE
COPY --from=rootdisk / /rootdisk
COPY --from=vm-runtime /neonvm /rootdisk/neonvm
COPY vmstart /rootdisk/neonvm/bin/vmstart
RUN set -e \
    && chmod +x /rootdisk/neonvm/bin/vmstart \
    && mkdir -p /rootdisk/etc \
    && cp -f /rootdisk/neonvm/bin/inittab /rootdisk/etc/inittab \
    && mkfs.ext4 -d /rootdisk /disk.raw ${DISK_SIZE} \
    && qemu-img convert -f raw -O qcow2 -o cluster_size=2M,lazy_refcounts=on /disk.raw /disk.qcow2

FROM alpine:3.16
RUN apk add --no-cache --no-progress --quiet qemu-img
COPY --from=builder /disk.qcow2 /
`
	scriptVmStart = `#!/neonvm/bin/sh
{{ range .Env }}
export {{.}}
{{- end }}
{{- range .ExtraEnv }}
export {{.}}
{{- end }}

/neonvm/bin/su-exec {{.User}} {{- range .Entrypoint}} {{.}} {{- end }} {{- range .Cmd }} {{.}} {{- end }}
`

	scriptInitTab = `
::sysinit:/neonvm/bin/vminit

::respawn:/neonvm/bin/udevd
::respawn:/neonvm/bin/acpid -f -c /neonvm/acpi

::respawn:/neonvm/bin/vmstart

ttyS0::respawn:/neonvm/bin/agetty --8bits --local-line --noissue --noclear --noreset --host console --login-program /neonvm/bin/login --autologin root 115200 ttyS0 linux
`

	scriptVmAcpi = `
event=button/power
action=/neonvm/bin/poweroff
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
mount -t devpts -o noexec,nosuid       devpts    /dev/pts
mount -t tmpfs  -o noexec,nosuid,nodev shm-tmpfs /dev/shm

# try resize filesystem
resize2fs /dev/vda

# networking
ip link set up dev lo
ETH_LIST=$(find /sys/class/net -mindepth 1 -maxdepth 1 -name "eth*")
for i in ${ETH_LIST}; do
    iface=$(basename $i)
    ip link set up dev $iface
    udhcpc -t 1 -T 1 -A 1 -b -q -i $iface -O 121 -O 119
done
`
)

var (
	srcImage = flag.String("src", "", `Docker image used as source for virtual machine disk image: --src=alpine:3.16`)
	dstImage = flag.String("dst", "", `Docker image with resulting disk image: --dst=vm-alpine:3.16`)
	size     = flag.String("size", "1G", `Size for disk image: --size=1G`)
	outFile  = flag.String("file", "", `Save disk image as file: --file=vm-alpine.qcow2`)
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
	ExtraEnv   []string
}

func main() {
	flag.Parse()

	if len(*srcImage) == 0 {
		log.Println("-src not set, see usage info:")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if len(*dstImage) == 0 {
		log.Println("-dst not set, see usage info:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	ctx := context.Background()

	log.Println("Setup docker connection")
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalln(err)
	}
	defer cli.Close()

	// pull source image
	log.Printf("Pull source docker image: %s\n", *srcImage)
	pull, err := cli.ImagePull(ctx, *srcImage, types.ImagePullOptions{})
	if err != nil {
		log.Fatalln(err)
	}
	defer pull.Close()

	// dp quiet pull
	io.Copy(io.Discard, pull)
	/*
	   if err = printReader(pull); err != nil {
	   	log.Fatalln(err)
	   }
	*/

	log.Printf("Build docker image for virtual machine (disk size %s): %s\n", *size, *dstImage)
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
	if len(SrcImageSpec.Entrypoint) == 0 && len(SrcImageSpec.Cmd) == 0 {
		SrcImageSpec.Cmd = []string{"/neonvm/bin/sleep", "3650d"}
	}

	// !!!!!
	// TODO: delete it as it temporary solution for debug
	SrcImageSpec.Env = imageSpec.Config.Env
	SrcImageSpec.ExtraEnv = []string{"POSTGRES_HOST_AUTH_METHOD=trust"}
	// !!!!!

	buf := new(bytes.Buffer)
	tw := tar.NewWriter(buf)
	defer tw.Close()

	dockerfileVmBuilderTmpl, err := template.New("vm-builder").Parse(dockerfileVmBuilder)
	if err != nil {
		log.Fatal(err)
	}
	var dockerfileVmBuilderBuffer bytes.Buffer
	err = dockerfileVmBuilderTmpl.Execute(&dockerfileVmBuilderBuffer, *srcImage)
	if err != nil {
		log.Fatalln(err)
	}

	scriptVmStartTmpl, err := template.New("vmstart").Parse(scriptVmStart)
	if err != nil {
		log.Fatal(err)
	}
	var scriptVmStartBuffer bytes.Buffer
	err = scriptVmStartTmpl.Execute(&scriptVmStartBuffer, SrcImageSpec)
	if err != nil {
		log.Fatalln(err)
	}

	if err = AddToTar(tw, "Dockerfile", dockerfileVmBuilderBuffer); err != nil {
		log.Fatalln(err)
	}

	if err = AddToTar(tw, "vmstart", scriptVmStartBuffer); err != nil {
		log.Fatalln(err)
	}

	var b bytes.Buffer

	_, err = b.WriteString(scriptInitTab)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "inittab", b); err != nil {
		log.Fatalln(err)
	}

	b.Reset()
	_, err = b.WriteString(scriptVmAcpi)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "vmacpi", b); err != nil {
		log.Fatalln(err)
	}

	b.Reset()
	_, err = b.WriteString(scriptVmInit)
	if err != nil {
		log.Fatalln(err)
	}
	if err = AddToTar(tw, "vminit", b); err != nil {
		log.Fatalln(err)
	}

	dockerFileTarReader := bytes.NewReader(buf.Bytes())

	buildArgs := make(map[string]*string)
	buildArgs["DISK_SIZE"] = size

	opt := types.ImageBuildOptions{
		Tags: []string{
			*dstImage,
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

	// quiet build
	io.Copy(io.Discard, buildResp.Body)
	/*
	   if err = printReader(buildResp.Body); err != nil {
	   	log.Fatalln(err)
	   }
	*/

	if len(*outFile) != 0 {
		log.Printf("Save disk image as %s", *outFile)
		containerResp, err := cli.ContainerCreate(ctx, &container.Config{
			Image:      *dstImage,
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

		fromContainer, _, err := cli.CopyFromContainer(ctx, containerResp.ID, "/disk.qcow2")
		if err != nil {
			log.Fatalln(err)
		}

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

		if err = cli.ContainerRemove(ctx, containerResp.ID, types.ContainerRemoveOptions{}); err != nil {
			log.Println(err)
		}

	}

}
