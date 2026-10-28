// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package interceptor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const testGCPID = "projects/test-project/zones/us-central1-a/disks/csi-one"

type gcpFileInfo struct {
	os.FileInfo
	mode os.FileMode
	raw  syscall.Stat_t
}

// Mode returns the fixture device type without requiring a real block device.
func (i gcpFileInfo) Mode() os.FileMode { return i.mode }

// Sys returns fixture device numbers for sysfs and root-device checks.
func (i gcpFileInfo) Sys() any { return &i.raw }

func TestGCPDiskDiscovery(t *testing.T) { testGCPDiskDiscovery(t, testGCPID) }
func TestGCPRegionalDiskDiscovery(t *testing.T) {
	testGCPDiskDiscovery(t, "projects/test-project/regions/us-central1/disks/csi-one")
}

func testGCPDiskDiscovery(t *testing.T, diskID string) {
	for _, layout := range []struct {
		name, data, root, partition, other string
		major                              uint32
	}{
		{"NVMe", "nvme2n1", "nvme0n1/nvme0n1p1", "nvme2n1p1", "nvme1n1", 259},
		{"SCSI", "sdb", "sda/sda1", "sdb1", "sdc", 8},
	} {
		t.Run(layout.name, func(t *testing.T) {
			for _, scenario := range []string{"data", "missing", "wrong-id", "regular-file", "character-device", "partition", "root-disk", "verity-root", "unknown-root"} {
				t.Run(scenario, func(t *testing.T) {
					tmp := t.TempDir()
					byID, sysRoot := filepath.Join(tmp, "by-id"), filepath.Join(tmp, "sys")
					mkdir := func(path string) {
						t.Helper()
						if err := os.MkdirAll(path, 0755); err != nil {
							t.Fatal(err)
						}
					}
					link := func(target, path string) {
						t.Helper()
						if err := os.Symlink(target, path); err != nil {
							t.Fatal(err)
						}
					}
					touch := func(path string) {
						t.Helper()
						if err := os.WriteFile(path, nil, 0600); err != nil {
							t.Fatal(err)
						}
					}
					mkdir(byID)
					mkdir(filepath.Join(sysRoot, "dev/block"))
					dataSys := filepath.Join(sysRoot, "devices/"+layout.data)
					rootSys := filepath.Join(sysRoot, "devices/"+layout.root)
					mkdir(filepath.Join(dataSys, "slaves"))
					mkdir(rootSys)
					touch(filepath.Join(rootSys, "partition"))
					link(dataSys, filepath.Join(sysRoot, fmt.Sprintf("dev/block/%d:2", layout.major)))
					rootLink := filepath.Join(sysRoot, fmt.Sprintf("dev/block/%d:1", layout.major))
					if scenario == "root-disk" {
						rootSys = filepath.Join(dataSys, layout.partition)
						mkdir(rootSys)
						touch(filepath.Join(rootSys, "partition"))
					}
					if scenario == "verity-root" {
						dm := filepath.Join(sysRoot, "devices/dm-0")
						mkdir(filepath.Join(dm, "slaves"))
						part := filepath.Join(dataSys, layout.partition)
						mkdir(part)
						touch(filepath.Join(part, "partition"))
						link(part, filepath.Join(dm, "slaves/"+layout.partition))
						rootSys = dm
					}
					if scenario != "unknown-root" {
						link(rootSys, rootLink)
					}
					device := filepath.Join(tmp, layout.data)
					touch(device)
					// A different disk is present even when the requested identity is absent.
					other := filepath.Join(tmp, layout.other)
					touch(other)
					link(other, filepath.Join(byID, "google-csi-other"))
					if scenario != "missing" && scenario != "wrong-id" {
						link(device, filepath.Join(byID, "google-csi-one"))
					}
					if scenario == "partition" {
						touch(filepath.Join(dataSys, "partition"))
					}
					stat := func(path string) (os.FileInfo, error) {
						if path == "/" {
							return gcpFileInfo{raw: syscall.Stat_t{Dev: unix.Mkdev(layout.major, 1)}}, nil
						}
						info, err := os.Stat(path)
						if err != nil {
							return nil, err
						}
						mode := os.ModeDevice
						if scenario == "regular-file" {
							mode = 0
						}
						if scenario == "character-device" {
							mode |= os.ModeCharDevice
						}
						return gcpFileInfo{FileInfo: info, mode: mode, raw: syscall.Stat_t{Rdev: unix.Mkdev(layout.major, 2)}}, nil
					}
					got, err := findGCPDataDiskAt(diskID, byID, sysRoot, stat)
					if scenario == "data" {
						if err != nil || got != device {
							t.Fatalf("got %q, %v", got, err)
						}
					} else if err == nil || got != "" {
						t.Fatalf("unsafe result %q, %v", got, err)
					}
				})
			}
		})
	}

}

func TestGCPDiscoveryRetryAndNoFallback(t *testing.T) {
	t.Setenv("CAA_DISK_DETECT_MAX_ATTEMPTS", "3")
	t.Setenv("CAA_DISK_DETECT_RETRY_DELAY", "1ns")
	calls := 0
	got, err := findDataDiskDeviceWithGCP(99, testGCPID, func(id string) (string, error) {
		calls++
		if id != testGCPID {
			t.Fatalf("identity changed: %s", id)
		}
		if calls < 3 {
			return "", os.ErrNotExist
		}
		return "/dev/fixture-exact-disk", nil
	})
	if err != nil || got != "/dev/fixture-exact-disk" || calls != 3 {
		t.Fatalf("got %q %v calls=%d", got, err, calls)
	}
	calls = 0
	sentinel := errors.New("exact GCP disk missing")
	got, err = findDataDiskDeviceWithGCP(0, testGCPID, func(string) (string, error) { calls++; return "", sentinel })
	if got != "" || !errors.Is(err, sentinel) || calls != 3 {
		t.Fatalf("must not fall back: %q %v calls=%d", got, err, calls)
	}
}

func TestGCPDiscoveryRejectsMalformedIdentityBeforeLookup(t *testing.T) {
	for _, id := range []string{testGCPID + "/", "/" + testGCPID, "https://compute.googleapis.com/compute/v1/" + testGCPID, strings.Replace(testGCPID, "csi-one", "..", 1), strings.Replace(testGCPID, "csi-one", "disk/other", 1)} {
		got, err := findDataDiskDeviceWithGCP(0, id, func(string) (string, error) { t.Fatal("lookup called for invalid ID"); return "", nil })
		if err == nil || got != "" {
			t.Fatalf("invalid ID accepted: %q %q %v", id, got, err)
		}
	}
}
