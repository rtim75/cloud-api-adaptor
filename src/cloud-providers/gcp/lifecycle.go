// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	compute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
)

// InstanceDeletionWaitsForCompletion enables owner fencing until deletion completes.
func (*gcpProvider) InstanceDeletionWaitsForCompletion() bool { return true }

type instanceInsert struct {
	mutex     sync.Mutex
	operation *compute.Operation
}

func (p *gcpProvider) waitForTrackedInstanceInsert(ctx context.Context, op *compute.Operation, req *computepb.InsertInstanceRequest) error {
	name := req.GetInstanceResource().GetName()
	record, ok := p.pendingInserts.Load(name)
	if !ok {
		return waitForInstanceInsert(ctx, op, req)
	}
	pending := record.(*instanceInsert)
	pending.mutex.Lock()
	defer pending.mutex.Unlock()
	err := waitForInstanceInsert(ctx, op, req)
	if op.Done() {
		p.pendingInserts.CompareAndDelete(name, pending)
	}
	return err
}

// A pending Insert can complete after an instance 404. If its operation ID is
// unknown, retain ownership until the operator reconciles the request.
func (p *gcpProvider) finishPendingInsert(ctx context.Context, name string) error {
	record, ok := p.pendingInserts.Load(name)
	if !ok {
		return nil
	}
	pending := record.(*instanceInsert)
	pending.mutex.Lock()
	defer pending.mutex.Unlock()
	if pending.operation == nil {
		return fmt.Errorf("instance %s has an unknown insert outcome; reconciliation is required", name)
	}
	err := pending.operation.Wait(ctx)
	if !pending.operation.Done() {
		return fmt.Errorf("waiting for insert of %s before deletion: %w", name, err)
	}
	p.pendingInserts.CompareAndDelete(name, pending)
	// A terminal failed insert may still have partial resources. Delete/404 below
	// establishes absence instead of treating the insert error as cleanup proof.
	return nil
}

func definitiveInsertRejection(err error) bool {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case 400, 401, 403, 404:
		return true
	}
	return false
}

func instanceMissing(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == 404
}
