package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alessio/shellescape"
	"github.com/kdomanski/iso9660"
	"go.uber.org/zap"

	"k8s.io/apimachinery/pkg/api/resource"

	vmv1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
)

const (
	rootDiskPath    = "/vm/images/rootdisk.qcow2"
	runtimeDiskPath = "/vm/images/runtime.iso"
	mountedDiskPath = "/vm/images"

	toolsDiskPath = "/vm/tools.iso"

	sshAuthorizedKeysDiskPath   = "/vm/images/ssh-authorized-keys.iso"
	sshAuthorizedKeysMountPoint = "/vm/ssh"

	swapName = "swapdisk"
)

// setupVMDisks creates the disks for the VM and returns the appropriate QEMU args
func setupVMDisks(
	logger *zap.Logger,
	cfg *Config,
	enableSSH bool,
	swapSize *resource.Quantity,
	extraDisks []vmv1.Disk,
) ([]string, error) {
	var qemuCmd []string

	qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=rootdisk,file=%s,if=virtio,media=disk,index=0,%s", rootDiskPath, cfg.diskCacheSettings))
	qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=runtime,file=%s,if=virtio,media=cdrom,readonly=on,cache=none", runtimeDiskPath))

	{
		name := "vm-tools"
		if err := createISO9660FromPath(logger, name, toolsDiskPath, cfg.toolsPath); err != nil {
			return nil, fmt.Errorf("Failed to create ISO9660 image: %w", err)
		}
		qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=cdrom,cache=none", name, toolsDiskPath))
	}

	if enableSSH {
		name := "ssh-authorized-keys"
		if err := createISO9660FromPath(logger, name, sshAuthorizedKeysDiskPath, sshAuthorizedKeysMountPoint); err != nil {
			return nil, fmt.Errorf("failed to create ISO9660 image: %w", err)
		}
		qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=cdrom,cache=none", name, sshAuthorizedKeysDiskPath))
	}

	if swapSize != nil {
		dPath := fmt.Sprintf("%s/swapdisk.qcow2", mountedDiskPath)
		logger.Info("creating QCOW2 image for swap", zap.String("diskPath", dPath))
		if err := createSwap(dPath, swapSize); err != nil {
			return nil, fmt.Errorf("failed to create swap disk: %w", err)
		}
		qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=disk,%s,discard=unmap", swapName, dPath, cfg.diskCacheSettings))
	}

	for _, disk := range extraDisks {
		switch {
		case disk.EmptyDisk != nil:
			logger.Info("creating QCOW2 image with empty ext4 filesystem", zap.String("diskName", disk.Name))
			dPath := fmt.Sprintf("%s/%s.qcow2", mountedDiskPath, disk.Name)
			if err := createQCOW2(disk.Name, dPath, &disk.EmptyDisk.Size, nil); err != nil {
				return nil, fmt.Errorf("failed to create QCOW2 image: %w", err)
			}
			discard := ""
			if disk.EmptyDisk.Discard {
				discard = ",discard=unmap"
			}
			qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=disk,%s%s", disk.Name, dPath, cfg.diskCacheSettings, discard))
		case disk.ConfigMap != nil || disk.Secret != nil:
			dPath := fmt.Sprintf("%s/%s.iso", mountedDiskPath, disk.Name)
			mnt := fmt.Sprintf("/vm/mounts%s", disk.MountPath)
			logger.Info("creating iso9660 image", zap.String("diskPath", dPath), zap.String("diskName", disk.Name), zap.String("mountPath", mnt))
			if err := createISO9660FromPath(logger, disk.Name, dPath, mnt); err != nil {
				return nil, fmt.Errorf("failed to create ISO9660 image: %w", err)
			}
			qemuCmd = append(qemuCmd, "-drive", fmt.Sprintf("id=%s,file=%s,if=virtio,media=cdrom,cache=none", disk.Name, dPath))
		default:
			// do nothing
		}
	}

	return qemuCmd, nil
}

