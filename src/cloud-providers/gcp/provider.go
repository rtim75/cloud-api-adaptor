// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	crm "cloud.google.com/go/resourcemanager/apiv3"
	resourcemanagerpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	provider "github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/util"
	"github.com/confidential-containers/cloud-api-adaptor/src/cloud-providers/util/cloudinit"
	"google.golang.org/api/option"
	proto "google.golang.org/protobuf/proto"
)

var logger = log.New(log.Writer(), "[adaptor/cloud/gcp] ", log.LstdFlags|log.Lmsgprefix)
var computeScope = "https://www.googleapis.com/auth/compute"

const maxInstanceNameLen = 63

type gcpProvider struct {
	serviceConfig   *Config
	instancesClient *compute.InstancesClient
	clientOptions   []option.ClientOption
	pendingInserts  sync.Map
}

func (p *gcpProvider) ConfigVerifier() error {
	return nil
}

// authOptions returns the client options needed for GCP API clients to use
// the explicit credentials from GCP_CREDENTIALS, if any were configured.
// Without this, clients silently fall back to Application Default
// Credentials (the node's own service account), which can fail with
// "insufficient authentication scopes" when that account lacks compute
// scope.
func (p *gcpProvider) authOptions() []option.ClientOption {
	return p.clientOptions
}

func NewProvider(config *Config) (provider.Provider, error) {
	logger.Printf("gcp config: %#v", config.Redact())
	provider := &gcpProvider{
		serviceConfig:   config,
		instancesClient: nil,
	}
	if config.GcpCredentials != "" {
		creds, err := loadCredentials(config.GcpCredentials)
		if err != nil {
			return nil, fmt.Errorf("configuration error when using creds: %s", err)
		}
		provider.clientOptions = []option.ClientOption{option.WithAuthCredentials(creds)}
		provider.instancesClient, err = compute.NewInstancesRESTClient(context.TODO(), provider.clientOptions...)
		if err != nil {
			return nil, fmt.Errorf("NewInstancesRESTClient with credentials error: %s", err)
		}
	} else {
		var err error
		provider.instancesClient, err = compute.NewInstancesRESTClient(context.TODO())
		if err != nil {
			return nil, fmt.Errorf("NewInstancesRESTClient error: %s", err)
		}
	}

	if err := provider.updateInstanceSizeSpecList(); err != nil {
		return nil, err
	}

	return provider, nil
}

func parseIPString(ipStr string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to parse pod node IP %q: %w", ipStr, err)
	}

	return ip, nil
}

func getNatIPs(nic *computepb.NetworkInterface) ([]netip.Addr, error) {
	var natIPs []netip.Addr

	for _, access := range nic.GetAccessConfigs() {
		ip, err := parseIPString(access.GetNatIP())
		if err != nil {
			return nil, err
		}

		natIPs = append(natIPs, ip)
	}

	return natIPs, nil
}

func getIPs(intfcs []*computepb.NetworkInterface, usePublicIPs bool) ([]netip.Addr, error) {
	var podNodeIPs []netip.Addr

	for _, nic := range intfcs {
		var ips []netip.Addr

		if usePublicIPs {
			var err error

			ips, err = getNatIPs(nic)
			if err != nil {
				return nil, err
			}
		} else {
			ip, err := parseIPString(nic.GetNetworkIP())
			if err != nil {
				return nil, err
			}

			ips = []netip.Addr{ip}
		}

		podNodeIPs = append(podNodeIPs, ips...)
	}

	return podNodeIPs, nil
}

func (p *gcpProvider) ListAllTags(ctx context.Context) (map[string]map[string]*resourcemanagerpb.TagValue, error) {
	tagKeysClient, err := crm.NewTagKeysClient(ctx)
	if err != nil {
		return nil, err
	}
	defer tagKeysClient.Close()

	tagValuesClient, err := crm.NewTagValuesClient(ctx)
	if err != nil {
		return nil, err
	}
	defer tagValuesClient.Close()

	parent := fmt.Sprintf("projects/%s", p.serviceConfig.ProjectID)
	tags := make(map[string]map[string]*resourcemanagerpb.TagValue)

	it := tagKeysClient.ListTagKeys(ctx, &resourcemanagerpb.ListTagKeysRequest{Parent: parent})
	for {
		key, err := it.Next()
		if err != nil {
			break
		}
		tagKeyID := key.Name
		keyName := key.ShortName
		tags[keyName] = make(map[string]*resourcemanagerpb.TagValue)

		valIt := tagValuesClient.ListTagValues(ctx, &resourcemanagerpb.ListTagValuesRequest{Parent: tagKeyID})
		for {
			val, err := valIt.Next()
			if err != nil {
				break
			}
			tags[keyName][val.ShortName] = val
		}
	}
	return tags, nil
}

