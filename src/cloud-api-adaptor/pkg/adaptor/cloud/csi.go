// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"fmt"
	"maps"
	"os"

	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/util"
	cri "github.com/containerd/containerd/pkg/cri/annotations"
)

// podUIDResolver retrieves the Kubernetes identity used to select CSI publications.
type podUIDResolver interface {
	GetPodUID(context.Context, string, string) (string, error)
}

// resolveCSIPodAnnotations fills a missing pod UID before selecting CSI volumes
// from the node-wide publication directory, without mutating the input map.
// Requests without a UID need no lookup when the directory is absent or empty;
// otherwise lookup and directory errors abort attachment to preserve pod isolation.
func resolveCSIPodAnnotations(ctx context.Context, annotations map[string]string, resolver podUIDResolver) (map[string]string, error) {
	if annotations[cri.SandboxUID] != "" {
		return annotations, nil
	}
	// No publications means there is nothing to attach, including in standalone
	// deployments without a Kubernetes client. On scan errors, fail closed.
	entries, err := os.ReadDir(util.KataDirectVolumesDir)
	if os.IsNotExist(err) || (err == nil && len(entries) == 0) {
		return annotations, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect CSI publications: %w", err)
	}
	if resolver == nil {
		return nil, fmt.Errorf("pod UID resolver unavailable")
	}
	uid, err := resolver.GetPodUID(ctx, util.GetPodName(annotations), util.GetPodNamespace(annotations))
	if err != nil {
		return nil, err
	}
	if uid == "" {
		return nil, fmt.Errorf("pod UID is empty")
	}
	resolved := maps.Clone(annotations)
	if resolved == nil {
		resolved = make(map[string]string)
	}
	resolved[cri.SandboxUID] = uid
	return resolved, nil
}
