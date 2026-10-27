// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package interceptor

import (
	"strings"
	"testing"
)

func TestNonGCPDiskIdentityPreservesPlatformDiscovery(t *testing.T) {
	t.Setenv("CAA_DISK_DETECT_MAX_ATTEMPTS", "1")
	for _, id := range []string{
		"/subscriptions/test-sub/resourceGroups/control/providers/Microsoft.Compute/disks/data",
		"/subscriptions/test-sub/resourceGroups/projects/providers/Microsoft.Compute/disks/data",
		"/subscriptions/test-sub/resourceGroups/lab-projects/providers/Microsoft.Compute/disks/data",
		"/SUBSCRIPTIONS/test-sub/RESOURCEGROUPS/projects/PROVIDERS/microsoft.compute/DISKS/data",
		"vol-0123456789abcdef0",
	} {
		t.Run(id, func(t *testing.T) {
			// An absent LUN avoids selecting a host device. Platform lookup may
			// fail on this host, but must not interpret an Azure/EBS ID as GCP.
			_, err := findDataDiskDeviceWithGCP(9999, id, func(string) (string, error) {
				t.Fatal("non-GCP identity reached GCP device lookup")
				return "", nil
			})
			if err != nil && strings.Contains(err.Error(), "GCP") {
				t.Fatalf("non-GCP identity rejected as GCP: %v", err)
			}
		})
	}
}

func TestAzureDiskIdentityDoesNotBypassMalformedGCPValidation(t *testing.T) {
	for _, id := range []string{
		"/subscriptions//resourceGroups/projects/providers/Microsoft.Compute/disks/data",
		"/subscriptions/test-sub/resourceGroups/projects/providers/Microsoft.Storage/disks/data",
		"/subscriptions/test-sub/resourceGroups/projects/providers/Microsoft.Compute/disks/",
		"/subscriptions/test-sub/resourceGroups/projects/providers/Microsoft.Compute/disks/data/extra",
	} {
		t.Run(id, func(t *testing.T) {
			_, err := findDataDiskDeviceWithGCP(9999, id, func(string) (string, error) {
				t.Fatal("malformed identity reached GCP device lookup")
				return "", nil
			})
			if err == nil || !strings.Contains(err.Error(), "invalid GCP disk path") {
				t.Fatalf("malformed GCP-looking identity bypassed validation: %v", err)
			}
		})
	}
}