func (p *gcpProvider) getImageSizeGB(ctx context.Context, image string) (int64, error) {
	client, err := compute.NewImagesRESTClient(ctx, p.authOptions()...)
	if err != nil {
		return 0, fmt.Errorf("failed to create compute client: %w", err)
	}
	defer client.Close()

	var projectID string
	var imageName string

	// Parse project ID from full path if present
	// Supported formats:
	// - /projects/PROJECT-ID/global/images/IMAGE-NAME
	// - projects/PROJECT-ID/global/images/IMAGE-NAME
	// - https://www.googleapis.com/compute/v1/projects/PROJECT-ID/global/images/IMAGE-NAME
	if strings.HasPrefix(image, "/projects/") || strings.HasPrefix(image, "projects/") || strings.HasPrefix(image, "https://") {
		parts := strings.Split(image, "/")
		// Look for pattern: .../images/IMAGE-NAME
		for i := len(parts) - 2; i >= 0; i-- {
			if parts[i] == "images" && i >= 2 {
				projectID = parts[i-2]
				imageName = parts[len(parts)-1]
				break
			}
		}
	}

	// Fallback to ConfigMap project and image name
	if projectID == "" {
		projectID = p.serviceConfig.ProjectID
		parts := strings.Split(image, "/")
		imageName = parts[len(parts)-1]
	}

	req := &computepb.GetImageRequest{
		Project: projectID,
		Image:   imageName,
	}

	img, err := client.Get(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("Failed to get image for %s: %w", image, err)
	}

	return img.GetDiskSizeGb(), nil
}

// Select a machine type based on the memory, vcpu, and GPU requirements
func (p *gcpProvider) selectMachineType(ctx context.Context, spec provider.InstanceTypeSpec) (string, error) {
	return provider.SelectInstanceTypeToUse(spec, p.serviceConfig.MachineTypeSpecList, p.serviceConfig.MachineTypes, p.serviceConfig.MachineType)
}

func (p *gcpProvider) updateInstanceSizeSpecList() error {
	machineTypeClient, err := compute.NewMachineTypesRESTClient(context.TODO(), p.authOptions()...)
	if err != nil {
		return err
	}

	machineTypes := p.serviceConfig.MachineTypes

	if len(machineTypes) == 0 {
		machineTypes = append(machineTypes, p.serviceConfig.MachineType)
	}

	var instanceTypeSpecList []provider.InstanceTypeSpec

	for _, name := range machineTypes {
		machineType, err := machineTypeClient.Get(
			context.TODO(),
			&computepb.GetMachineTypeRequest{
				Project:     p.serviceConfig.ProjectID,
				Zone:        p.serviceConfig.Zone,
				MachineType: name,
			},
		)
		if err != nil {
			return fmt.Errorf("getting machine type(%s): %w", name, err)
		}

		instanceTypeSpecList = append(instanceTypeSpecList, provider.InstanceTypeSpec{
			InstanceType: name,
			VCPUs:        int64(*machineType.GuestCpus),
			Memory:       int64(*machineType.MemoryMb),
		})
	}
	p.serviceConfig.MachineTypeSpecList = instanceTypeSpecList
	logger.Printf("instanceSizeSpecList (%v)", p.serviceConfig.MachineTypeSpecList)

	return nil
}