func resizeRootDisk(logger *zap.Logger, vmSpec *vmv1.VirtualMachineSpec) error {
	// resize rootDisk image of size specified and new size more than current
	type QemuImgOutputPartial struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	// get current disk size by qemu-img info command
	qemuImgOut, err := exec.Command(qemuImgBin, "info", "--output=json", rootDiskPath).Output()
	if err != nil {
		return fmt.Errorf("could not get root image size: %w", err)
	}
	var imageSize QemuImgOutputPartial
	if err := json.Unmarshal(qemuImgOut, &imageSize); err != nil {
		return fmt.Errorf("failed to unmarshal QEMU image size: %w", err)
	}
	imageSizeQuantity := resource.NewQuantity(imageSize.VirtualSize, resource.BinarySI)

	// going to resize
	if !vmSpec.Guest.RootDisk.Size.IsZero() {
		if vmSpec.Guest.RootDisk.Size.Cmp(*imageSizeQuantity) == 1 {
			logger.Info(fmt.Sprintf("resizing rootDisk from %s to %s", imageSizeQuantity.String(), vmSpec.Guest.RootDisk.Size.String()))
			if err := execFg(qemuImgBin, "resize", rootDiskPath, fmt.Sprintf("%d", vmSpec.Guest.RootDisk.Size.Value())); err != nil {
				return fmt.Errorf("failed to resize rootDisk: %w", err)
			}
		} else {
			logger.Info(fmt.Sprintf("rootDisk.size (%s) is less than than image size (%s)", vmSpec.Guest.RootDisk.Size.String(), imageSizeQuantity.String()))
		}
	}
	return nil
}

