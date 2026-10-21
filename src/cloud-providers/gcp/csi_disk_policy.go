// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
)

// diskBudget tracks PD and combined disk limits, including the boot disk.
// Compute Engine validates machine-specific Hyperdisk limits at instance creation.
type diskBudget struct {
	machine        string
	mt             *computepb.MachineType
	confidential   *computepb.ConfidentialInstanceConfig
	count, pdCount int64
	total, pdTotal int64
	hyperdisk      bool
}

func (b *diskBudget) add(path string, size, iops, throughput int64, boot bool) error {
	kind := path[strings.LastIndex(path, "/")+1:]
	if kind == "" && boot {
		kind = "pd-standard" // Compute API boot default.
	}
	minSize, maxSize := int64(10), int64(65536)
	switch kind {
	case "pd-standard", "pd-balanced", "pd-ssd":
	case "pd-extreme":
		minSize = 500
	case "hyperdisk-balanced":
		minSize = 4
	case "hyperdisk-extreme":
		minSize = 64
	case "hyperdisk-throughput":
		minSize, maxSize = 2048, 32768
	default:
		return fmt.Errorf("unsupported type %q (zonal CSI disks required)", kind)
	}
	if size < minSize || size > maxSize {
		return fmt.Errorf("invalid size %d GiB for %s (%d..%d)", size, kind, minSize, maxSize)
	}
	if boot && (kind == "hyperdisk-extreme" || kind == "hyperdisk-throughput") {
		return fmt.Errorf("%s cannot be a boot disk", kind)
	}
	if err := b.compatible(kind); err != nil {
		return err
	}
	// Native provisioned fields are checked when present. Boot defaults are not
	// resolved until Insert; actual existing data disks have already been created.
	if err := diskPerformance(kind, size, iops, throughput); err != nil {
		return err
	}
	b.count++
	b.total += size
	hd := strings.HasPrefix(kind, "hyperdisk-")
	b.hyperdisk = b.hyperdisk || hd
	if !hd {
		b.pdCount++
		b.pdTotal += size
	}
	if b.confidential.GetEnableConfidentialCompute() && b.count > 40 {
		return fmt.Errorf("Confidential VM allows at most 40 disks including boot")
	}
	if b.count > 128 {
		return fmt.Errorf("at most 128 disks including boot")
	}
	if b.pdCount > 0 {
		countLimit, sizeLimit := int64(b.mt.GetMaximumPersistentDisks()), b.mt.GetMaximumPersistentDisksSizeGb()
		if countLimit <= 0 || sizeLimit <= 0 {
			return fmt.Errorf("machine type %q does not report usable Persistent Disk limits", b.machine)
		}
		if b.pdCount > countLimit {
			return fmt.Errorf("machine type %q allows %d Persistent Disks including boot", b.machine, countLimit)
		}
		if b.pdTotal > min(sizeLimit, 257*1024) {
			return fmt.Errorf("Persistent Disks exceed machine type %q total limit %d GiB including boot", b.machine, sizeLimit)
		}
	}
	if b.hyperdisk {
		limit := int64(257 * 1024)
		if b.mt.GetGuestCpus() >= 32 {
			limit = 512 * 1024
		}
		if b.total > limit {
			return fmt.Errorf("disks including boot exceed combined total limit %d GiB", limit)
		}
	}
	return nil
}

func (b *diskBudget) compatible(kind string) error {
	cvm := b.confidential.GetEnableConfidentialCompute()
	tee := b.confidential.GetConfidentialInstanceType()
	if cvm && tee == "TDX" && kind != "pd-balanced" {
		return fmt.Errorf("TDX supports only pd-balanced")
	}
	if cvm && kind == "pd-extreme" {
		return fmt.Errorf("pd-extreme has no supported Confidential VM machine type")
	}
	if cvm && tee == "SEV" && strings.HasPrefix(b.machine, "c3d-") && (kind == "hyperdisk-balanced" || kind == "hyperdisk-throughput") {
		return fmt.Errorf("%s is unsupported on C3D SEV", kind)
	}
	// Compute Engine additionally validates the CPU platform for PD Extreme.
	if kind == "pd-extreme" && !(strings.HasPrefix(b.machine, "m2-") || strings.HasPrefix(b.machine, "m3-") || (strings.HasPrefix(b.machine, "n2-") && b.mt.GetGuestCpus() >= 64)) {
		return fmt.Errorf("pd-extreme requires N2 >=64 vCPUs, M2 or M3")
	}
	return nil
}

func diskPerformance(kind string, size, i, t int64) error {
	invalid := false
	switch kind {
	case "pd-extreme":
		invalid = (i != 0 && (i < 2500 || i > 120000)) || t != 0
	case "hyperdisk-balanced":
		lo, hi := int64(3000), min(500*size, 160000)
		if size < 6 {
			lo, hi = 500*size, 500*size
		}
		invalid = (i != 0 && (i < lo || i > hi)) || (t != 0 && (t < 140 || t > 2400 || 4*t > hi || (i != 0 && (4*t > i || i > 256*t))))
	case "hyperdisk-extreme":
		invalid = t != 0 || (i != 0 && (i < 2*size || i > min(1200*size, 350000)))
	case "hyperdisk-throughput":
		invalid = i != 0 || (t != 0 && (t < max(20, (5*size+1023)/1024) || t > min(2400, 90*size/1024)))
	default:
		invalid = i != 0 || t != 0
	}
	if invalid {
		return fmt.Errorf("invalid provisioned performance for %s at %d GiB", kind, size)
	}
	return nil
}
