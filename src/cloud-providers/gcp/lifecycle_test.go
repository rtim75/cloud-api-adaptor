// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

func TestInstanceCleanupWaitsForInsert(t *testing.T) {
	var complete, missing, deleteFail atomic.Bool
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/instances"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "insert-1", "status": "RUNNING"})
		case strings.HasSuffix(r.URL.Path, "/operations/insert-1"):
			if !complete.Load() {
				http.Error(w, `{"error":{"code":403,"message":"poll denied"}}`, 403)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "insert-1", "status": "DONE"})
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			if deleteFail.Load() {
				http.Error(w, `{"error":{"code":403,"message":"delete denied"}}`, 403)
				return
			}
			if missing.Load() {
				http.Error(w, `{"error":{"code":404,"message":"absent"}}`, 404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "delete-1", "status": "RUNNING"})
		case strings.HasSuffix(r.URL.Path, "/operations/delete-1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "delete-1", "status": "DONE"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 400)
		}
	}))
	defer server.Close()
	client, err := compute.NewInstancesRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	p := &gcpProvider{serviceConfig: &Config{ProjectID: "test-project", Zone: "us-central1-a"}, instancesClient: client}
	req := &computepb.InsertInstanceRequest{Project: "test-project", Zone: "us-central1-a", InstanceResource: &computepb.Instance{Name: proto.String("old")}}
	op, err := p.insertInstance(context.Background(), req, nil)
	if err != nil || op == nil {
		t.Fatalf("insert: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = p.waitForTrackedInstanceInsert(cancelled, op, req); err == nil {
		t.Fatal("cancelled wait returned success")
	}
	if err = p.DeleteInstance(context.Background(), "old"); err == nil {
		t.Fatal("unconfirmed insert allowed deletion")
	}
	if deletes.Load() != 0 {
		t.Fatal("delete ran before insert completed")
	}
	if _, ok := p.pendingInserts.Load("old"); !ok {
		t.Fatal("pending operation lost")
	}
	complete.Store(true)
	deleteFail.Store(true)
	if err = p.DeleteInstance(context.Background(), "old"); err == nil {
		t.Fatal("delete failure hidden")
	}
	deleteFail.Store(false)
	if err = p.DeleteInstance(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	missing.Store(true)
	if err = p.DeleteInstance(context.Background(), "old"); err != nil {
		t.Fatalf("idempotent absence: %v", err)
	}
	if deletes.Load() != 3 {
		t.Fatalf("delete calls: %d", deletes.Load())
	}
}

func TestUnknownInsertCannotBeCleanedFrom404(t *testing.T) {
	// The record models an Insert transport failure after request submission. A
	// single instance 404 cannot rule out a still-running, unobserved operation.
	p := &gcpProvider{}
	p.pendingInserts.Store("uncertain", &instanceInsert{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.DeleteInstance(ctx, "uncertain"); err == nil || !strings.Contains(err.Error(), "unknown insert outcome") {
		t.Fatalf("got %v", err)
	}
	if _, ok := p.pendingInserts.Load("uncertain"); !ok {
		t.Fatal("uncertain owner forgotten")
	}
}

func TestTerminalFailedInsertAllowsAbsenceCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "insert-failed", "status": "RUNNING"})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "insert-failed", "status": "DONE", "httpErrorStatusCode": 503, "httpErrorMessage": "capacity"})
		case http.MethodDelete:
			http.Error(w, `{"error":{"code":404,"message":"absent"}}`, 404)
		}
	}))
	defer server.Close()
	client, err := compute.NewInstancesRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	p := &gcpProvider{serviceConfig: &Config{ProjectID: "test-project", Zone: "us-central1-a"}, instancesClient: client}
	req := &computepb.InsertInstanceRequest{Project: "test-project", Zone: "us-central1-a", InstanceResource: &computepb.Instance{Name: proto.String("failed")}}
	op, err := p.insertInstance(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.waitForTrackedInstanceInsert(context.Background(), op, req); err == nil {
		t.Fatal("failed insert returned success")
	}
	if err = p.DeleteInstance(context.Background(), "failed"); err != nil {
		t.Fatal(err)
	}
}

func TestInsertOutcomeTracking(t *testing.T) {
	for _, code := range []int{403, 409} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "insert rejected or conflicted"}})
			}))
			defer server.Close()
			client, err := compute.NewInstancesRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			p := &gcpProvider{serviceConfig: &Config{ProjectID: "test-project", Zone: "us-central1-a"}, instancesClient: client}
			req := &computepb.InsertInstanceRequest{Project: "test-project", Zone: "us-central1-a", InstanceResource: &computepb.Instance{Name: proto.String("uncertain")}}
			if _, err = p.insertInstance(context.Background(), req, nil); err == nil {
				t.Fatal("insert unexpectedly succeeded")
			}
			_, pending := p.pendingInserts.Load("uncertain")
			if pending != (code == 409) {
				t.Fatalf("code %d pending=%t", code, pending)
			}
			if pending {
				if err = p.DeleteInstance(context.Background(), "uncertain"); err == nil || !strings.Contains(err.Error(), "unknown insert outcome") {
					t.Fatalf("got %v", err)
				}
			}
		})
	}
}