func (p *gcpProvider) CreateInstance(ctx context.Context, podName, sandboxID string, cloudConfig cloudinit.CloudConfigGenerator, spec provider.InstanceTypeSpec) (instance *provider.Instance, err error) {

	dataDisks, err := existingDataDisks(p.serviceConfig.ProjectID, p.serviceConfig.Zone, spec.Volumes)
	if err != nil {
		return nil, fmt.Errorf("invalid CSI volumes: %w", err)
	}

	instanceName := util.GenerateInstanceName(podName, sandboxID, maxInstanceNameLen)
	logger.Printf("CreateInstance: name: %q", instanceName)

	userData, err := cloudConfig.Generate()
	if err != nil {
		return nil, err
	}

	// Check if the tags exist within the project
	// Otherwise, abort the instance creation
	allTags, err := p.ListAllTags(ctx)
	if err != nil {
		return nil, fmt.Errorf("Aborting: Failed to list tags: %w", err)
	}

	allTagValues := make([]*resourcemanagerpb.TagValue, 0)
	for tagKey, tagValue := range p.serviceConfig.Tags {
		tagID := allTags[tagKey][tagValue]
		if tagID == nil {
			msg := fmt.Sprintf("Aborting: Tag %s=%s not found", tagKey, tagValue)
			logger.Print(msg)
			return nil, fmt.Errorf("%s", msg)
		}
		allTagValues = append(allTagValues, tagID)
	}

	//Convert userData to base64
	userDataEnc := base64.StdEncoding.EncodeToString([]byte(userData))

	// It's expected that the image from the annotation will follow one of supported formats:
	// - "projects/<project>/global/images/<imageid>" and "/projects/<project>/global/images/<imageid>",
	// - url: "https://www.googleapis.com/compute/v1/projects/<project>/global/images/<imageid>",
	// - simple "<imageid>" if the image is present on the same project.
	var srcImage *string
	if hasAnyPrefix(p.serviceConfig.ImageName, "projects/", "/projects", "https") {
		srcImage = proto.String(p.serviceConfig.ImageName)
	} else {
		srcImage = proto.String(fmt.Sprintf("projects/%s/global/images/%s", p.serviceConfig.ProjectID, p.serviceConfig.ImageName))
	}

	if spec.Image != "" {
		logger.Printf("Choosing %s from annotation as the GCP image for the PodVM image", spec.Image)
		srcImage = proto.String(spec.Image)
	}

	// Select and validate machine type
	machineType, err := p.selectMachineType(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("failed to select machine type: %w", err)
	}

	imageSizeGB, err := p.getImageSizeGB(ctx, *srcImage)
	if err != nil {
		return nil, fmt.Errorf("Failed to get image size: %w", err)
	}

	// If user provided RootVolumeSize, use the larger of the two
	if p.serviceConfig.RootVolumeSize > 0 && int64(p.serviceConfig.RootVolumeSize) > imageSizeGB {
		imageSizeGB = int64(p.serviceConfig.RootVolumeSize)
	}

	// Format subnetwork: support both short names and full paths
	// GCP accepts formats:
	// - "projects/<project>/regions/<region>/subnetworks/<subnetwork>" (full path)
	// - "regions/<region>/subnetworks/<subnetwork>" (partial path)
	// - "<subnetwork>" (short name, will be formatted as full path)
	// Extract region from zone (e.g., "us-central1-a" -> "us-central1")
	var subnetworkValue *string
	if p.serviceConfig.Subnetwork != "" {
		subnetworkName := p.serviceConfig.Subnetwork
		if hasAnyPrefix(subnetworkName, "projects/", "/projects", "regions/", "https") {
			subnetworkValue = proto.String(subnetworkName)
		} else {
			// Extract region from zone (format: "region-zone" e.g., "us-central1-a")
			zoneParts := strings.Split(p.serviceConfig.Zone, "-")
			if len(zoneParts) >= 2 {
				region := strings.Join(zoneParts[:len(zoneParts)-1], "-")
				formattedSubnetwork := fmt.Sprintf("projects/%s/regions/%s/subnetworks/%s", p.serviceConfig.ProjectID, region, subnetworkName)
				subnetworkValue = proto.String(formattedSubnetwork)
			} else {
				// Fallback: assume zone format is invalid, try to use as-is
				subnetworkValue = proto.String(subnetworkName)
			}
		}
	}

	networkInterface := &computepb.NetworkInterface{
		Network:   proto.String(p.serviceConfig.Network),
		StackType: proto.String("IPV4_Only"),
	}
	if subnetworkValue != nil {
		networkInterface.Subnetwork = subnetworkValue
	}

	if p.serviceConfig.UsePublicIP {
		networkInterface.AccessConfigs = []*computepb.AccessConfig{
			{
				Name:        proto.String("External NAT"),
				NetworkTier: proto.String("STANDARD"),
			},
		}
	}

	instanceResource := &computepb.Instance{
		Name: proto.String(instanceName),
		Disks: []*computepb.AttachedDisk{
			{
				InitializeParams: &computepb.AttachedDiskInitializeParams{
					DiskSizeGb:  proto.Int64(imageSizeGB),
					SourceImage: srcImage,
					DiskType:    proto.String(fmt.Sprintf("zones/%s/diskTypes/%s", p.serviceConfig.Zone, p.serviceConfig.DiskType)),
				},
				AutoDelete: proto.Bool(true),
				Boot:       proto.Bool(true),
				Type:       proto.String(computepb.AttachedDisk_PERSISTENT.String()),
			},
		},
		Metadata: &computepb.Metadata{
			Items: []*computepb.Items{
				{
					Key:   proto.String("user-data"),
					Value: proto.String(userDataEnc),
				},
				{
					Key:   proto.String("user-data-encoding"),
					Value: proto.String("base64"),
				},
			},
		},
		MachineType:       proto.String(fmt.Sprintf("zones/%s/machineTypes/%s", p.serviceConfig.Zone, machineType)),
		NetworkInterfaces: []*computepb.NetworkInterface{networkInterface},
	}

	instanceResource.Disks = append(instanceResource.Disks, dataDisks...)

	// Check if OnHostMaintenance needs to be set to TERMINATE
	// This is required for:
	// 1. Confidential VMs
	// 2. GPU instances (when spec.GPUs > 0)
	requiresTerminatePolicy := false

	if !p.serviceConfig.DisableCVM {
		if p.serviceConfig.ConfidentialType == "" {
			return nil, fmt.Errorf("ConfidentialType must be set when using Confidential VM.")
		}

		instanceResource.ConfidentialInstanceConfig = &computepb.ConfidentialInstanceConfig{
			ConfidentialInstanceType:  proto.String(p.serviceConfig.ConfidentialType),
			EnableConfidentialCompute: proto.Bool(true),
		}
		requiresTerminatePolicy = true
	}

	// Check if GPUs are requested via annotation
	if spec.GPUs > 0 {
		logger.Printf("GPUs requested (%d), setting OnHostMaintenance to TERMINATE", spec.GPUs)
		requiresTerminatePolicy = true
	}

	if requiresTerminatePolicy {
		instanceResource.Scheduling = &computepb.Scheduling{
			OnHostMaintenance: proto.String("TERMINATE"),
		}
	}

	insertReq := &computepb.InsertInstanceRequest{
		Project:          p.serviceConfig.ProjectID,
		Zone:             p.serviceConfig.Zone,
		InstanceResource: instanceResource,
	}

	op, err := p.insertInstance(ctx, insertReq, spec.Volumes)
	if err != nil {
		if _, pending := p.pendingInserts.Load(instanceName); pending {
			return &provider.Instance{ID: instanceName, Name: instanceName}, fmt.Errorf("creating instance: %w", err)
		}
		return nil, fmt.Errorf("creating instance: %w", err)
	}
	if err := p.waitForTrackedInstanceInsert(ctx, op, insertReq); err != nil {
		return &provider.Instance{ID: instanceName, Name: instanceName}, err
	}
	logger.Printf("created an instance %s for sandbox %s", instanceName, sandboxID)

	// Create partial instance to return on error (allows caller to cleanup)
	instance = &provider.Instance{
		ID:   instanceName,
		Name: instanceName,
	}

	getReq := &computepb.GetInstanceRequest{
		Project:  p.serviceConfig.ProjectID,
		Zone:     p.serviceConfig.Zone,
		Instance: instanceName,
	}

	gcpInstance, err := p.instancesClient.Get(ctx, getReq)
	if err != nil {
		return instance, fmt.Errorf("unable to get instance: %w, req: %v", err, getReq)
	}
	logger.Printf("instance name %s, id %d", gcpInstance.GetName(), gcpInstance.GetId())

	// Binding all the tagValues to the instance that was already created
	// Specific endpoint is needed for tag bindings because global endpoint
	// doesn't work for zonal resources.
	tagBindingsOpts := append(p.authOptions(),
		option.WithEndpoint(fmt.Sprintf("%s-cloudresourcemanager.googleapis.com:443", p.serviceConfig.Zone)),
	)
	tagBindingsClient, err := crm.NewTagBindingsClient(ctx, tagBindingsOpts...)
	if err != nil {
		return instance, fmt.Errorf("failed to create bind client: %w", err)
	}
	defer tagBindingsClient.Close()

	parent := fmt.Sprintf("//compute.googleapis.com/projects/%s/zones/%s/instances/%d", p.serviceConfig.ProjectID, p.serviceConfig.Zone, gcpInstance.GetId())

	for _, tagValue := range allTagValues {
		logger.Printf("Creating tag binding for %s on %s", tagValue.Name, parent)

		tagBinding := &resourcemanagerpb.TagBinding{
			Parent:   parent,
			TagValue: tagValue.Name,
		}

		req := &resourcemanagerpb.CreateTagBindingRequest{
			TagBinding: tagBinding,
		}

		op, err := tagBindingsClient.CreateTagBinding(ctx, req)
		if err != nil {
			return instance, fmt.Errorf("API call to create tag binding failed for %s: %v", tagValue, err)
		}

		_, err = op.Wait(ctx)
		if err != nil {
			return instance, fmt.Errorf("Long-running operation for tag binding %s failed: %v", tagValue, err)
		}

		logger.Printf("Created tag binding for %s on %s successfully", tagValue, parent)
	}

	ips, err := getIPs(gcpInstance.GetNetworkInterfaces(), p.serviceConfig.UsePublicIP)
	if err != nil {
		logger.Printf("failed to get IPs for the instance: %v", err)
		return instance, err
	}

	logger.Printf("Found pod node IP(s): %v", ips)

	instance.IPs = ips

	return instance, nil
}

