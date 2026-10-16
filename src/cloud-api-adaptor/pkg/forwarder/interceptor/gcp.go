// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package interceptor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var gcpDiskPath = regexp.MustCompile(`^projects/([a-z][a-z0-9-]{4,28}[a-z0-9])/zones/([a-z][a-z0-9-]*[0-9]-[a-z])/disks/([a-z](?:[a-z0-9-]{0,61}[a-z0-9])?)$`)

// findGCPDataDisk resolves the exact google-<deviceName> symlink for a zonal
// disk and rejects partitions, non-block devices and disks backing the root filesystem.
func findGCPDataDisk(diskID string) (string, error) {
	return findGCPDataDiskAt(diskID, "/dev/disk/by-id", "/sys", os.Stat)
}

// findGCPDataDiskAt validates the by-id target and root exclusion using the given
// device and sysfs directories. The stat callback permits testing without block devices.
func findGCPDataDiskAt(diskID, byID, sysRoot string, stat func(string) (os.FileInfo, error)) (string, error) {
	parts := gcpDiskPath.FindStringSubmatch(diskID)
	if parts == nil {
		return "", fmt.Errorf("invalid GCP disk path %q", diskID)
	}
	device, err := filepath.EvalSymlinks(filepath.Join(byID, "google-"+parts[3]))
	if err != nil {
		return "", fmt.Errorf("finding exact GCP device: %w", err)
	}
	info, err := stat(device)
	if err != nil {
		return "", err
	}
	if info.Mode()&(os.ModeDevice|os.ModeCharDevice) != os.ModeDevice {
		return "", fmt.Errorf("GCP device %s is not a block device", device)
	}
	ds, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("missing device number for %s", device)
	}
	sysDevice, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "dev/block", fmt.Sprintf("%d:%d", unix.Major(ds.Rdev), unix.Minor(ds.Rdev))))
	if err != nil {
		return "", fmt.Errorf("resolving GCP sysfs device: %w", err)
	}
	if _, err := os.Stat(filepath.Join(sysDevice, "partition")); !os.IsNotExist(err) {
		return "", fmt.Errorf("GCP device %s is a partition or cannot be verified", device)
	}
	root, err := stat("/")
	if err != nil {
		return "", fmt.Errorf("identifying root filesystem: %w", err)
	}
	rs, ok := root.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("missing root filesystem device number")
	}
	rootPath := filepath.Join(sysRoot, "dev/block", fmt.Sprintf("%d:%d", unix.Major(rs.Dev), unix.Minor(rs.Dev)))
	boot, err := isRootBackingDisk(rootPath, sysDevice, make(map[string]bool))
	if err != nil {
		return "", fmt.Errorf("verifying root disk exclusion: %w", err)
	}
	if boot {
		return "", fmt.Errorf("GCP device %s backs the root filesystem", device)
	}
	return device, nil
}

// isRootBackingDisk follows root partitions and device-mapper slaves to detect
// whether candidate backs the root filesystem, including dm-verity images.
// The seen set prevents cycles; unreadable or unresolved topology returns an error.
func isRootBackingDisk(rootPath, candidate string, seen map[string]bool) (bool, error) {
	root, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return false, err
	}
	if root == candidate || strings.HasPrefix(root, candidate+string(os.PathSeparator)) {
		return true, nil
	}
	if seen[root] {
		return false, nil
	}
	seen[root] = true
	slaves, err := os.ReadDir(filepath.Join(root, "slaves"))
	if os.IsNotExist(err) {
		// Partitions need not have a slaves directory.
		if _, partitionErr := os.Stat(filepath.Join(root, "partition")); partitionErr == nil {
			return false, nil
		}
	}
	if err != nil {
		return false, err
	}
	for _, slave := range slaves {
		match, err := isRootBackingDisk(filepath.Join(root, "slaves", slave.Name()), candidate, seen)
		if err != nil || match {
			return match, err
		}
	}
	return false, nil
}
