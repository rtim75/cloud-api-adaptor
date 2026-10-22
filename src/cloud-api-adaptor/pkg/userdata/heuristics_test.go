package userdata

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cpuid "github.com/klauspost/cpuid/v2"
)

type gcpMetadataRoundTripper func(*http.Request) (*http.Response, error)

func (f gcpMetadataRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func gcpMetadataResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func setGCPTestTransport(t *testing.T, tr http.RoundTripper) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = tr
	t.Cleanup(func() { http.DefaultTransport = old })
}

func TestGCPDetectionLocalIdentityAndMetadataFallback(t *testing.T) {
	oldVendor := cpuid.CPU.HypervisorVendorID
	t.Cleanup(func() { cpuid.CPU.HypervisorVendorID = oldVendor })
	for _, tc := range []struct {
		name, product string
		missing       bool
		kvm           bool
		status        int
		want          bool
		calls         int
	}{
		{name: "GCE before metadata and without KVM CPUID", product: "Google Compute Engine\n", want: true},
		{name: "legacy Google product", product: "Google\n", want: true},
		{name: "trim whitespace", product: " Google Compute Engine \n", want: true},
		{name: "unknown product metadata fallback", product: "unknown", kvm: true, status: 200, want: true, calls: 1},
		{name: "missing DMI metadata fallback", missing: true, kvm: true, status: 200, want: true, calls: 1},
		{name: "Google hardware is not enough", product: "Google Pixel", kvm: true, status: 404, calls: 1},
		{name: "missing DMI unavailable metadata preserves baseline", missing: true, kvm: true, status: 503, calls: 1},
		{name: "unknown non KVM preserves baseline", product: "unknown", status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpuid.CPU.HypervisorVendorID = cpuid.UNKNOWN
			if tc.kvm {
				cpuid.CPU.HypervisorVendorID = cpuid.KVM
			}
			path := filepath.Join(t.TempDir(), "product_name")
			if !tc.missing {
				if err := os.WriteFile(path, []byte(tc.product), 0600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			setGCPTestTransport(t, gcpMetadataRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != GcpImdsURL || r.Header.Get("Metadata-Flavor") != "Google" {
					t.Errorf("unexpected metadata detection request")
				}
				return gcpMetadataResponse(r, tc.status, "{}"), nil
			}))
			if got := isGCPVMWithProductName(context.Background(), path); got != tc.want {
				t.Fatalf("GCP=%v, want %v", got, tc.want)
			}
			if calls != tc.calls {
				t.Fatalf("metadata calls=%d, want %d", calls, tc.calls)
			}
		})
	}
}

type fastGCPUserDataProvider struct{ GCPUserDataProvider }

func (fastGCPUserDataProvider) GetRetryDelay() time.Duration { return time.Millisecond }

func TestGCPDetectedLocallyRetrievesConfigWithExistingRetry(t *testing.T) {
	oldVendor := cpuid.CPU.HypervisorVendorID
	cpuid.CPU.HypervisorVendorID = cpuid.UNKNOWN
	t.Cleanup(func() { cpuid.CPU.HypervisorVendorID = oldVendor })
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprintf("permanent_failure=%v", permanent), func(t *testing.T) {
			dir := t.TempDir()
			product := filepath.Join(dir, "product_name")
			apf := filepath.Join(dir, "apf.json")
			if err := os.WriteFile(product, []byte("Google Compute Engine\n"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			setGCPTestTransport(t, gcpMetadataRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != GcpUserDataImdsURL || r.Header.Get("Metadata-Flavor") != "Google" {
					return nil, fmt.Errorf("unexpected request: detection must not use HTTP")
				}
				calls++
				if permanent || calls == 1 {
					return gcpMetadataResponse(r, 503, "synthetic unavailable"), nil
				}
				config := fmt.Sprintf("write_files:\n  - path: %s\n    content: '{}'\n", apf)
				return gcpMetadataResponse(r, 200, base64.StdEncoding.EncodeToString([]byte(config))), nil
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if !isGCPVMWithProductName(ctx, product) {
				t.Fatal("local GCP identity was lost before retrieval")
			}
			cc, err := retrieveCloudConfig(ctx, fastGCPUserDataProvider{})
			if permanent {
				if err == nil {
					t.Fatal("persistent metadata failure returned success")
				}
				if _, err := os.Stat(apf); !os.IsNotExist(err) {
					t.Fatal("failed retrieval created APF config")
				}
				if calls == 0 {
					t.Fatal("metadata was never queried")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg := NewConfig(1)
			cfg.writeFiles = []string{apf}
			if err := processCloudConfig(cfg, cc); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(apf)
			if err != nil || string(data) != "{}" {
				t.Fatalf("config missing after metadata recovery: err=%v", err)
			}
			if calls != 2 {
				t.Fatalf("userdata calls=%d, want failed attempt plus successful retry", calls)
			}
		})
	}
}
