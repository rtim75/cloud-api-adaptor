// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package interceptor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/util"
	pb "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols/grpc"
)

// Run only in a private user+mount namespace, never against a host /run:
// unshare -Urnm <test-binary> -test.run '^TestLocalCloudVolumeReadonlyBind$'
// The block-device mount is replaced with a bind fixture; production APF rewrites
// the request. Linux then enforces its resulting OCI ro/rw bind options. This is
// local enforcement evidence, not a real Kata-agent/cloud Pod acceptance test.
func TestLocalCloudVolumeReadonlyBind(t *testing.T) {
	if os.Getenv("CAA_PRIVATE_MOUNT_DIAGNOSTIC") != "1" {
		t.Skip("requires explicit private namespace runner")
	}
	uidMap, err := os.ReadFile("/proc/self/uid_map")
	if err != nil || len(strings.Fields(string(uidMap))) != 3 || strings.Fields(string(uidMap))[0] != "0" || strings.Fields(string(uidMap))[2] != "1" {
		t.Fatal("private user namespace required")
	}
	if err := syscall.Mount("tmpfs", "/run", "tmpfs", 0, "size=16m"); err != nil {
		t.Fatal(err)
	}
	defer syscall.Unmount("/run", syscall.MNT_DETACH)
	if err := syscall.Mount("tmpfs", "/dev", "tmpfs", 0, "size=1m"); err != nil {
		t.Fatal(err)
	}
	defer syscall.Unmount("/dev", syscall.MNT_DETACH)
	// os/exec opens /dev/null for nil stdin; a private empty file suffices here.
	if err := os.WriteFile("/dev/null", nil, 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("/dev/disk", 0700); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "target")
	bin := filepath.Join(dir, "bin")
	for _, p := range []string{source, target, bin} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mount("tmpfs", source, "tmpfs", 0, "size=1m"); err != nil {
		t.Fatal(err)
	}
	defer syscall.Unmount(source, syscall.MNT_DETACH)
	script := "#!/bin/sh\n[ \"$1\" = -t ] && [ \"$2\" = ext4 ] || exit 1\nexec /bin/mount --bind \"$READONLY_TEST_SOURCE\" \"$4\"\n"
	if err := os.WriteFile(filepath.Join(bin, "mount"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("READONLY_TEST_SOURCE", source)
	device := "/dev/disk/fixture-device"
	if err := os.WriteFile(device, []byte("not-a-cloud-block-device"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"ro", "rw"} {
		t.Run(mode, func(t *testing.T) {
			mock := &mockRedirector{}
			i := &interceptor{Redirector: mock, findDataDisk: func(int, string) (string, error) { return device, nil }}
			req := &pb.CreateContainerRequest{OCI: &pb.Spec{Linux: &pb.Linux{}, Annotations: map[string]string{util.CloudVolumesAnnotationKey: `{"vol-0":{"mount_point":"/data","fs_type":"ext4","lun":"0","disk_id":"projects/test/zones/us-east1-b/disks/data"}}`}, Mounts: []*pb.Mount{{Source: "/host/publication", Destination: "/data", Type: "bind", Options: []string{"rbind", mode}}}}}
			_, err := i.CreateContainer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !mock.createContainerCalled {
				t.Fatal("request never reached agent boundary")
			}
			m := req.OCI.Mounts[0]
			if m.Source != "/run/cloud-volumes/vol-0" || m.Type != "bind" || len(m.Options) != 2 || m.Options[1] != mode {
				t.Fatalf("unexpected rewritten mount %+v", m)
			}
			if err := syscall.Mount(m.Source, target, "", syscall.MS_BIND, ""); err != nil {
				t.Fatal(err)
			}
			defer syscall.Unmount(target, syscall.MNT_DETACH)
			if mode == "ro" {
				if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
					t.Fatal(err)
				}
			}
			err = os.WriteFile(filepath.Join(target, "write-"+mode), []byte("test"), 0600)
			if mode == "ro" && !errors.Is(err, syscall.EROFS) {
				t.Fatalf("readonly write: %v, want EROFS", err)
			}
			if mode == "rw" && err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "sibling-"+mode), []byte("writer remains available"), 0600); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s bind: write=%v, sibling source remains writable", mode, err)
		})
	}
}
