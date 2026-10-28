// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/option"
)

func TestCredentialsExternalAccount(t *testing.T) {
	for _, impersonate := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "service-account-impersonation"}[impersonate], func(t *testing.T) {
			// All tokens and endpoints belong to this local fixture.
			const subject = "synthetic-projected-oidc-token"
			const audience = "//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/test-pool/providers/test-provider"
			var exchanges, impersonations, requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/sts":
					exchanges.Add(1)
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					if r.Method != http.MethodPost || r.Form.Get("subject_token") != subject || r.Form.Get("audience") != audience || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" {
						t.Error("unexpected STS exchange")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-federated-token", "token_type": "Bearer", "expires_in": 3600, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token"})
				case "/impersonate":
					impersonations.Add(1)
					var body struct {
						Scope []string `json:"scope"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if !impersonate || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer synthetic-federated-token" || len(body.Scope) != 1 || body.Scope[0] != computeScope {
						t.Error("unexpected service account impersonation")
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "synthetic-impersonated-token", "expireTime": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
				case "/compute/v1/projects/test-project/zones/us-central1-a/instances/test-vm":
					requests.Add(1)
					want := "Bearer synthetic-federated-token"
					if impersonate {
						want = "Bearer synthetic-impersonated-token"
					}
					if r.Header.Get("Authorization") != want {
						t.Error("Compute request did not use the configured credentials")
					}
					_, _ = w.Write([]byte(`{"name":"test-vm","id":"123"}`))
				default:
					t.Errorf("unexpected fixture path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			tokenPath := filepath.Join(t.TempDir(), "projected-token")
			if err := os.WriteFile(tokenPath, []byte(subject), 0600); err != nil {
				t.Fatal(err)
			}
			config := map[string]any{"type": "external_account", "audience": audience, "subject_token_type": "urn:ietf:params:oauth:token-type:jwt", "token_url": server.URL + "/sts", "credential_source": map[string]string{"file": tokenPath}}
			if impersonate {
				config["service_account_impersonation_url"] = server.URL + "/impersonate"
			}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			creds, err := loadCredentials(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			client, err := compute.NewInstancesRESTClient(context.Background(), option.WithEndpoint(server.URL), option.WithAuthCredentials(creds))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			for range 2 {
				vm, err := client.Get(context.Background(), &computepb.GetInstanceRequest{Project: "test-project", Zone: "us-central1-a", Instance: "test-vm"})
				if err != nil || vm.GetName() != "test-vm" {
					t.Fatalf("authenticated Compute request failed: %v", err)
				}
			}
			wantImpersonations := int32(0)
			if impersonate {
				wantImpersonations = 1
			}
			if exchanges.Load() != 1 || impersonations.Load() != wantImpersonations || requests.Load() != 2 {
				t.Fatal("unexpected token exchange, impersonation or Compute request count")
			}
		})
	}
}

func TestCredentialsServiceAccount(t *testing.T) {
	// Generate a test-only key in memory; no key or credential JSON is persisted.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || len(strings.Split(r.Form.Get("assertion"), ".")) != 3 {
			t.Error("unexpected service account JWT exchange")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"synthetic-service-account-token","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()
	raw, err := json.Marshal(map[string]string{"type": "service_account", "client_email": "test@test-project.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), "token_uri": server.URL})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := loadCredentials(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	token, err := creds.Token(context.Background())
	if err != nil || token.Value != "synthetic-service-account-token" {
		t.Fatalf("service account authentication failed: %v", err)
	}
}

func TestCredentialsRejectUnsupported(t *testing.T) {
	const canary = "synthetic-private-value-not-for-errors"
	for name, raw := range map[string]string{
		"empty": "", "null": "null", "missing-type": `{}`, "malformed": `{"type":`,
		"authorized-user":      `{"type":"authorized_user","refresh_token":"` + canary + `"}`,
		"impersonated-account": `{"type":"impersonated_service_account"}`,
		"unknown":              `{"type":"unknown"}`, "invalid-type": `{"type":123}`,
	} {
		t.Run(name, func(t *testing.T) {
			creds, err := loadCredentials(raw)
			if err == nil || creds != nil {
				t.Fatal("unsupported or malformed credentials accepted")
			}
			if strings.Contains(err.Error(), canary) {
				t.Fatal("credential value leaked into error")
			}
		})
	}
}
