// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"fmt"
	"sync"

	pb "github.com/kata-containers/kata-containers/src/runtime/protocols/hypervisor"
)

// terminalInstanceDeletion marks providers that wait for deletion to finish.
type terminalInstanceDeletion interface {
	InstanceDeletionWaitsForCompletion() bool
}

type podOwner struct{ namespace, name string }

type instanceLifecycle struct {
	operation sync.Mutex
	owner     podOwner
	sandboxID sandboxID
	deleted   bool
}

func (s *cloudService) tracksInstanceLifecycle() bool {
	p, ok := s.provider.(terminalInstanceDeletion)
	return ok && p.InstanceDeletionWaitsForCompletion()
}

// Claim before calling the provider. Ownership survives a caller's RPC deadline
// and is released only after the provider confirms cleanup (or no create ran).
func (s *cloudService) beginInstanceLifecycle(sid sandboxID, sb *sandbox) (*instanceLifecycle, error) {
	if !s.tracksInstanceLifecycle() {
		return nil, nil
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if sb.lifecycle != nil {
		return nil, fmt.Errorf("sandbox %s has already started", sid)
	}
	owner := podOwner{namespace: sb.podNamespace, name: sb.podName}
	if s.instanceOwners == nil {
		s.instanceOwners = make(map[podOwner]*instanceLifecycle)
	}
	if old := s.instanceOwners[owner]; old != nil {
		return nil, fmt.Errorf("pod %s/%s still owns sandbox %s", owner.namespace, owner.name, old.sandboxID)
	}
	lease := &instanceLifecycle{owner: owner, sandboxID: sid}
	lease.operation.Lock()
	sb.lifecycle = lease
	s.instanceOwners[owner] = lease
	return lease, nil
}

// Caller holds lease.operation. Pointer equality prevents a delayed old StopVM
// from releasing ownership of an already running replacement.
func (s *cloudService) completeInstanceLifecycle(lease *instanceLifecycle) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	lease.deleted = true
	if s.instanceOwners[lease.owner] == lease {
		delete(s.instanceOwners, lease.owner)
	}
}

func (s *cloudService) stopVMWithLifecycle(ctx context.Context, sid sandboxID, sb *sandbox) (*pb.StopVMResponse, error) {
	s.mutex.Lock()
	lease := sb.lifecycle
	if lease == nil {
		// Exclude a racing StartVM without claiming another sandbox's owner.
		// This sandbox never called CreateInstance and has nothing to delete.
		lease = &instanceLifecycle{deleted: true}
		lease.operation.Lock()
		sb.lifecycle = lease
	} else if !lease.operation.TryLock() {
		s.mutex.Unlock()
		return nil, fmt.Errorf("sandbox %s lifecycle operation is in progress", sid)
	}
	s.mutex.Unlock()
	defer lease.operation.Unlock()
	if err := sb.agentProxy.Shutdown(); err != nil {
		logger.Printf("stopping agent proxy: %v", err)
	}
	if !lease.deleted {
		if sb.instanceID != "" {
			if err := s.provider.DeleteInstance(ctx, sb.instanceID); err != nil {
				return nil, fmt.Errorf("deleting instance %s: %w", sb.instanceID, err)
			}
			if s.ppService != nil {
				if err := s.ppService.ReleasePeerPod(sb.podName, sb.podNamespace, sb.instanceID); err != nil {
					logger.Printf("failed to release PeerPod: %v", err)
				}
			}
		}
		s.completeInstanceLifecycle(lease)
	}
	if err := s.workerNode.Teardown(sb.netNSPath, sb.podNetwork); err != nil {
		logger.Printf("tearing down netns %s: %v", sb.netNSPath, err)
	}
	if err := s.removeSandbox(sid); err != nil {
		return nil, err
	}
	return &pb.StopVMResponse{}, nil
}