func (p *gcpProvider) DeleteInstance(ctx context.Context, instanceID string) error {
	if err := p.finishPendingInsert(ctx, instanceID); err != nil {
		return err
	}
	req := &computepb.DeleteInstanceRequest{
		Project:  p.serviceConfig.ProjectID,
		Zone:     p.serviceConfig.Zone,
		Instance: instanceID,
	}
	op, err := p.instancesClient.Delete(ctx, req)
	if err != nil {
		if instanceMissing(err) {
			return nil
		}
		return fmt.Errorf("Instances.Delete error: %w, req: %v", err, req)
	}
	err = op.Wait(ctx)
	if err != nil {
		return fmt.Errorf("waiting for Instances.Delete error: %s. req: %v", err, req)
	}
	logger.Printf("deleted an instance %s", instanceID)
	return nil
}

func (p *gcpProvider) Teardown() error {
	return nil
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// existingDataDisks maps CSI disk identities to existing zonal or regional attachments.
// API preflight and Instances.Insert additionally validate disk state and limits.
// DeviceName is the guest lookup identity; it must not depend on list order.
// Interface is omitted so Compute Engine selects a compatible interface for the VM.
func existingDataDisks(project, zone string, volumes []provider.CloudVolume) ([]*computepb.AttachedDisk, error) {
	var disks []*computepb.AttachedDisk
	seen := make(map[string]bool, len(volumes))
	devices := make(map[string]bool, len(volumes))
	for _, volume := range volumes {
		parts, parseErr := parseCSIDiskPath(volume.DiskID)
		if parseErr != nil {
			return nil, parseErr
		}
		location := zone
		if parts[2] == "regions" {
			i := strings.LastIndex(zone, "-")
			if i < 0 {
				return nil, fmt.Errorf("invalid PodVM zone %q", zone)
			}
			location = zone[:i]
		}
		if parts[1] != project || parts[3] != location {
			return nil, fmt.Errorf("disk %q must be in PodVM project %q and zone %q", volume.DiskID, project, zone)
		}
		if seen[volume.DiskID] || devices[parts[4]] {
			return nil, fmt.Errorf("duplicate disk %q", volume.DiskID)
		}
		seen[volume.DiskID] = true
		devices[parts[4]] = true
		disks = append(disks, &computepb.AttachedDisk{
			Source:     proto.String(volume.DiskID),
			DeviceName: proto.String(parts[4]),
			Boot:       proto.Bool(false),
			AutoDelete: proto.Bool(false),
			Type:       proto.String(computepb.AttachedDisk_PERSISTENT.String()),
			Mode:       proto.String(computepb.AttachedDisk_READ_WRITE.String()),
		})
	}
	return disks, nil
}

var csiDiskPath = regexp.MustCompile(`^projects/([a-z][a-z0-9-]{4,28}[a-z0-9])/(zones|regions)/([a-z][a-z0-9-]*[0-9](?:-[a-z])?)/disks/([a-z](?:[a-z0-9-]{0,61}[a-z0-9])?)$`)

func parseCSIDiskPath(path string) ([]string, error) {
	parts := csiDiskPath.FindStringSubmatch(path)
	if parts == nil {
		return nil, fmt.Errorf("invalid GCP disk path %q", path)
	}
	isZone := strings.LastIndex(parts[3], "-") == len(parts[3])-2
	if (parts[2] == "zones") != isZone {
		return nil, fmt.Errorf("invalid GCP disk location %q", path)
	}
	return parts, nil
}

func validateReplicaZones(disk *computepb.Disk, project, region, zone string) error {
	if disk == nil {
		return fmt.Errorf("GCP returned no regional disk")
	}
	seen := map[string]bool{}
	for _, replica := range disk.GetReplicaZones() {
		replica = strings.TrimPrefix(replica, "https://www.googleapis.com/compute/v1/")
		replica = strings.TrimPrefix(replica, "https://compute.googleapis.com/compute/v1/")
		parts, err := parseCSIDiskPath(replica + "/disks/replica")
		if err != nil || parts[1] != project || parts[2] != "zones" || parts[3][:strings.LastIndex(parts[3], "-")] != region || seen[parts[3]] {
			return fmt.Errorf("invalid regional replica zones")
		}
		seen[parts[3]] = true
	}
	if len(seen) != 2 || !seen[zone] {
		return fmt.Errorf("PodVM zone %q is not a disk replica zone", zone)
	}
	return nil
}

// Report only placement/operation identifiers: the request metadata contains
// cloud-init with private TLS material and must never be serialized into errors.
func waitForInstanceInsert(ctx context.Context, op *compute.Operation, req *computepb.InsertInstanceRequest) error {
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for Instances.Insert (project=%s zone=%s instance=%s operation=%s): %w",
			req.GetProject(), req.GetZone(), req.GetInstanceResource().GetName(), op.Name(), err)
	}
	return nil
}

