// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"testing"

	pb "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols/grpc"
	"github.com/stretchr/testify/require"
)

func TestCloudVolumesPreserveReadonlyOCI(t *testing.T) {
	for _, options := range [][]string{{"rbind", "ro"}, {"rbind", "rw"}} {
		t.Run(options[1], func(t *testing.T) {
			dir := t.TempDir()
			overrideKataDirectVolumesDir(t, dir)
			service, cleanup := setupMockAgentAndService(t)
			defer cleanup()
			path := "/var/lib/kubelet/pods/readonly-pod/volumes/kubernetes.io~csi/pvc-data/mount"
			writeTestMountInfo(t, dir, path, map[string]interface{}{"device": "projects/test/zones/us-east1-b/disks/data", "fstype": "ext4"})
			req := newCreateContainerRequest("readonly-container").withAnnotations(map[string]string{"io.kubernetes.cri.sandbox-uid": "readonly-pod"}).withMounts(&pb.Mount{Source: path, Destination: "/data", Type: "bind", Options: options}).build()
			_, err := service.CreateContainer(context.Background(), req)
			require.NoError(t, err)
			require.Equal(t, options, req.OCI.Mounts[0].Options)
			require.Contains(t, req.OCI.Annotations, "io.confidentialcontainers.org.cloud_volumes")
		})
	}
}

func TestCloudVolumesCSIReadonlyOverridesOCIRW(t *testing.T) {
	dir := t.TempDir()
	overrideKataDirectVolumesDir(t, dir)
	service, cleanup := setupMockAgentAndService(t)
	defer cleanup()
	path := "/var/lib/kubelet/pods/readonly-pod/volumes/kubernetes.io~csi/pvc-data/mount"
	writeTestMountInfo(t, dir, path, map[string]interface{}{"device": "cloud-disk", "fstype": "ext4", "options": []string{"ro"}})
	req := newCreateContainerRequest("readonly-container").withAnnotations(map[string]string{"io.kubernetes.cri.sandbox-uid": "readonly-pod"}).withMounts(&pb.Mount{Source: path, Destination: "/data", Type: "bind", Options: []string{"rbind", "rw", "nosuid"}}, &pb.Mount{Source: "/unrelated", Destination: "/other", Type: "bind", Options: []string{"rw"}}).build()
	_, err := service.CreateContainer(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, []string{"rbind", "nosuid", "ro"}, req.OCI.Mounts[0].Options)
	require.Equal(t, []string{"rw"}, req.OCI.Mounts[1].Options)
}
