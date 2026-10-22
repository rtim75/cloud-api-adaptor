// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"testing"

	provider "github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/util/cloudinit"
	pb "github.com/kata-containers/kata-containers/src/runtime/protocols/hypervisor"
	"github.com/stretchr/testify/require"
)

type restartDiagnosticProvider struct {
	*strictDiagnosticProvider
	active map[string]bool
}

func (p *restartDiagnosticProvider) CreateInstance(ctx context.Context, pod, sid string, cc cloudinit.CloudConfigGenerator, spec provider.InstanceTypeSpec) (*provider.Instance, error) {
	p.active[sid] = true
	return p.mockProvider.CreateInstance(ctx, pod, sid, cc, spec)
}

func (p *restartDiagnosticProvider) DeleteInstance(ctx context.Context, id string) error {
	// This fixture retains external resource state across service restart.
	for sid := range p.active {
		if id == "same-pod-"+sid {
			delete(p.active, sid)
		}
	}
	return nil
}

// Owners are process-local; a restarted service can admit a second instance.
func TestDiagnosticOwnerFenceDoesNotSurviveRestart(t *testing.T) {
	p := &restartDiagnosticProvider{strictDiagnosticProvider: &strictDiagnosticProvider{&diagnosticProvider{}}, active: map[string]bool{}}
	before := diagnosticService(t, p)
	old := before.sandboxes["old"]
	old.agentProxy = &diagnosticProxy{mockProxy: mockProxy{readyCh: make(chan struct{}), stopCh: make(chan struct{})}, ready: true}
	_, err := before.StartVM(context.Background(), &pb.StartVMRequest{Id: "old"})
	require.NoError(t, err)
	defer old.agentProxy.Shutdown()
	_, err = before.StartVM(context.Background(), &pb.StartVMRequest{Id: "replacement"})
	require.Error(t, err, "same-process replacement must remain fenced")
	// A process crash drops the service, but not its external VM. Recreate
	// only process state, then deliver the replacement RPC.
	after := diagnosticService(t, p)
	_, err = after.StartVM(context.Background(), &pb.StartVMRequest{Id: "replacement"})
	require.NoError(t, err)
	defer after.sandboxes["replacement"].agentProxy.Shutdown()
	require.True(t, p.active["old"], "old cloud instance must still exist")
	require.True(t, p.active["replacement"], "fresh service admitted a replacement while old instance exists")
	t.Log("unsupported boundary reproduced: two same-pod instances can coexist after owner state is lost")
}
