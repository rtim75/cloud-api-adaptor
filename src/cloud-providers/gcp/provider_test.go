// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	provider "github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

// TestExistingDataDisks checks stable device names and retained, non-boot NVMe attachments.
func TestExistingDataDisks(t *testing.T) {
	const prefix = "projects/test-project/zones/us-central1-a/disks/"
	for _, names := range [][]string{nil, {"csi-cbluaekrkfbgoxkab4yncgutnzmnzx6v5vmfphua7ew5sj33732q"}, {"csi-two", "csi-one"}} {
		var volumes []provider.CloudVolume
		for _, name := range names {
			volumes = append(volumes, provider.CloudVolume{DiskID: prefix + name})
		}
		disks, err := existingDataDisks("test-project", "us-central1-a", volumes)
		if err != nil {
			t.Fatal(err)
		}
		if len(disks) != len(names) {
			t.Fatalf("got %d disks, want %d", len(disks), len(names))
		}
		for i, disk := range disks {
			if disk.GetSource() != prefix+names[i] || disk.GetDeviceName() != names[i] {
				t.Fatalf("disk identity changed: %v", disk)
			}
			if disk.Boot == nil || disk.GetBoot() || disk.AutoDelete == nil || disk.GetAutoDelete() {
				t.Fatalf("data disk must explicitly be non-boot and retained: %v", disk)
			}
			if disk.InitializeParams != nil || disk.GetType() != "PERSISTENT" || disk.GetMode() != "READ_WRITE" || disk.Interface != nil {
				t.Fatalf("unexpected attachment configuration: %v", disk)
			}
		}
	}
}

// TestExistingDataDisksRejectInvalid rejects malformed, duplicate and cross-project or cross-zone disk IDs.
func TestExistingDataDisksRejectInvalid(t *testing.T) {
	const valid = "projects/test-project/zones/us-central1-a/disks/csi-one"
	for _, id := range []string{
		"", "/" + valid, "https://compute.googleapis.com/compute/v1/" + valid,
		valid + "/", valid + "?x=1", strings.Replace(valid, "csi-one", "..", 1),
		strings.Replace(valid, "csi-one", "disk/other", 1),
		strings.Replace(valid, "csi-one", "Disk", 1),
		strings.Replace(valid, "csi-one", strings.Repeat("a", 64), 1),
		strings.Replace(valid, "test-project", "other-project", 1),
		strings.Replace(valid, "us-central1-a", "us-central1-b", 1),
		strings.Replace(valid, "test-project", "123456789", 1),
	} {
		t.Run(id, func(t *testing.T) {
			disks, err := existingDataDisks("test-project", "us-central1-a", []provider.CloudVolume{{DiskID: valid}, {DiskID: id}})
			if err == nil || disks != nil {
				t.Fatalf("invalid path %q returned disks=%v err=%v", id, disks, err)
			}
		})
	}
	disks, err := existingDataDisks("test-project", "us-central1-a", []provider.CloudVolume{{DiskID: valid}, {DiskID: valid}})
	if err == nil || !strings.Contains(err.Error(), "duplicate") || disks != nil {
		t.Fatalf("duplicate returned disks=%v err=%v", disks, err)
	}
}

// Invalid volumes must fail before cloud-init generation or any cloud API call.
func TestCreateInstanceValidatesVolumesBeforeCloudCalls(t *testing.T) {
	p := &gcpProvider{serviceConfig: &Config{ProjectID: "test-project", Zone: "us-central1-a"}}
	instance, err := p.CreateInstance(context.Background(), "pod", "sandbox", nil,
		provider.InstanceTypeSpec{Volumes: []provider.CloudVolume{{DiskID: "invalid"}}})
	if instance != nil || err == nil || !strings.Contains(err.Error(), "invalid CSI volumes") {
		t.Fatalf("got instance=%v err=%v", instance, err)
	}
}