func createISO9660runtime(
	diskPath string,
	command []string,
	args []string,
	sysctl []string,
	env []vmv1.EnvVar,
	disks []vmv1.Disk,
	enableSSH bool,
	swapSize *resource.Quantity,
	shmsize *resource.Quantity,
) error {
	writer, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer writer.Cleanup() //nolint:errcheck // Nothing to do with the error, maybe log it ? TODO

	if len(sysctl) != 0 {
		err = writer.AddFile(bytes.NewReader([]byte(strings.Join(sysctl, "\n"))), "sysctl.conf")
		if err != nil {
			return err
		}
	}

	if len(command) != 0 {
		err = writer.AddFile(bytes.NewReader([]byte(shellescape.QuoteCommand(command))), "command.sh")
		if err != nil {
			return err
		}
	}

	if len(args) != 0 {
		err = writer.AddFile(bytes.NewReader([]byte(shellescape.QuoteCommand(args))), "args.sh")
		if err != nil {
			return err
		}
	}

	if len(env) != 0 {
		envstring := []string{}
		for _, e := range env {
			envstring = append(envstring, fmt.Sprintf(`export %s=%s`, e.Name, shellescape.Quote(e.Value)))
		}
		envstring = append(envstring, "")
		err = writer.AddFile(bytes.NewReader([]byte(strings.Join(envstring, "\n"))), "env.sh")
		if err != nil {
			return err
		}
	}

	mounts := []string{
		"set -euo pipefail",
	}
	if enableSSH {
		mounts = append(mounts, "/neonvm/bin/mkdir -p /mnt/ssh")
		mounts = append(mounts, "/neonvm/bin/mount -t iso9660 -o ro,mode=0644 $(/neonvm/bin/blkid -L ssh-authorized-keys) /mnt/ssh")
	}

	if swapSize != nil {
		mounts = append(mounts, fmt.Sprintf("/neonvm/bin/sh /neonvm/runtime/resize-swap-internal.sh %d", swapSize.Value()))
	}

	// Add tools.
	mounts = append(mounts, "/neonvm/bin/mkdir -p /neonvm/tools")
	mounts = append(mounts, "/neonvm/bin/mount -t iso9660 -o ro,exec $(/neonvm/bin/blkid -L vm-tools) /neonvm/tools")

	if len(disks) != 0 {
		for _, disk := range disks {
			if disk.MountPath != "" {
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mkdir -p %s`, disk.MountPath))
			}
			if disk.Watch != nil && *disk.Watch {
				// do nothing as we will mount it into the VM via neonvm-daemon later
				continue
			}
			switch {
			case disk.EmptyDisk != nil:
				opts := ""
				if disk.EmptyDisk.Discard {
					opts = "-o discard"
				}

				if disk.EmptyDisk.EnableQuotas {
					mounts = append(mounts, fmt.Sprintf(`tune2fs -Q prjquota $(/neonvm/bin/blkid -L %s)`, disk.Name))
					mounts = append(mounts, fmt.Sprintf(`tune2fs -E mount_opts=prjquota $(/neonvm/bin/blkid -L %s)`, disk.Name))
				}

				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount %s $(/neonvm/bin/blkid -L %s) %s`, opts, disk.Name, disk.MountPath))
				// Note: chmod must be after mount, otherwise it gets overwritten by mount.
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/chmod 0777 %s`, disk.MountPath))
			case disk.ConfigMap != nil || disk.Secret != nil:
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount -t iso9660 -o ro,mode=0644 $(/neonvm/bin/blkid -L %s) %s`, disk.Name, disk.MountPath))
			case disk.Tmpfs != nil:
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/chmod 0777 %s`, disk.MountPath))
				mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount -t tmpfs -o size=%d %s %s`, disk.Tmpfs.Size.Value(), disk.Name, disk.MountPath))
			default:
				// do nothing
			}
		}
	}

	if shmsize != nil {
		mounts = append(mounts, fmt.Sprintf(`/neonvm/bin/mount -o remount,size=%d /dev/shm`, shmsize.Value()))
	}

	mounts = append(mounts, "")
	err = writer.AddFile(bytes.NewReader([]byte(strings.Join(mounts, "\n"))), "mounts.sh")
	if err != nil {
		return err
	}

	if swapSize != nil {
		lines := []string{
			`#!/neonvm/bin/sh`,
			`set -euo pipefail`,
			// this script may be run as root, so we should avoid potentially-malicious path
			// injection
			`export PATH="/neonvm/bin"`,
			fmt.Sprintf(`swapdisk="$(/neonvm/bin/blkid -L %s)"`, swapName),
			// disable swap. Allow it to fail if it's already disabled.
			`swapoff "$swapdisk" || true`,
			// if the requested size is zero, then... just exit. There's nothing we need to do.
			`new_size="$1"`,
			`if [ "$new_size" = '0' ]; then exit 0; fi`,
			// re-make the swap.
			// mkswap expects the size to be given in KiB, so divide the new size by 1K
			fmt.Sprintf(`mkswap -L %s "$swapdisk" $(( new_size / 1024 ))`, swapName),
			// ... and then re-enable the swap
			//
			// nb: busybox swapon only supports '-d', not its long form '--discard'.
			`swapon -d "$swapdisk"`,
		}
		err = writer.AddFile(bytes.NewReader([]byte(strings.Join(lines, "\n"))), "resize-swap-internal.sh")
		if err != nil {
			return err
		}
	}

	outputFile, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}

	// uid=36(qemu) gid=34(kvm) groups=34(kvm)
	err = outputFile.Chown(36, 34)
	if err != nil {
		return err
	}

	err = writer.WriteTo(outputFile, "vmruntime")
	if err != nil {
		return err
	}

	err = outputFile.Close()
	if err != nil {
		return err
	}

	return nil
}

func calcDirUsage(dirPath string) (int64, error) {
	stat, err := os.Lstat(dirPath)
	if err != nil {
		return 0, err
	}

	size := stat.Size()

	if !stat.IsDir() {
		return size, nil
	}

	dir, err := os.Open(dirPath)
	if err != nil {
		return size, err
	}
	defer dir.Close()

	files, err := dir.Readdir(-1)
	if err != nil {
		return size, err
	}

	for _, file := range files {
		if file.Name() == "." || file.Name() == ".." {
			continue
		}
		s, err := calcDirUsage(dirPath + "/" + file.Name())
		if err != nil {
			return size, err
		}
		size += s
	}
	return size, nil
}

