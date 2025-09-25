package main

import (
	"fmt"
	"os/exec"
	"strings"

	"go.uber.org/zap"
)

func resizeSwap(logger *zap.Logger, sizeKB uint64) error {
	swapdisk_bytes, err := exec.Command("blkid", "-L", "swapdisk").Output()
	if err != nil {
		return fmt.Errorf("could not find swap device: %w", err)
	}
	swapdisk := strings.TrimSpace(string(swapdisk_bytes))

	// disable swap. Allow it to fail if it's already disabled.
	_, _ = exec.Command("swapoff", swapdisk).CombinedOutput()

	// re-make the swap.
	output, err := exec.Command("mkswap", "-L", "swapdisk", swapdisk, fmt.Sprintf("%d", sizeKB)).CombinedOutput()
	if err != nil {
		logger.Error(
			"mkswap failed",
			zap.Error(err),
			zap.String("output", string(output)),
		)
		return fmt.Errorf("mkswap failed: %w", err)
	}

	// ... and then re-enable the swap
	//
	// nb: busybox swapon only supports '-d', not its long form '--discard'.
	output, err = exec.Command("swapon", "-d", swapdisk).Output()
	if err != nil {
		logger.Error(
			"swapon failed",
			zap.Error(err),
			zap.String("output", string(output)),
		)
		return fmt.Errorf("swapon failed: %w", err)
	}

	return err
}