// insertInstance keeps preflight immediately before the mutating request. A
// successful preflight cannot prevent races; Insert errors still reach callers.
func (p *gcpProvider) insertInstance(ctx context.Context, req *computepb.InsertInstanceRequest, volumes []provider.CloudVolume) (*compute.Operation, error) {
	if len(volumes) > 0 {
		resource := req.GetInstanceResource()
		if len(resource.GetDisks()) == 0 {
			return nil, fmt.Errorf("CSI preflight requires the boot disk request")
		}
		machine := resource.GetMachineType()
		machine = machine[strings.LastIndex(machine, "/")+1:]
		bootSize := resource.GetDisks()[0].GetInitializeParams().GetDiskSizeGb()
		if err := p.validateVolumes(ctx, machine, bootSize, resource.GetDisks()[0].GetInitializeParams().GetDiskType(), resource.GetConfidentialInstanceConfig(), volumes); err != nil {
			return nil, fmt.Errorf("CSI disk preflight: %w", err)
		}
	}
	name := req.GetInstanceResource().GetName()
	pending := &instanceInsert{}
	if _, exists := p.pendingInserts.LoadOrStore(name, pending); exists {
		return nil, fmt.Errorf("instance %s already has an unresolved insert", name)
	}
	pending.mutex.Lock()
	defer pending.mutex.Unlock()
	op, err := p.instancesClient.Insert(ctx, req)
	pending.operation = op
	if definitiveInsertRejection(err) {
		p.pendingInserts.CompareAndDelete(name, pending)
	}
	return op, err
}

