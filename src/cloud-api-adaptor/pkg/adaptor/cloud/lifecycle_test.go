// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	provider "github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/util/cloudinit"
	cri "github.com/containerd/containerd/pkg/cri/annotations"
	"github.com/containerd/ttrpc"
	pb "github.com/kata-containers/kata-containers/src/runtime/protocols/hypervisor"
	"github.com/stretchr/testify/require"
)

type diagnosticProvider struct {
	mockProvider
	deleteStarted      chan struct{}
	deleteRelease      chan struct{}
	deleteDone         chan struct{}
	replacementCreated chan struct{}
	deleteErr          error
	once               sync.Once
}

func (p *diagnosticProvider) CreateInstance(ctx context.Context, pod, sid string, cc cloudinit.CloudConfigGenerator, spec provider.InstanceTypeSpec) (*provider.Instance, error) {
	if sid == "replacement" {
		close(p.replacementCreated)
	}
	return p.mockProvider.CreateInstance(ctx, pod, sid, cc, spec)
}
func (p *diagnosticProvider) DeleteInstance(ctx context.Context, id string) error {
	if p.deleteStarted != nil && id == "same-pod-old" {
		p.once.Do(func() { close(p.deleteStarted) })
		select {
		case <-p.deleteRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
		close(p.deleteDone)
	}
	return p.deleteErr
}

// The first sandbox never becomes ready; replacement is ready immediately.
type diagnosticProxy struct {
	mockProxy
	once  sync.Once
	ready bool
}

func (p *diagnosticProxy) Start(ctx context.Context, u *url.URL) error {
	if p.ready {
		close(p.readyCh)
	}
	<-p.stopCh
	return nil
}
func (p *diagnosticProxy) Shutdown() error { p.once.Do(func() { close(p.stopCh) }); return nil }
func awaitDiagnostic(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: " + label)
	}
}
func diagnosticService(t *testing.T, p provider.Provider) *cloudService {
	t.Helper()
	// No Kubernetes client, cloud credential, tunnel or filesystem setup.
	s := &cloudService{provider: p, workerNode: &mockWorkerNode{}, serverConfig: &ServerConfig{ForwarderPort: "15150"}, sandboxes: map[sandboxID]*sandbox{}}
	s.cond = sync.NewCond(&s.mutex)
	for _, id := range []string{"old", "replacement"} {
		px := &diagnosticProxy{mockProxy: mockProxy{readyCh: make(chan struct{}), stopCh: make(chan struct{})}, ready: id == "replacement"}
		if err := s.addSandbox(sandboxID(id), &sandbox{podName: "same-pod", podNamespace: "same-namespace", agentProxy: px}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

type strictDiagnosticProvider struct{ *diagnosticProvider }

func (*strictDiagnosticProvider) InstanceDeletionWaitsForCompletion() bool { return true }

type observedService struct {
	Service
	oldDone chan struct{}
}

func (s *observedService) StartVM(ctx context.Context, req *pb.StartVMRequest) (*pb.StartVMResponse, error) {
	if req.Id == "old" {
		defer close(s.oldDone)
	}
	return s.Service.StartVM(ctx, req)
}
func TestGCPLifecycleRPCRetryDuringCleanup(t *testing.T) {
	p := &diagnosticProvider{deleteStarted: make(chan struct{}), deleteRelease: make(chan struct{}), deleteDone: make(chan struct{}), replacementCreated: make(chan struct{})}
	s := diagnosticService(t, &strictDiagnosticProvider{p})
	oldDone := make(chan struct{})
	srv, err := ttrpc.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	pb.RegisterHypervisorService(srv, &observedService{Service: s, oldDone: oldDone})
	listener, err := net.Listen("unix", filepath.Join(t.TempDir(), "hv.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); _ = srv.Serve(context.Background(), listener) }()
	defer func() { _ = srv.Close(); awaitDiagnostic(t, serveDone, "server exit") }()
	dial := func() (*ttrpc.Client, pb.HypervisorService) {
		c, e := net.Dial("unix", listener.Addr().String())
		if e != nil {
			t.Fatal(e)
		}
		tt := ttrpc.NewClient(c)
		return tt, pb.NewHypervisorClient(tt)
	}
	tt, client := dial()
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_, err = client.StartVM(ctx, &pb.StartVMRequest{Id: "old"})
	if err == nil {
		t.Fatal("expected deadline")
	}
	_ = tt.Close() // actual remoteHypervisor closes its RPC connection on return
	awaitDiagnostic(t, p.deleteStarted, "old deferred cleanup started")
	release := sync.OnceFunc(func() { close(p.deleteRelease) })
	defer release()

	select {
	case <-p.deleteDone:
		t.Fatal("cleanup completed before release")
	default:
	}
	tt2, client2 := dial()
	defer tt2.Close()
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer retryCancel()
	_, err = client2.StartVM(retryCtx, &pb.StartVMRequest{Id: "replacement"})
	require.Error(t, err, "replacement must be fenced during cleanup")
	select {
	case <-p.replacementCreated:
		t.Fatal("replacement reached provider during cleanup")
	default:
	}
	unstarted := &sandbox{podName: "same-pod", podNamespace: "same-namespace", agentProxy: &diagnosticProxy{mockProxy: mockProxy{readyCh: make(chan struct{}), stopCh: make(chan struct{})}}}
	require.NoError(t, s.addSandbox("unstarted", unstarted))
	_, err = s.StopVM(retryCtx, &pb.StopVMRequest{Id: "unstarted"})
	require.NoError(t, err)
	require.NotEmpty(t, s.instanceOwners, "stopping an unstarted sandbox must not release the old owner")
	other := &sandbox{podName: "other-pod", podNamespace: "same-namespace", agentProxy: &diagnosticProxy{mockProxy: mockProxy{readyCh: make(chan struct{}), stopCh: make(chan struct{})}, ready: true}}
	require.NoError(t, s.addSandbox("other", other))
	_, err = s.StartVM(retryCtx, &pb.StartVMRequest{Id: "other"})
	require.NoError(t, err)
	_, err = s.StopVM(retryCtx, &pb.StopVMRequest{Id: "old"})
	require.Error(t, err, "concurrent cleanup must retain ownership")
	release()
	awaitDiagnostic(t, oldDone, "old handler cleanup completed")
	_, err = client2.StartVM(retryCtx, &pb.StartVMRequest{Id: "replacement"})
	require.NoError(t, err)
	awaitDiagnostic(t, p.replacementCreated, "replacement after cleanup")
	id, idErr := s.GetInstanceID(retryCtx, "same-namespace", "same-pod", false)
	require.NoError(t, idErr)
	require.Equal(t, "same-pod-replacem", id)

	_, err = s.StartVM(retryCtx, &pb.StartVMRequest{Id: "replacement"})
	require.Error(t, err, "duplicate StartVM must not create again")
	_, err = s.StopVM(retryCtx, &pb.StopVMRequest{Id: "old"})
	require.NoError(t, err)
	_, err = s.StartVM(retryCtx, &pb.StartVMRequest{Id: "old"})
	require.Error(t, err)
	_, err = s.StopVM(retryCtx, &pb.StopVMRequest{Id: "replacement"})
	require.NoError(t, err)
	_, err = s.StopVM(retryCtx, &pb.StopVMRequest{Id: "other"})
	require.NoError(t, err)
}

func TestLegacyLifecycleDeleteFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		label := "success-control"
		if fail {
			label = "delete-failure"
		}
		t.Run(label, func(t *testing.T) {
			p := &diagnosticProvider{}
			if fail {
				p.deleteErr = errors.New("injected native delete incomplete")
			}
			s := diagnosticService(t, p)
			s.sandboxes["old"].instanceID = "old-native-instance"
			res, err := s.StopVM(context.Background(), &pb.StopVMRequest{Id: "old"})
			if err != nil || res == nil {
				t.Fatalf("unexpected StopVM response: %v", err)
			}
			if _, err = s.getSandbox("old"); err == nil {
				t.Fatal("expected current implementation removes sandbox")
			}
			t.Logf("CONFIRMED: delete failure=%t, StopVM returned success and removed sandbox", fail)
		})
	}
}

func TestGCPLifecycleDeleteFailureRetainsOwner(t *testing.T) {
	p := &diagnosticProvider{replacementCreated: make(chan struct{})}
	s := diagnosticService(t, &strictDiagnosticProvider{p})
	_, err := s.StartVM(context.Background(), &pb.StartVMRequest{Id: "replacement"})
	require.NoError(t, err)
	p.deleteErr = errors.New("injected delete failure")
	_, err = s.StopVM(context.Background(), &pb.StopVMRequest{Id: "replacement"})
	require.Error(t, err)
	sb, err := s.getSandbox("replacement")
	require.NoError(t, err)
	require.NotEmpty(t, sb.instanceID)
	_, err = s.StartVM(context.Background(), &pb.StartVMRequest{Id: "old"})
	require.Error(t, err)
	p.deleteErr = nil
	_, err = s.StopVM(context.Background(), &pb.StopVMRequest{Id: "replacement"})
	require.NoError(t, err)
	_, err = s.getSandbox("replacement")
	require.Error(t, err)
}

// The strict wrapper is absent for legacy providers; no per-owner lifecycle
// state is allocated and a second sandbox retains its existing behavior.
func TestLegacyLifecycleDoesNotFenceOwners(t *testing.T) {
	p := &diagnosticProvider{replacementCreated: make(chan struct{})}
	s := diagnosticService(t, p)
	s.sandboxes["old"].agentProxy = &diagnosticProxy{mockProxy: mockProxy{readyCh: make(chan struct{}), stopCh: make(chan struct{})}, ready: true}
	for _, id := range []string{"old", "replacement"} {
		_, err := s.StartVM(context.Background(), &pb.StartVMRequest{Id: id})
		require.NoError(t, err)
	}
	require.Nil(t, s.instanceOwners)
	for _, id := range []string{"old", "replacement"} {
		_, err := s.StopVM(context.Background(), &pb.StopVMRequest{Id: id})
		require.NoError(t, err)
	}
}

type partialDiagnosticProvider struct {
	*strictDiagnosticProvider
	partial bool
}

func (p *partialDiagnosticProvider) CreateInstance(ctx context.Context, pod, sid string, cc cloudinit.CloudConfigGenerator, spec provider.InstanceTypeSpec) (*provider.Instance, error) {
	if !p.partial {
		return nil, errors.New("rejected before insert")
	}
	return &provider.Instance{ID: "uncertain", Name: "uncertain"}, errors.New("insert wait cancelled")
}
func TestGCPLifecyclePartialCreateCleanupRetry(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-insert", true: "partial-insert"}[partial], func(t *testing.T) {
			base := &diagnosticProvider{deleteErr: errors.New("cleanup incomplete")}
			p := &partialDiagnosticProvider{strictDiagnosticProvider: &strictDiagnosticProvider{base}, partial: partial}
			s := diagnosticService(t, p)
			_, err := s.StartVM(context.Background(), &pb.StartVMRequest{Id: "old"})
			require.Error(t, err)
			if partial {
				require.Equal(t, "uncertain", s.sandboxes["old"].instanceID)
				_, err = s.StartVM(context.Background(), &pb.StartVMRequest{Id: "replacement"})
				require.ErrorContains(t, err, "still owns")
				base.deleteErr = nil
				_, err = s.StopVM(context.Background(), &pb.StopVMRequest{Id: "old"})
				require.NoError(t, err)
			}
			require.Empty(t, s.instanceOwners)
		})
	}
}

func TestGCPLifecycleStandaloneWithoutUID(t *testing.T) {
	p := &strictDiagnosticProvider{&diagnosticProvider{}}
	s := diagnosticService(t, p)
	s.proxyFactory = &mockProxyFactory{}
	s.serverConfig.PodsDir = t.TempDir()
	_, err := s.CreateVM(context.Background(), &pb.CreateVMRequest{Id: "standalone", Annotations: map[string]string{cri.SandboxName: "pod", cri.SandboxNamespace: "ns"}})
	require.NoError(t, err)
	_, err = s.StartVM(context.Background(), &pb.StartVMRequest{Id: "standalone"})
	require.NoError(t, err)
	_, err = s.StopVM(context.Background(), &pb.StopVMRequest{Id: "standalone"})
	require.NoError(t, err)
}
