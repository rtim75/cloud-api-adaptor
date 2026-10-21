// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"testing"

	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

func testDiskBudget(machine, tee string) *diskBudget {
	b := &diskBudget{machine: machine, mt: &computepb.MachineType{GuestCpus: proto.Int32(64), MaximumPersistentDisks: proto.Int32(32), MaximumPersistentDisksSizeGb: proto.Int64(257 * 1024)}}
	if tee != "" {
		b.confidential = &computepb.ConfidentialInstanceConfig{EnableConfidentialCompute: proto.Bool(true), ConfidentialInstanceType: proto.String(tee)}
	}
	return b
}

func TestCSIDiskTypesAndTEE(t *testing.T) {
	for _, tc := range []struct {
		machine, tee, kind  string
		size, i, throughput int64
		valid               bool
	}{
		{"n2d-standard-2", "", "pd-standard", 10, 0, 0, true},
		{"n2d-standard-2", "SEV", "pd-ssd", 10, 0, 0, true},
		{"n2d-standard-2", "SEV_SNP", "pd-standard", 10, 0, 0, true},
		{"c3-standard-4", "TDX", "pd-balanced", 10, 0, 0, true},
		{"c3-standard-4", "TDX", "pd-ssd", 10, 0, 0, false},
		{"c3-standard-4", "TDX", "hyperdisk-balanced", 10, 5000, 500, false},
		{"n2-standard-64", "", "pd-extreme", 500, 2500, 0, true},
		{"n2d-standard-64", "", "pd-extreme", 500, 2500, 0, false},
		{"n2d-standard-64", "SEV", "pd-extreme", 500, 2500, 0, false},
		{"m3-ultramem-64", "", "pd-extreme", 500, 120000, 0, true},
		{"c4d-standard-4", "SEV", "hyperdisk-balanced", 4, 2000, 140, true},
		{"c3d-standard-4", "SEV", "hyperdisk-balanced", 4, 2000, 140, false},
		{"c3d-standard-4", "", "hyperdisk-balanced", 4, 2000, 140, true},
		{"c4d-standard-64", "SEV", "hyperdisk-extreme", 64, 128, 0, true},
		{"c4d-standard-4", "SEV", "hyperdisk-throughput", 2048, 0, 180, true},
		{"c3d-standard-4", "SEV", "hyperdisk-throughput", 2048, 0, 180, false},
		{"c4d-standard-4", "", "hyperdisk-throughput", 2048, 0, 181, false},
		{"c4d-standard-4", "", "hyperdisk-throughput", 2048, 1, 180, false},
		{"c4d-standard-4", "", "hyperdisk-extreme", 64, 127, 0, false},
		{"c4d-standard-4", "", "hyperdisk-balanced", 5, 3000, 140, false},
		{"c4d-standard-4", "", "hyperdisk-balanced-high-availability", 10, 0, 0, false},
	} {
		t.Run(tc.machine+tc.tee+tc.kind, func(t *testing.T) {
			err := testDiskBudget(tc.machine, tc.tee).add(tc.kind, tc.size, tc.i, tc.throughput, false)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestCSIPDHyperdiskAndBootBudgets(t *testing.T) {
	b := testDiskBudget("c3-standard-64", "")
	b.mt.MaximumPersistentDisks = proto.Int32(1)
	b.mt.MaximumPersistentDisksSizeGb = proto.Int64(20)
	if err := b.add("pd-balanced", 20, 0, 0, true); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if err := b.add("hyperdisk-balanced", 100, 3000, 140, false); err != nil {
			t.Fatal(err)
		}
	}
	if b.pdCount != 1 || b.pdTotal != 20 || b.count != 9 || b.total != 820 {
		t.Fatalf("wrong budget: %+v", b)
	}
	if err := b.add("pd-ssd", 10, 0, 0, false); err == nil {
		t.Fatal("PD budget ignored boot")
	}
	for _, tee := range []string{"", "SEV"} {
		b = testDiskBudget("c4d-standard-64", tee)
		b.mt.MaximumPersistentDisks = nil
		b.mt.MaximumPersistentDisksSizeGb = nil
		if err := b.add("hyperdisk-balanced", 10, 3000, 140, true); err != nil {
			t.Fatal(err)
		}
		limit := 128
		if tee != "" {
			limit = 40
		}
		for n := 1; n < limit; n++ {
			if err := b.add("hyperdisk-balanced", 4, 2000, 140, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.add("hyperdisk-balanced", 4, 2000, 140, false); err == nil {
			t.Fatal("combined count ignored boot")
		}
	}
	for _, cpus := range []int32{4, 64} {
		b = testDiskBudget("c4d-standard-64", "")
		b.mt.GuestCpus = proto.Int32(cpus)
		if err := b.add("hyperdisk-balanced", 1024, 0, 0, true); err != nil {
			t.Fatal(err)
		}
		for range 4 {
			if err := b.add("hyperdisk-balanced", 65536, 0, 0, false); err != nil {
				t.Fatal(err)
			}
		}
		err := b.add("hyperdisk-balanced", 4, 2000, 140, false)
		if (err == nil) != (cpus >= 32) {
			t.Fatalf("CPU=%d error=%v", cpus, err)
		}
	}
	b = testDiskBudget("n2-standard-32", "")
	b.mt.GuestCpus = proto.Int32(32)
	if err := b.add("pd-extreme", 500, 2500, 0, false); err == nil {
		t.Fatal("N2 <64 accepted")
	}
	if err := b.add("hyperdisk-extreme", 64, 1000, 0, true); err == nil {
		t.Fatal("unsupported boot accepted")
	}
}

func TestCSIBootDiskTypes(t *testing.T) {
	b := testDiskBudget("n2-standard-64", "")
	if err := b.add("pd-extreme", 500, 0, 0, true); err != nil {
		t.Fatalf("compatible PD Extreme boot disk rejected: %v", err)
	}
	if err := b.add("pd-ssd", 10, 0, 0, false); err != nil {
		t.Fatal(err)
	}
	if b.pdCount != 2 || b.pdTotal != 510 {
		t.Fatalf("boot missing from PD budget: %+v", b)
	}
	for _, kind := range []string{"hyperdisk-extreme", "hyperdisk-throughput"} {
		if err := testDiskBudget("c4d-standard-64", "").add(kind, 2048, 0, 0, true); err == nil {
			t.Fatalf("unsupported %s boot disk accepted", kind)
		}
	}
}
