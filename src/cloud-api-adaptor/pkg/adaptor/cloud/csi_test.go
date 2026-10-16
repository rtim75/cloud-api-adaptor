// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/util"
	cri "github.com/containerd/containerd/pkg/cri/annotations"
	"github.com/stretchr/testify/require"
)

type testPodUIDResolver func(context.Context, string, string) (string, error)

// GetPodUID delegates identity lookup to the test callback.
func (f testPodUIDResolver) GetPodUID(ctx context.Context, name, namespace string) (string, error) {
	return f(ctx, name, namespace)
}

// TestResolveCSIPodAnnotations checks UID resolution, input preservation and lookup failures.
func TestResolveCSIPodAnnotations(t *testing.T) {
	dir := t.TempDir()
	old := util.KataDirectVolumesDir
	util.KataDirectVolumesDir = dir
	t.Cleanup(func() { util.KataDirectVolumesDir = old })
	require.NoError(t, os.Mkdir(filepath.Join(dir, "publication"), 0700))
	annotations := map[string]string{cri.SandboxName: "contender", cri.SandboxNamespace: "test"}
	got, err := resolveCSIPodAnnotations(context.Background(), annotations, testPodUIDResolver(func(ctx context.Context, name, namespace string) (string, error) {
		require.Equal(t, "contender", name)
		require.Equal(t, "test", namespace)
		return "contender-uid", nil
	}))
	require.NoError(t, err)
	require.Equal(t, "contender-uid", got[cri.SandboxUID])
	require.Empty(t, annotations[cri.SandboxUID], "do not mutate the caller's annotations")
	got, err = resolveCSIPodAnnotations(context.Background(), got, nil)
	require.NoError(t, err)
	require.Equal(t, "contender-uid", got[cri.SandboxUID])
	for _, lookup := range []testPodUIDResolver{
		func(context.Context, string, string) (string, error) { return "", errors.New("forbidden") },
		func(context.Context, string, string) (string, error) { return "", nil },
	} {
		got, err = resolveCSIPodAnnotations(context.Background(), annotations, lookup)
		require.Error(t, err)
		require.Nil(t, got)
	}
}

// TestResolvedPodUIDIsolatesCSIVolumes prevents another pod's publication from entering attachment requests.
func TestResolvedPodUIDIsolatesCSIVolumes(t *testing.T) {
	dir := t.TempDir()
	old := util.KataDirectVolumesDir
	util.KataDirectVolumesDir = dir
	t.Cleanup(func() { util.KataDirectVolumesDir = old })
	for _, uid := range []string{"owner-uid", "contender-uid"} {
		path := "/var/lib/kubelet/pods/" + uid + "/volumes/kubernetes.io~csi/pvc-data/mount"
		entry := filepath.Join(dir, base64.URLEncoding.EncodeToString([]byte(path)))
		require.NoError(t, os.MkdirAll(entry, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(entry, "mountInfo.json"), []byte(`{"device":"same-disk"}`), 0600))
	}
	annotations, err := resolveCSIPodAnnotations(context.Background(), map[string]string{cri.SandboxName: "contender", cri.SandboxNamespace: "test"}, testPodUIDResolver(func(context.Context, string, string) (string, error) { return "contender-uid", nil }))
	require.NoError(t, err)
	volumes := util.GetCSIVolumesForPod(annotations)
	require.Len(t, volumes, 1, "the owner publication must not be attached to the contender")
	require.Equal(t, "same-disk", volumes[0].DiskID)
	annotations[cri.SandboxUID] = "pod-without-volumes"
	require.Empty(t, util.GetCSIVolumesForPod(annotations))
}

// TestNoCSIPublicationsNeedsNoPodUID permits an absent or empty publication directory
// and rejects scan errors or unresolved ownership when publications exist.
func TestNoCSIPublicationsNeedsNoPodUID(t *testing.T) {
	old := util.KataDirectVolumesDir
	t.Cleanup(func() { util.KataDirectVolumesDir = old })
	dir := t.TempDir()
	for _, path := range []string{dir, filepath.Join(dir, "absent")} {
		util.KataDirectVolumesDir = path
		got, err := resolveCSIPodAnnotations(context.Background(), nil, nil)
		require.NoError(t, err)
		require.Empty(t, util.GetCSIVolumesForPod(got))
	}
	util.KataDirectVolumesDir = filepath.Join(dir, "not-a-directory")
	require.NoError(t, os.WriteFile(util.KataDirectVolumesDir, nil, 0600))
	_, scanErr := resolveCSIPodAnnotations(context.Background(), nil, nil)
	require.Error(t, scanErr)
	util.KataDirectVolumesDir = dir
	require.NoError(t, os.Mkdir(filepath.Join(dir, "publication"), 0700))
	_, err := resolveCSIPodAnnotations(context.Background(), nil, nil)
	require.Error(t, err, "unknown ownership must fail closed when publications exist")
}
