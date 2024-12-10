package main

import (
	"archive/tar"
	"bytes"
	"context"
	_ "embed"
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
	"github.com/distribution/reference"
	cliconfig "github.com/docker/cli/cli/config"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// vm-builder --src alpine:3.19 --dst vm-alpine:dev --file vm-alpine.qcow2

var (
	//go:embed files/Dockerfile.img
	dockerfileVmBuilder string
	//go:embed files/helper.move-bins.sh
	scriptMoveBinsHelper string
	//go:embed files/vmstart
	scriptVmStart string
	//go:embed files/inittab
	scriptInitTab string
	//go:embed files/vmacpi
	scriptVmAcpi string
	//go:embed files/vmshutdown
	scriptVmShutdown string
	//go:embed files/vminit
	scriptVmInit string
	//go:embed files/udev-init.sh
	scriptUdevInit string
	//go:embed files/resize-swap.sh
	scriptResizeSwap string
	//go:embed files/set-disk-quota.sh
	scriptSetDiskQuota string
	//go:embed files/vector.yaml
	configVector string
	//go:embed files/chrony.conf
	configChrony string
	//go:embed files/sshd_config
	configSshd string
)

const (
	targetArchLinuxAmd64 = "linux/amd64"
	targetArchLinuxArm64 = "linux/arm64"
)

var (
	Version           string
	NeonvmDaemonImage string

	srcImage  = flag.String("src", "", `Docker image used as source for virtual machine disk image: --src=alpine:3.19`)
	dstImage  = flag.String("dst", "", `Docker image with resulting disk image: --dst=vm-alpine:3.19`)
	size      = flag.String("size", "1G", `Size for disk image: --size=1G`)
	outFile   = flag.String("file", "", `Save disk image as file: --file=vm-alpine.qcow2`)
	specFile  = flag.String("spec", "", `File containing additional customization: --spec=spec.yaml`)
	quiet     = flag.Bool("quiet", false, `Show less output from the docker build process`)
	forcePull = flag.Bool("pull", false, `Pull src image even if already present locally`)
	version   = flag.Bool("version", false, `Print vm-builder version`)

	daemonImageFlag = flag.String("daemon-image", "", `Specify the neonvm-daemon image: --daemon-image=neonvm-daemon:dev`)
	targetArch      = flag.String("target-arch", "", fmt.Sprintf("Target architecture: --arch %s | %s", targetArchLinuxAmd64, targetArchLinuxArm64))
)

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
		Mode: 0o755, // TODO: shouldn't just set this for everything.
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

	NeonvmDaemonImage string

	SpecBuild       string
	SpecMerge       string
	InittabCommands []inittabCommand
	AgettyTTY       string
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
	if len(*daemonImageFlag) == 0 && len(NeonvmDaemonImage) == 0 {
		log.Println("neonvm-daemon image not set, needs to be explicitly passed in, or compiled with -ldflags '-X main.NeonvmDaemonImage=...'")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if targetArch == nil || *targetArch == "" {
		log.Println("Target architecture not set, see usage info:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if *targetArch != targetArchLinuxAmd64 && *targetArch != targetArchLinuxArm64 {
		log.Fatalf("Unsupported target architecture: %q", *targetArch)
		flag.PrintDefaults()
		return
	}

	neonvmDaemonImage := NeonvmDaemonImage
	if len(*daemonImageFlag) != 0 {
		neonvmDaemonImage = *daemonImageFlag
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

	log.Println("Load docker credentials")
	dockerConfig, err := cliconfig.Load("" /* auto-detect right directory */)
	if err != nil {
		log.Fatalln(err)
	}

	credentials, err := dockerConfig.GetAllCredentials()
	if err != nil {
		log.Fatalln(err)
	}
	authConfigs := make(map[string]registry.AuthConfig)
	for key, value := range credentials {
		log.Printf("Found docker credentials for %s", key)
		authConfigs[key] = registry.AuthConfig(value)
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
			log.Fatalln(err) //nolint:gocritic // linter complains that Fatalln circumvents deferred cli.Close(). Too much work to fix in #721, leaving for later.
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
		// use a closure so deferred close is closer
		err := func() error {
			named, err := reference.ParseNormalizedNamed(*srcImage)
			if err != nil {
				return err
			}
			reg := reference.Domain(named)

			imagePullOptions := types.ImagePullOptions{}
			if authConfig, ok := authConfigs[reg]; ok {
				encoded, err := registry.EncodeAuthConfig(authConfig)
				if err != nil {
					return err
				}
				imagePullOptions.RegistryAuth = encoded
			} else {
				// Special case handling of docker.io weirdness.
				// ref https://github.com/moby/moby/blob/e7347f8a8c2fd3d2abd34b638d6fc8c18b0278d1/registry/config.go#L26-L49
				// (and other handling around index.docker.io in that file...)
				//
				// See also e.g. https://github.com/containrrr/watchtower/issues/1176
				legacyConfig, hasLegacyDockerConfig := authConfigs["https://index.docker.io/v1/"]
				if hasLegacyDockerConfig && (reg == "docker.io" || reg == "registry-1.docker.io") {
					encoded, err := registry.EncodeAuthConfig(legacyConfig)
					if err != nil {
						return err
					}
					imagePullOptions.RegistryAuth = encoded
				} else {
					log.Printf("No docker credentials found for %s", reg)
				}
			}

			log.Printf("Pull source docker image: %s", *srcImage)
			pull, err := cli.ImagePull(ctx, *srcImage, imagePullOptions)
			if err != nil {
				return err
			}
			defer pull.Close()
			// do quiet pull - discard output
			_, err = io.Copy(io.Discard, pull)
			return err
		}()
		if err != nil {
			log.Fatalln(err)
		}

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
		User:          "root", // overridden below, if imageSpec.Config.User != ""
		Entrypoint:    imageSpec.Config.Entrypoint,
		Cmd:           imageSpec.Config.Cmd,
		Env:           imageSpec.Config.Env,
		RootDiskImage: *srcImage,

		NeonvmDaemonImage: neonvmDaemonImage,

		SpecBuild:       "",  // overridden below if spec != nil
		SpecMerge:       "",  // overridden below if spec != nil
		InittabCommands: nil, // overridden below if spec != nil
		ShutdownHook:    "",  // overridden below if spec != nil
		AgettyTTY:       getAgettyTTY(*targetArch),
	}

	if len(imageSpec.Config.User) != 0 {
		tmplArgs.User = imageSpec.Config.User
	}

	tarBuffer := new(bytes.Buffer)
	tw := tar.NewWriter(tarBuffer)
	defer tw.Close()

	if spec != nil {
		tmplArgs.SpecBuild = spec.Build
		tmplArgs.SpecMerge = spec.Merge
		tmplArgs.ShutdownHook = strings.ReplaceAll(spec.ShutdownHook, "\n", "\n\t")

		for _, c := range spec.Commands {
			// Allow core dumps for all inittab targets
			c.Shell = fmt.Sprintf("ulimit -c unlimited; %s", c.Shell)
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
		{"helper.move-bins.sh", scriptMoveBinsHelper},
		{"vmstart", scriptVmStart},
		{"vmshutdown", scriptVmShutdown},
		{"inittab", scriptInitTab},
		{"vmacpi", scriptVmAcpi},
		{"vminit", scriptVmInit},
		{"vector.yaml", configVector},
		{"chrony.conf", configChrony},
		{"sshd_config", configSshd},
		{"udev-init.sh", scriptUdevInit},
		{"resize-swap.sh", scriptResizeSwap},
		{"set-disk-quota.sh", scriptSetDiskQuota},
	}

	for _, f := range files {
		if err := AddTemplatedFileToTar(tw, tmplArgs, f.filename, f.tmpl); err != nil {
			log.Fatalln(err)
		}
	}

	buildArgs := make(map[string]*string)
	buildArgs["DISK_SIZE"] = size
	buildArgs["TARGET_ARCH"] = targetArch
	opt := types.ImageBuildOptions{
		AuthConfigs:    authConfigs,
		Tags:           []string{dstIm},
		BuildArgs:      buildArgs,
		SuppressOutput: *quiet,
		NoCache:        false,
		Context:        tarBuffer,
		Dockerfile:     "Dockerfile",
		Remove:         true,
		ForceRemove:    true,
		Platform:       *targetArch,
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
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				log.Fatalln(err)
			}

			if header.Name != "disk.qcow2" {
				log.Printf("skip file %s", header.Name)
				continue
			}
			path := filepath.Join(*outFile) //nolint:gocritic // FIXME: this is probably incorrect, intended to join with header.Name ?
			info := header.FileInfo()

			// Open and write to the file inside a closure, so we can defer close
			err = func() error {
				file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
				if err != nil {
					return err
				}
				defer file.Close()
				_, err = io.Copy(file, tarReader)
				return err
			}()
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

// getAgettyTTY returns the tty device name for agetty based on the target architecture.
func getAgettyTTY(targetArch string) string {
	switch targetArch {
	case targetArchLinuxAmd64:
		return "ttyS0"
	case targetArchLinuxArm64:
		return "ttyAMA0"
	default:
		log.Fatalf("Unsupported target architecture: %q", targetArch)
		return ""
	}
}
