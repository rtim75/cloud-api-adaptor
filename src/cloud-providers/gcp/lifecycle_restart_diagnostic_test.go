// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"
)

// A fresh provider has no pending-insert state and cannot fence an unknown Insert.
func TestDiagnosticUnknownInsertFenceDoesNotSurviveRestart(t *testing.T) {
	var visible atomic.Bool
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && visible.Load() {
			_, _ = w.Write([]byte(`{"name":"old","id":"123","status":"RUNNING"}`))
			return
		}
		if r.Method != http.MethodDelete || visible.Load() {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		deletes.Add(1)
		http.Error(w, `{"error":{"code":404,"message":"insert not visible yet"}}`, http.StatusNotFound)
	}))
	defer server.Close()
	client, err := compute.NewInstancesRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	newProvider := func() *gcpProvider {
		return &gcpProvider{serviceConfig: &Config{ProjectID: "test-project", Zone: "us-central1-a"}, instancesClient: client}
	}
	before := newProvider()
	// Model an accepted asynchronous Insert whose response/operation ID was
	// lost. Its instance becomes visible only after the cleanup attempt below.
	before.pendingInserts.Store("old", &instanceInsert{})
	if err := before.DeleteInstance(context.Background(), "old"); err == nil || !strings.Contains(err.Error(), "unknown insert outcome") {
		t.Fatalf("live-process fence failed: %v", err)
	}
	if deletes.Load() != 0 {
		t.Fatal("live-process fence submitted a delete")
	}
	after := newProvider() // fresh process state, same external cloud
	if err := after.DeleteInstance(context.Background(), "old"); err != nil {
		t.Fatalf("diagnostic expectation changed: %v", err)
	}
	if deletes.Load() != 1 {
		t.Fatalf("expected one absence check, got %d", deletes.Load())
	}
	visible.Store(true) // accepted old Insert can finish after that 404
	instance, err := client.Get(context.Background(), &computepb.GetInstanceRequest{Project: "test-project", Zone: "us-central1-a", Instance: "old"})
	if err != nil || instance.GetId() != 123 || instance.GetStatus() != "RUNNING" {
		t.Fatalf("late instance visibility: %v, %v", instance, err)
	}
	t.Log("unsupported boundary reproduced: cleanup returned success before an accepted old Insert became visible")
}
