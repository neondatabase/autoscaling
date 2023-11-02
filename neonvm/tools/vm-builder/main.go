package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/alessio/shellescape"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// vm-builder --src alpine:3.16 --dst vm-alpine:dev --file vm-alpine.qcow2

const (
	dockerfileVmBuilder = `
{{.SpecBuild}}

FROM {{.RootDiskImage}} AS rootdisk

# Temporarily set to root in order to do the "merge" step, so that it's possible to make changes in
# the final VM to files owned by root, even if the source image sets the user to something else.
USER root
{{.SpecMerge}}

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
::once:/neonvm/bin/touch /neonvm/vmstart.allowed
::respawn:/neonvm/bin/udhcpc -t 1 -T 1 -A 1 -f -i eth0 -O 121 -O 119 -s /neonvm/bin/udhcpc.script
::respawn:/neonvm/bin/udevd
::respawn:/neonvm/bin/acpid -f -c /neonvm/acpi
::respawn:/neonvm/bin/vector -c /neonvm/config/vector.yaml --config-dir /etc/vector
::respawn:/neonvm/bin/vmstart
{{ range .InittabCommands }}
::{{.SysvInitAction}}:su -p {{.CommandUser}} -c {{.ShellEscapedCommand}}
{{ end }}
ttyS0::respawn:/neonvm/bin/agetty --8bits --local-line --noissue --noclear --noreset --host console --login-program /neonvm/bin/login --login-pause --autologin root 115200 ttyS0 linux
::shutdown:/neonvm/bin/vmshutdown
`

	scriptVmAcpi = `
event=button/power
action=/neonvm/bin/poweroff
`

	scriptVmShutdown = `#!/neonvm/bin/sh
rm /neonvm/vmstart.allowed
{{if .ShutdownHook}}
if [ -e /neonvm/vmstart.allowed ]; then
	echo "Error: could not remove vmstart.allowed marker, might hang indefinitely during shutdown" 1>&2
fi
# we inhibited new command starts, but there may still be a command running
while ! /neonvm/bin/flock -n /neonvm/vmstart.lock true; do
	echo 'Running shutdown hook...'
	{{.ShutdownHook}}
	sleep 0.5s # make sure we don't spin if things aren't working
done
echo "vmstart workload shut down cleanly" 1>&2
{{end}}
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

# set any user-supplied sysctl settings
test -f /neonvm/runtime/sysctl.conf && /neonvm/bin/sysctl -p /neonvm/runtime/sysctl.conf

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
)

var (
	Version string

	srcImage  = flag.String("src", "", `Docker image used as source for virtual machine disk image: --src=alpine:3.16`)
	dstImage  = flag.String("dst", "", `Docker image with resulting disk image: --dst=vm-alpine:3.16`)
	size      = flag.String("size", "1G", `Size for disk image: --size=1G`)
	outFile   = flag.String("file", "", `Save disk image as file: --file=vm-alpine.qcow2`)
	specFile  = flag.String("spec", "", `File containing additional customization: --spec=spec.yaml`)
	quiet     = flag.Bool("quiet", false, `Show less output from the docker build process`)
	forcePull = flag.Bool("pull", false, `Pull src image even if already present locally`)
	version   = flag.Bool("version", false, `Print vm-builder version`)
)

type dockerMessage struct {
	Stream string `json:"stream"`
	Error  string `json:"error"`
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

	return addFileToTar(tw, filename, buf.Bytes())
}

func addFileToTar(tw *tar.Writer, filename string, contents []byte) error {
	tarHeader := &tar.Header{
		Name: filename,
		Size: int64(len(contents)),
		Mode: 0755, // TODO: shouldn't just set this for everything.
	}

	if err := tw.WriteHeader(tarHeader); err != nil {
		return fmt.Errorf("failed to write tar header for %q: %w", filename, err)
	}
	if _, err := tw.Write(contents); err != nil {
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

	SpecBuild       string
	SpecMerge       string
	InittabCommands []inittabCommand
	ShutdownHook    string
}

type inittabCommand struct {
	SysvInitAction      string
	CommandUser         string
	ShellEscapedCommand string
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

	var spec *imageSpec
	if *specFile != "" {
		var err error
		spec, err = readImageSpec(*specFile)
		if err != nil {
			log.Fatalln(err)
			os.Exit(1)
		}
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
	}

	if len(imageSpec.Config.User) != 0 {
		tmplArgs.User = imageSpec.Config.User
	} else {
		tmplArgs.User = "root"
	}

	tarBuffer := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuffer)
	defer tw.Close()

	if spec != nil {
		tmplArgs.SpecBuild = spec.Build
		tmplArgs.SpecMerge = spec.Merge
		tmplArgs.ShutdownHook = strings.ReplaceAll(spec.ShutdownHook, "\n", "\n\t")

		for _, c := range spec.Commands {
			tmplArgs.InittabCommands = append(tmplArgs.InittabCommands, inittabCommand{
				SysvInitAction:      c.SysvInitAction,
				CommandUser:         c.User,
				ShellEscapedCommand: shellescape.Quote(c.Shell),
			})
		}
		for _, f := range spec.Files {
			var contents []byte
			switch {
			case f.Content != nil:
				contents = []byte(*f.Content)
			case f.HostPath != nil:
				// the 'host path' is relative to the directory that the spec file is in
				path := filepath.Join(filepath.Dir(*specFile), *f.HostPath)

				var err error
				contents, err = os.ReadFile(path)
				if err != nil {
					err = fmt.Errorf("failed to read file %q: %w", path, err)
					log.Fatalln(err)
				}
			}

			if err := addFileToTar(tw, f.Filename, contents); err != nil {
				log.Fatalln(err)
			}
		}
	}

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
		{"vector.yaml", configVector},
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

	defer buildResp.Body.Close()

	out := io.Writer(os.Stdout)
	if *quiet {
		out = io.Discard
	}
	err = jsonmessage.DisplayJSONMessagesStream(buildResp.Body, out, os.Stdout.Fd(), term.IsTerminal(int(os.Stdout.Fd())), nil)
	if err != nil {
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

type imageSpec struct {
	Commands     []command `yaml:"commands"`
	ShutdownHook string    `yaml:"shutdownHook,omitempty"`
	Build        string    `yaml:"build"`
	Merge        string    `yaml:"merge"`
	Files        []file    `yaml:"files"`
}

type command struct {
	Name           string `yaml:"name"`
	User           string `yaml:"user"`
	SysvInitAction string `yaml:"sysvInitAction"`
	Shell          string `yaml:"shell"`
}

type file struct {
	Filename string  `yaml:"filename"`
	HostPath *string `yaml:"hostPath,omitempty"`
	Content  *string `yaml:"content,omitempty"`
}

func readImageSpec(path string) (*imageSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file at %q: %w", path, err)
	}

	var spec imageSpec

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // disallow unknown fields
	if err := dec.Decode(&spec); err != nil {
		return nil, err
	}

	var errs []error

	for i, c := range spec.Commands {
		for _, e := range c.validate() {
			errs = append(errs, fmt.Errorf("error in commands[%d]: %w", i, e))
		}
	}
	for i, f := range spec.Files {
		for _, e := range f.validate() {
			errs = append(errs, fmt.Errorf("error in files[%d]: %w", i, e))
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid image spec: %w", err)
	}

	return &spec, nil
}

func (c command) validate() []error {
	checkNonempty := func(errs *[]error, field string, value string) {
		if value == "" {
			*errs = append(*errs, fmt.Errorf("command must have non-empty field '%s'", field))
		}
	}

	var errs []error

	checkNonempty(&errs, "name", c.Name)
	checkNonempty(&errs, "user", c.User)
	checkNonempty(&errs, "sysvInitAction", c.SysvInitAction)
	checkNonempty(&errs, "shell", c.Shell)

	return errs
}

func (f file) validate() []error {
	var errs []error

	if f.Filename == "" {
		errs = append(errs, errors.New("file must have non-empty field 'filename'"))
	}

	if f.HostPath == nil && f.Content == nil {
		errs = append(errs, errors.New("file missing either 'hostPath' or 'content'"))
	} else if f.HostPath != nil && f.Content != nil {
		errs = append(errs, errors.New("file must have only one of 'hostPath' or 'content'"))
	}

	return errs
}
