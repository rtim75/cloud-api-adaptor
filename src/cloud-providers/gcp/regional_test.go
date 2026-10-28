// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	provider "github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

func TestExistingRegionalDisksPreserveExactIdentity(t *testing.T) {
	const id = "projects/test-project/regions/us-central1/disks/regional"
	for _, zone := range []string{"us-central1-a", "us-central1-b"} {
		disks, err := existingDataDisks("test-project", zone, []provider.CloudVolume{{DiskID: id}})
		if err != nil || len(disks) != 1 {
			t.Fatalf("regional path: %v", err)
		}
		d := disks[0]
		if d.GetSource() != id || d.GetDeviceName() != "regional" || d.GetBoot() || d.GetAutoDelete() || d.Interface != nil {
			t.Fatalf("attachment %+v", d)
		}
	}
	for _, volumes := range [][]provider.CloudVolume{
		{{DiskID: id}, {DiskID: id}},
		{{DiskID: id}, {DiskID: "projects/test-project/zones/us-central1-a/disks/regional"}},
		{{DiskID: "projects/test-project/regions/us-central1-a/disks/regional"}},
		{{DiskID: "projects/test-project/zones/us-central1/disks/regional"}},
		{{DiskID: "projects/test-project/regions/us-east1/disks/regional"}},
	} {
		if _, err := existingDataDisks("test-project", "us-central1-a", volumes); err == nil {
			t.Fatalf("accepted ambiguous placement %v", volumes)
		}
	}
}

func TestRegionalInsertPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, kind, machine, tee string
		size, iops               int64
		outside, attached        bool
		want                     string
	}{
		{"pd", "pd-balanced", "n2d-standard-4", "SEV_SNP", 10, 0, false, false, ""},
		{"ha", "hyperdisk-balanced-high-availability", "c4d-standard-4", "SEV", 10, 5000, false, false, ""},
		{"unrelated-zone", "pd-balanced", "n2d-standard-4", "", 10, 0, true, false, "replica zone"},
		{"attached", "pd-balanced", "n2d-standard-4", "", 10, 0, false, true, "already attached"},
		{"multiwriter", "pd-balanced", "n2d-standard-4", "", 10, 0, false, false, "single-writer"},
		{"wrong-type-location", "pd-balanced", "n2d-standard-4", "", 10, 0, false, false, "placement mismatch"},
		{"regional-standard-minimum", "pd-standard", "n2d-standard-4", "", 10, 0, false, false, "invalid size"},
		{"c3-is-not-regional-pd", "pd-balanced", "c3-standard-4", "TDX", 10, 0, false, false, "regional PD"},
		{"tdx-ha", "hyperdisk-balanced-high-availability", "c4d-standard-4", "TDX", 10, 5000, false, false, "TDX"},
		{"ha-limit", "hyperdisk-balanced-high-availability", "c4d-standard-4", "", 200, 100001, false, false, "performance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inserts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "POST" {
					inserts++
					var instance struct {
						Disks []map[string]any `json:"disks"`
					}
					if err := json.NewDecoder(r.Body).Decode(&instance); err != nil {
						t.Fatal(err)
					}
					if len(instance.Disks) != 2 || instance.Disks[1]["autoDelete"] == true || instance.Disks[1]["deviceName"] != "regional" || instance.Disks[1]["interface"] != nil {
						t.Error("regional identity/retention changed")
					}
					json.NewEncoder(w).Encode(map[string]string{"name": "op", "status": "DONE"})
					return
				}
				if strings.Contains(r.URL.Path, "/machineTypes/") {
					json.NewEncoder(w).Encode(map[string]any{"guestCpus": 4, "maximumPersistentDisks": 32, "maximumPersistentDisksSizeGb": "263168"})
					return
				}
				if !strings.Contains(r.URL.Path, "/regions/us-central1/disks/regional") {
					t.Errorf("wrong regional lookup %s", r.URL.Path)
				}
				zones := []string{"projects/test-project/zones/us-central1-a", "projects/test-project/zones/us-central1-b"}
				if tc.outside {
					zones[1] = "projects/test-project/zones/us-central1-c"
				}
				disk := map[string]any{"name": "regional", "status": "READY", "type": "projects/test-project/regions/us-central1/diskTypes/" + tc.kind, "sizeGb": tc.size, "provisionedIops": tc.iops, "replicaZones": zones}
				if tc.name == "multiwriter" {
					disk["accessMode"] = "READ_WRITE_MANY"
				}
				if tc.name == "wrong-type-location" {
					disk["type"] = "projects/test-project/zones/us-central1-b/diskTypes/pd-balanced"
				}
				if tc.attached {
					disk["users"] = []string{"another-vm"}
				}
				json.NewEncoder(w).Encode(disk)
			}))
			defer server.Close()
			opts := []option.ClientOption{option.WithEndpoint(server.URL), option.WithoutAuthentication()}
			instances, err := compute.NewInstancesRESTClient(context.Background(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer instances.Close()
			p := &gcpProvider{instancesClient: instances, serviceConfig: &Config{ProjectID: "test-project", Zone: "us-central1-b"}, clientOptions: opts}
			volumes := []provider.CloudVolume{{DiskID: "projects/test-project/regions/us-central1/disks/regional"}}
			data, err := existingDataDisks("test-project", "us-central1-b", volumes)
			if err != nil {
				t.Fatal(err)
			}
			bootType := "pd-balanced"
			if strings.HasPrefix(tc.kind, "hyperdisk-") {
				bootType = "hyperdisk-balanced"
			}
			boot := &computepb.AttachedDisk{Boot: proto.Bool(true), InitializeParams: &computepb.AttachedDiskInitializeParams{DiskSizeGb: proto.Int64(10), DiskType: proto.String(bootType)}}
			resource := &computepb.Instance{MachineType: proto.String(tc.machine), Disks: append([]*computepb.AttachedDisk{boot}, data...)}
			if tc.tee != "" {
				resource.ConfidentialInstanceConfig = &computepb.ConfidentialInstanceConfig{EnableConfidentialCompute: proto.Bool(true), ConfidentialInstanceType: proto.String(tc.tee)}
			}
			_, err = p.insertInstance(context.Background(), &computepb.InsertInstanceRequest{Project: "test-project", Zone: "us-central1-b", InstanceResource: resource}, volumes)
			if tc.want == "" {
				if err != nil || inserts != 1 {
					t.Fatalf("insert %d err%v", inserts, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) || inserts != 0 {
				t.Fatalf("want %s without mutation; got %v inserts%d", tc.want, err, inserts)
			}
		})
	}
}