// Exercise real SDK request/response handling and the production preflight →
// Insert boundary. The server is local and no cloud credentials are needed.
func TestInsertInstancePreflight(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   string
		insert bool
	}{
		{"success", "", true}, {"ssd", "", true}, {"hyperdisk", "", true}, {"tdx-ssd", "TDX supports only", false}, {"exact-size-limit", "", true}, {"no-volumes", "", true},
		{"missing", "reading disk", false}, {"forbidden", "reading disk", false},
		{"in-use", "already attached", false}, {"not-ready", "not READY", false},
		{"wrong-type", "unsupported type", false}, {"invalid-size", "invalid size", false},
		{"count-limit", "including boot", false}, {"size-limit", "total limit", false},
		{"boot-limit", "boot disk exceeds", false}, {"unknown-limits", "usable Persistent Disk limits", false},
		{"machine-forbidden", "reading machine type", false},
		{"insert-race", "attached after preflight", true}, {"cancelled", "context canceled", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gets, inserts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fail := func(code int, msg string) {
					w.WriteHeader(code)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
				}
				if r.Method == "POST" {
					inserts.Add(1)
					// The REST request must let Compute Engine choose the disk interface.
					var body struct {
						Disks []map[string]any `json:"disks"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode Insert request: %v", err)
					}
					wantDisks := 3
					if tc.name == "no-volumes" {
						wantDisks = 1
					}
					if len(body.Disks) != wantDisks {
						t.Errorf("Insert disk count = %d, want %d", len(body.Disks), wantDisks)
					}
					for _, disk := range body.Disks {
						if _, present := disk["interface"]; present {
							t.Error("Insert request overrides the server-selected disk interface")
						}
					}
					if tc.name != "no-volumes" && gets.Load() != 3 {
						t.Errorf("Insert before all preflight GETs: %d", gets.Load())
					}
					if tc.name == "insert-race" {
						fail(400, "attached after preflight")
						return
					}
					_, _ = w.Write([]byte(`{"name":"operation-1","status":"DONE"}`))
					return
				}
				gets.Add(1)
				if strings.Contains(r.URL.Path, "/machineTypes/") {
					if tc.name == "machine-forbidden" {
						fail(403, "denied")
						return
					}
					limit, size := 32, int64(100)
					if tc.name == "hyperdisk" {
						limit, size = 0, 0
					}
					if tc.name == "count-limit" {
						limit = 2
					}
					if tc.name == "exact-size-limit" {
						size = 40
					}
					if tc.name == "size-limit" {
						size = 39
					} // boot 20 + two 10 GiB disks
					if tc.name == "boot-limit" {
						size = 19
					}
					if tc.name == "unknown-limits" {
						limit = 0
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"guestCpus": 4, "maximumPersistentDisks": limit, "maximumPersistentDisksSizeGb": fmt.Sprint(size)})
					return
				}
				if !strings.HasPrefix(r.URL.Path, "/compute/v1/projects/test-project/zones/us-central1-a/disks/") {
					t.Errorf("unexpected API path: %s", r.URL.Path)
					fail(400, "unexpected")
					return
				}
				if tc.name == "missing" {
					fail(404, "not found")
					return
				}
				if tc.name == "forbidden" {
					fail(403, "denied")
					return
				}
				disk := map[string]any{"status": "READY", "sizeGb": "10", "type": "projects/test-project/zones/us-central1-a/diskTypes/pd-balanced"}
				switch tc.name {
				case "ssd", "tdx-ssd":
					disk["type"] = "zones/us-central1-a/diskTypes/pd-ssd"
				case "hyperdisk":
					disk["type"] = "zones/us-central1-a/diskTypes/hyperdisk-throughput"
					disk["sizeGb"] = "2048"
					disk["provisionedThroughput"] = "180"
				case "in-use":
					disk["users"] = []string{"projects/test-project/zones/us-central1-a/instances/other"}
				case "not-ready":
					disk["status"] = "CREATING"
				case "wrong-type":
					disk["type"] = "zones/us-central1-a/diskTypes/hyperdisk-balanced-high-availability"
				case "invalid-size":
					disk["sizeGb"] = "0"
				}
				_ = json.NewEncoder(w).Encode(disk)
			}))
			defer server.Close()
			opts := []option.ClientOption{option.WithEndpoint(server.URL), option.WithoutAuthentication()}
			client, err := compute.NewInstancesRESTClient(context.Background(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			p := &gcpProvider{serviceConfig: &Config{ProjectID: "test-project", Zone: "us-central1-a"}, instancesClient: client, clientOptions: opts}
			volumes := []provider.CloudVolume{{DiskID: "projects/test-project/zones/us-central1-a/disks/one"}, {DiskID: "projects/test-project/zones/us-central1-a/disks/two"}}
			if tc.name == "no-volumes" {
				volumes = nil
			}
			data, err := existingDataDisks("test-project", "us-central1-a", volumes)
			if err != nil {
				t.Fatal(err)
			}
			boot := &computepb.AttachedDisk{Boot: proto.Bool(true), InitializeParams: &computepb.AttachedDiskInitializeParams{DiskSizeGb: proto.Int64(20)}}
			req := &computepb.InsertInstanceRequest{Project: "test-project", Zone: "us-central1-a", InstanceResource: &computepb.Instance{MachineType: proto.String("zones/us-central1-a/machineTypes/n2d-standard-2"), Disks: append([]*computepb.AttachedDisk{boot}, data...)}}
			if tc.name == "hyperdisk" {
				req.InstanceResource.MachineType = proto.String("zones/us-central1-a/machineTypes/c4d-standard-4")
				boot.InitializeParams.DiskType = proto.String("zones/us-central1-a/diskTypes/hyperdisk-balanced")
			}
			if tc.name == "tdx-ssd" {
				req.InstanceResource.ConfidentialInstanceConfig = &computepb.ConfidentialInstanceConfig{EnableConfidentialCompute: proto.Bool(true), ConfidentialInstanceType: proto.String("TDX")}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.name == "cancelled" {
				cancel()
			}
			op, err := p.insertInstance(ctx, req, volumes)
			if tc.want == "" {
				if err != nil || op == nil {
					t.Fatalf("op=%v err=%v", op, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q got %v", tc.want, err)
			}
			if (inserts.Load() > 0) != tc.insert {
				t.Fatalf("unexpected Insert count %d", inserts.Load())
			}
			if tc.name == "no-volumes" && gets.Load() != 0 {
				t.Fatal("preflight called without CSI volumes")
			}
		})
	}
}