// validateVolumes checks zonal/regional disk state and PD/Hyperdisk limits including boot.
// Instances.Insert validates remaining machine constraints and attachment races.
func (p *gcpProvider) validateVolumes(ctx context.Context, machine string, bootSize int64, bootType string, confidential *computepb.ConfidentialInstanceConfig, volumes []provider.CloudVolume) error {
	if len(volumes) == 0 {
		return nil
	}
	if _, err := existingDataDisks(p.serviceConfig.ProjectID, p.serviceConfig.Zone, volumes); err != nil {
		return err
	}
	if bootSize <= 0 {
		return fmt.Errorf("invalid boot disk size %d", bootSize)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	machineClient, err := compute.NewMachineTypesRESTClient(ctx, p.clientOptions...)
	if err != nil {
		return fmt.Errorf("creating machine types client: %w", err)
	}
	defer machineClient.Close()
	mt, err := machineClient.Get(ctx, &computepb.GetMachineTypeRequest{Project: p.serviceConfig.ProjectID, Zone: p.serviceConfig.Zone, MachineType: machine})
	if err != nil {
		return fmt.Errorf("reading machine type %q: %w", machine, err)
	}
	budget := diskBudget{machine: machine, mt: mt, confidential: confidential}
	if err := budget.add(bootType, bootSize, 0, 0, true); err != nil {
		return fmt.Errorf("boot disk exceeds or violates limits: %w", err)
	}
	disksClient, err := compute.NewDisksRESTClient(ctx, p.clientOptions...)
	if err != nil {
		return fmt.Errorf("creating disks client: %w", err)
	}
	defer disksClient.Close()
	var regionalClient *compute.RegionDisksClient
	defer func() {
		if regionalClient != nil {
			regionalClient.Close()
		}
	}()
	for _, vol := range volumes {
		parts, _ := parseCSIDiskPath(vol.DiskID)
		name := parts[4]
		regional := parts[2] == "regions"
		var disk *computepb.Disk
		if regional {
			if regionalClient == nil {
				regionalClient, err = compute.NewRegionDisksRESTClient(ctx, p.clientOptions...)
				if err != nil {
					return fmt.Errorf("creating regional disks client: %w", err)
				}
			}
			disk, err = regionalClient.Get(ctx, &computepb.GetRegionDiskRequest{Project: p.serviceConfig.ProjectID, Region: parts[3], Disk: name})
		} else {
			disk, err = disksClient.Get(ctx, &computepb.GetDiskRequest{Project: p.serviceConfig.ProjectID, Zone: p.serviceConfig.Zone, Disk: name})
		}
		if err != nil {
			return fmt.Errorf("reading disk %q: %w", vol.DiskID, err)
		}
		if disk == nil {
			return fmt.Errorf("GCP returned no disk %q", vol.DiskID)
		}
		actualPath := strings.TrimPrefix(strings.TrimPrefix(disk.GetSelfLink(), "https://www.googleapis.com/compute/v1/"), "https://compute.googleapis.com/compute/v1/")
		if (disk.GetName() != "" && disk.GetName() != name) || (actualPath != "" && actualPath != vol.DiskID) {
			return fmt.Errorf("disk %q identity mismatch", vol.DiskID)
		}
		if regional {
			typePath := strings.TrimPrefix(strings.TrimPrefix(disk.GetType(), "https://www.googleapis.com/compute/v1/"), "https://compute.googleapis.com/compute/v1/")
			if strings.HasPrefix(typePath, "regions/") {
				typePath = "projects/" + p.serviceConfig.ProjectID + "/" + typePath
			}
			expectedType := "projects/" + p.serviceConfig.ProjectID + "/regions/" + parts[3] + "/diskTypes/" + typePath[strings.LastIndex(typePath, "/")+1:]
			if typePath != expectedType {
				return fmt.Errorf("disk %q type placement mismatch", vol.DiskID)
			}
			if err := validateReplicaZones(disk, p.serviceConfig.ProjectID, parts[3], p.serviceConfig.Zone); err != nil {
				return fmt.Errorf("disk %q: %w", vol.DiskID, err)
			}
		}
		if disk.GetAccessMode() != "" && disk.GetAccessMode() != "READ_WRITE_SINGLE" {
			return fmt.Errorf("disk %q requires single-writer access", vol.DiskID)
		}
		if disk.GetStatus() != "READY" {
			return fmt.Errorf("disk %q is not READY (status %q)", vol.DiskID, disk.GetStatus())
		}
		if len(disk.GetUsers()) > 0 {
			return fmt.Errorf("disk %q is already attached to %v", vol.DiskID, disk.GetUsers())
		}
		if err := budget.add(disk.GetType(), disk.GetSizeGb(), disk.GetProvisionedIops(), disk.GetProvisionedThroughput(), false, regional); err != nil {
			return fmt.Errorf("disk %q: %w", vol.DiskID, err)
		}
	}
	return nil
}
