package main

import (
	"fmt"
	"os/exec"

	"go.uber.org/zap"
)

func setDiskQuota(logger *zap.Logger, path string, sizeBytes uint64) error {
	// setquota -P <project_id> <block-softlimit> <block-hardlimit> <inode-softlimit> <inode-hardlimit> <filesystem>
	output, err := exec.Command("setquota", "-P", "0", "0", fmt.Sprintf("%d", sizeBytes), "0", "0", path).CombinedOutput()
	if err != nil {
		logger.Error(
			"setquota failed",
			zap.Error(err),
			zap.String("output", string(output)),
		)
		return fmt.Errorf("setquota failed: %w", err)
	}

	output, err = exec.Command("repquota", "-P", "-a").CombinedOutput()
	if err != nil {
		logger.Error(
			"repquota failed",
			zap.Error(err),
			zap.String("output", string(output)),
		)
		return fmt.Errorf("repquota failed: %w", err)
	}

	return err
}
