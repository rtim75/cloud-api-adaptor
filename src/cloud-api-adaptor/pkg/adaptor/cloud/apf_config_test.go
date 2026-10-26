// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cri "github.com/containerd/containerd/pkg/cri/annotations"
	pb "github.com/kata-containers/kata-containers/src/runtime/protocols/hypervisor"

	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/adaptor/proxy"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/forwarder"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/util/tlsutil"
)

type apfTestCA struct{}

func (apfTestCA) RootCertificate() []byte { return []byte("synthetic-root-certificate") }
func (apfTestCA) Issue(string) ([]byte, []byte, error) {
	return []byte("synthetic-server-certificate"), []byte("synthetic-private-key"), nil
}

type apfTestProxy struct {
	proxy.AgentProxy
	ca tlsutil.CAService
}

func (p *apfTestProxy) CAService() tlsutil.CAService { return p.ca }

type apfTestFactory struct{ tls bool }

func (f apfTestFactory) New(name, path string) proxy.AgentProxy {
	p := &apfTestProxy{AgentProxy: (&mockProxyFactory{}).New(name, path)}
	if f.tls {
		p.ca = apfTestCA{}
	}
	return p
}

func newAPFTestService(dir string, tls bool) *cloudService {
	cfg := &ServerConfig{PodsDir: dir, ForwarderPort: forwarder.DefaultListenPort}
	if tls {
		cfg.TLSConfig = &tlsutil.TLSConfig{
			MinTLSVersion: "VersionTLS12", CipherSuites: []string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"},
		}
	}
	// No Kubernetes client, real cloud credentials or external network access.
	s := &cloudService{provider: &mockProvider{}, proxyFactory: apfTestFactory{tls: tls},
		workerNode: &mockWorkerNode{}, serverConfig: cfg, sandboxes: map[sandboxID]*sandbox{}}
	s.cond = sync.NewCond(&s.mutex)
	return s
}

func apfTestRequest() *pb.CreateVMRequest {
	return &pb.CreateVMRequest{Id: "private-apf", Annotations: map[string]string{
		cri.SandboxNamespace: "default", cri.SandboxName: "private-apf-pod",
	}}
}

func TestCreateVMPrivateAPFConfig(t *testing.T) {
	if os.Getenv("CAA_APF_UMASK_CHILD") != "1" {
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		for _, mask := range []string{"000", "022", "077"} {
			t.Run("umask-"+mask, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, "bash", "-c", `umask "$1"; exec "$2" -test.run '^TestCreateVMPrivateAPFConfig$' -test.v`, "apf-test", mask, binary)
				cmd.Env = append(os.Environ(), "CAA_APF_UMASK_CHILD=1")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("permission subprocess failed: %v\n%s", err, output)
				}
			})
		}
		return
	}
	for _, tls := range []bool{false, true} {
		for _, layout := range []string{"new", "existing", "symlink", "hardlink"} {
			t.Run(fmt.Sprintf("tls-%v/%s", tls, layout), func(t *testing.T) {
				dir := t.TempDir()
				podDir := filepath.Join(dir, "private-apf")
				if err := os.MkdirAll(podDir, 0755); err != nil {
					t.Fatal(err)
				}
				// Preserve an explicitly configured directory mode, independent of umask.
				if err := os.Chmod(podDir, 0755); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(podDir, "apf.json")
				target := filepath.Join(dir, "legacy-target")
				const old = "synthetic-legacy-data"
				if layout != "new" {
					if err := os.WriteFile(target, []byte(old), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(target, 0644); err != nil {
						t.Fatal(err)
					}
					var err error
					switch layout {
					case "existing":
						err = os.Rename(target, path)
					case "symlink":
						err = os.Symlink(target, path)
					case "hardlink":
						err = os.Link(target, path)
					}
					if err != nil {
						t.Fatal(err)
					}
					reader, err := os.Open(path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						defer reader.Close()
						buf := make([]byte, len(old)+1)
						n, _ := reader.ReadAt(buf, 0)
						if string(buf[:n]) != old {
							t.Error("pre-existing reader received new private material")
						}
					})
				}
				socket := filepath.Join(podDir, proxy.SocketName)
				listener, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				socketInfo, err := os.Stat(socket)
				if err != nil {
					t.Fatal(err)
				}
				s := newAPFTestService(dir, tls)
				response, err := s.CreateVM(context.Background(), apfTestRequest())
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
					t.Fatal("APF configuration is not a private regular file")
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				sb, err := s.getSandbox("private-apf")
				if err != nil {
					t.Fatal(err)
				}
				if len(sb.cloudConfig.WriteFiles) != 1 || !bytes.Equal(data, []byte(sb.cloudConfig.WriteFiles[0].Content)) {
					t.Fatal("guest payload differs from APF configuration")
				}
				var config forwarder.Config
				if err := json.Unmarshal(data, &config); err != nil {
					t.Fatal("APF JSON is invalid")
				}
				if tls && (config.TLSServerKey != "synthetic-private-key" || config.MinTLSVersion != "VersionTLS12" || len(config.CipherSuites) != 1) {
					t.Fatal("TLS material or profile changed")
				}
				if !tls && (config.TLSServerKey != "" || config.MinTLSVersion != "") {
					t.Fatal("TLS-off configuration changed")
				}
				if layout == "symlink" || layout == "hardlink" {
					unchanged, err := os.ReadFile(target)
					if err != nil || string(unchanged) != old {
						t.Fatal("pre-existing link target was overwritten")
					}
				}
				podInfo, err := os.Stat(podDir)
				if err != nil || podInfo.Mode().Perm() != 0755 {
					t.Fatal("pod directory permissions changed")
				}
				after, err := os.Stat(socket)
				if err != nil || !os.SameFile(socketInfo, after) || socketInfo.Mode() != after.Mode() || response.AgentSocketPath != socket {
					t.Fatal("agent socket identity or permissions changed")
				}
				connection, err := net.DialTimeout("unix", socket, time.Second)
				if err != nil {
					t.Fatal("agent socket is no longer accessible")
				}
				connection.Close()
				entries, err := os.ReadDir(podDir)
				if err != nil || len(entries) != 2 {
					t.Fatal("unexpected temporary artifact")
				}
			})
		}
	}
}

func TestWriteAPFConfigFailures(t *testing.T) {
	t.Run("temporary-file-creation", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "not-a-directory")
		if err := os.WriteFile(blocker, []byte("synthetic-canary"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writeAPFConfig(filepath.Join(blocker, "apf.json"), []byte("synthetic-config")); err == nil {
			t.Fatal("temporary-file creation failure was ignored")
		}
		data, err := os.ReadFile(blocker)
		if err != nil || string(data) != "synthetic-canary" {
			t.Fatal("existing file changed after failure")
		}
	})
	t.Run("rename-preserves-existing-directory", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "private-apf", "apf.json")
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(path, "canary")
		if err := os.WriteFile(marker, []byte("synthetic-canary"), 0600); err != nil {
			t.Fatal(err)
		}
		s := newAPFTestService(dir, true)
		if _, err := s.CreateVM(context.Background(), apfTestRequest()); err == nil {
			t.Fatal("rename failure was ignored")
		}
		if _, err := s.getSandbox("private-apf"); err == nil {
			t.Fatal("failed CreateVM registered a sandbox")
		}
		data, err := os.ReadFile(marker)
		if err != nil || string(data) != "synthetic-canary" {
			t.Fatal("existing destination changed after failure")
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil || len(entries) != 1 || entries[0].Name() != "apf.json" {
			t.Fatal("temporary configuration remained after rename failure")
		}
	})
}