func createSwap(diskPath string, swapSize *resource.Quantity) error {
	tmpRawFile := "swap.raw"

	if err := execFg(qemuImgBin, "create", "-q", "-f", "raw", tmpRawFile, fmt.Sprintf("%d", swapSize.Value())); err != nil {
		return err
	}
	if err := execFg("mkswap", "-L", swapName, tmpRawFile); err != nil {
		return err
	}

	if err := execFg(qemuImgBin, "convert", "-q", "-f", "raw", "-O", "qcow2", "-o", "cluster_size=2M,lazy_refcounts=on", tmpRawFile, diskPath); err != nil {
		return err
	}

	if err := execFg("rm", "-f", tmpRawFile); err != nil {
		return err
	}

	// uid=36(qemu) gid=34(kvm) groups=34(kvm)
	if err := execFg("chown", "36:34", diskPath); err != nil {
		return err
	}

	return nil
}

func createQCOW2(diskName string, diskPath string, diskSize *resource.Quantity, contentPath *string) error {
	ext4blocksMin := int64(64)
	ext4blockSize := int64(4096)
	ext4blockCount := int64(0)

	if diskSize != nil {
		ext4blockCount = diskSize.Value() / ext4blockSize
	} else if contentPath != nil {
		dirSize, err := calcDirUsage(*contentPath)
		if err != nil {
			return err
		}
		ext4blockCount = int64(math.Ceil(float64(ext4blocksMin) + float64((dirSize / ext4blockSize))))
	} else {
		return errors.New("diskSize or contentPath should be specified")
	}

	mkfsArgs := []string{
		"-q", // quiet
		"-L", // volume-label
		diskName,
	}

	if contentPath != nil {
		// [ -d root-directory|tarball ]
		mkfsArgs = append(mkfsArgs, "-d", *contentPath)
	}

	mkfsArgs = append(
		mkfsArgs,
		"-b", // block-size
		fmt.Sprintf("%d", ext4blockSize),
		"ext4.raw",                        // device
		fmt.Sprintf("%d", ext4blockCount), // fs-size
	)

	if err := execFg("mkfs.ext4", mkfsArgs...); err != nil {
		return err
	}

	if err := execFg(qemuImgBin, "convert", "-q", "-f", "raw", "-O", "qcow2", "-o", "cluster_size=2M,lazy_refcounts=on", "ext4.raw", diskPath); err != nil {
		return err
	}

	if err := execFg("rm", "-f", "ext4.raw"); err != nil {
		return err
	}

	// uid=36(qemu) gid=34(kvm) groups=34(kvm)
	if err := execFg("chown", "36:34", diskPath); err != nil {
		return err
	}

	return nil
}

func createISO9660FromPath(logger *zap.Logger, diskName string, diskPath string, contentPath string) error {
	writer, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer writer.Cleanup() //nolint:errcheck // Nothing to do with the error, maybe log it ? TODO

	dir, err := os.Open(contentPath)
	if err != nil {
		return err
	}
	dirEntrys, err := dir.ReadDir(0)
	if err != nil {
		return err
	}

	for _, file := range dirEntrys {
		fileName := fmt.Sprintf("%s/%s", contentPath, file.Name())
		outputPath := file.Name()

		if file.IsDir() {
			continue
		}
		// try to resolve symlink and check resolved file IsDir
		resolved, err := filepath.EvalSymlinks(fileName)
		if err != nil {
			return err
		}
		resolvedOpen, err := os.Open(resolved)
		if err != nil {
			return err
		}
		resolvedStat, err := resolvedOpen.Stat()
		if err != nil {
			return err
		}
		if resolvedStat.IsDir() {
			continue
		}

		// run the file handling logic in a closure, so the defers happen within the loop body,
		// rather than the outer function.
		err = func() error {
			logger.Info("adding file to ISO9660 disk", zap.String("path", outputPath))
			fileToAdd, err := os.Open(fileName)
			if err != nil {
				return err
			}
			defer fileToAdd.Close()

			return writer.AddFile(fileToAdd, outputPath)
		}()
		if err != nil {
			return err
		}
	}

	outputFile, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	// uid=36(qemu) gid=34(kvm) groups=34(kvm)
	err = outputFile.Chown(36, 34)
	if err != nil {
		return err
	}

	err = writer.WriteTo(outputFile, diskName)
	if err != nil {
		return err
	}

	err = outputFile.Close()
	if err != nil {
		return err
	}

	return nil
}
