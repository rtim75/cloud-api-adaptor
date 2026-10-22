package userdata

import (
	"context"
	"os"
	"strings"

	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-api-adaptor/pkg/paths"
	dmidecode "github.com/fenglyu/go-dmidecode"
	cpuid "github.com/klauspost/cpuid/v2"
)

func isAzureVM() bool {
	return cpuid.CPU.HypervisorVendorID == cpuid.MSVM
}

func isAWSVM(ctx context.Context) bool {
	t, err := dmidecode.NewDMITable()
	if err != nil {
		return false
	}

	provider := t.Query(dmidecode.KeywordSystemManufacturer)

	return provider == "Amazon EC2"
}

func isGCPVM(ctx context.Context) bool {
	return isGCPVMWithProductName(ctx, "/sys/class/dmi/id/product_name")
}

func isGCPVMWithProductName(ctx context.Context, productNamePath string) bool {
	// GCE identity does not depend on metadata being ready during cold boot.
	// These are the SMBIOS product names recognized by Google's metadata SDK.
	if product, err := os.ReadFile(productNamePath); err == nil {
		switch strings.TrimSpace(string(product)) {
		case "Google", "Google Compute Engine":
			return true
		}
	}

	// Retain metadata detection when SMBIOS is unavailable or unrecognized.
	if cpuid.CPU.HypervisorVendorID != cpuid.KVM {
		return false
	}
	_, err := imdsGet(ctx, GcpImdsURL, false, []kvPair{{"Metadata-Flavor", "Google"}})
	return err == nil
}

func hasUserDataFile() bool {
	_, err := os.Stat(paths.UserDataPath)
	if err != nil && os.IsNotExist(err) {
		return false
	}
	return true
}

func isAlibabaCloudVM() bool {
	t, err := dmidecode.NewDMITable()
	if err != nil {
		return false
	}

	provider := t.Query(dmidecode.KeywordSystemManufacturer)

	return provider == "Alibaba Cloud"
}
