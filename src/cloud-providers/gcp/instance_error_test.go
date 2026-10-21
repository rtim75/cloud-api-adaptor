// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

// Exercise the SDK operation failure used by CreateInstance, including the
// encoded cloud-init carried by the request. All key material here is synthetic.
func TestInstanceInsertWaitDoesNotExposeMetadata(t *testing.T) {
	const secret = "-----BEGIN PRIVATE KEY-----\nsynthetic-test-key\n-----END PRIVATE KEY-----"
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "capacity-failure"}[failed], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				op := map[string]any{"name": "operation-1", "status": "DONE"}
				if r.Method == http.MethodGet && failed {
					op["httpErrorStatusCode"] = 503
					op["httpErrorMessage"] = "ZONE_RESOURCE_POOL_EXHAUSTED"
				}
				_ = json.NewEncoder(w).Encode(op)
			}))
			defer server.Close()
			client, err := compute.NewInstancesRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			req := &computepb.InsertInstanceRequest{Project: "test-project", Zone: "us-central1-a", InstanceResource: &computepb.Instance{
				Name: proto.String("test-vm"), Metadata: &computepb.Metadata{Items: []*computepb.Items{
					{Key: proto.String("user-data"), Value: proto.String(encoded)},
					{Key: proto.String("other-sensitive-metadata"), Value: proto.String(secret)},
				}},
			}}
			op, err := client.Insert(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			err = waitForInstanceInsert(context.Background(), op, req)
			if !failed {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("failed operation returned success")
			}
			for _, sensitive := range []string{"synthetic-test-key", encoded, "user-data", "other-sensitive-metadata"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Error("operation error exposes request metadata")
				}
			}
			for _, diagnostic := range []string{"test-project", "us-central1-a", "test-vm", "operation-1", "ZONE_RESOURCE_POOL_EXHAUSTED"} {
				if !strings.Contains(err.Error(), diagnostic) {
					t.Errorf("operation error lost diagnostic %q", diagnostic)
				}
			}
			var apiErr *googleapi.Error
			if !errors.As(err, &apiErr) || apiErr.Code != 503 {
				t.Error("operation error lost wrapped API status")
			}
		})
	}
}
