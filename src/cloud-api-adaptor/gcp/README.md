# Cloud API Adaptor (CAA) on GCP

This documentation has been moved to the official location [here](https://confidentialcontainers.org/docs/examples/gcp-simple/).

For this repository's GCP guest image, including persistent LUKS2 headers,
follow the [GCP guest build instructions](../podvm/README.md#gcp-guest-images).
Both build stages require `CLOUD_PROVIDER=gcp`.

## CSI data disks

Use the [CSI driver installation guide](https://github.com/qlad-secure/caa-csi-block-driver/blob/7118a385a6c2e432bf1a2d48def3bd12b78093f3/README.md)
and [regional disk configuration](https://github.com/qlad-secure/caa-csi-block-driver/blob/7118a385a6c2e432bf1a2d48def3bd12b78093f3/docs/gcp-regional.md).
CAA and CSI must use the same GCP project. Zonal disks must match CAA's
`GCP_ZONE`; for regional disks, that zone must be one of the two replica zones.
CSI's advertised PodVM topology must match CAA placement, which may differ
from the Kubernetes worker's zone.

In addition to VM creation and disk attachment permissions, the CAA identity
needs `compute.disks.get` and `compute.machineTypes.get` for disk preflight.
These are additional prerequisites, not a complete IAM role definition.
The guest image must resolve the exact disk identity through `/dev/disk/by-id`
for the selected SCSI or NVMe interface. Build the GCP guest described above
when using persistent LUKS2 disks; older detached-header disks are not migrated
automatically.

Before restarting CAA or replacing a PodVM, follow the
[restart and uncertain-create procedure](https://github.com/qlad-secure/caa-csi-block-driver/blob/7118a385a6c2e432bf1a2d48def3bd12b78093f3/docs/gcp-regional.md#caa-restart-and-uncertain-creates).
CAA's in-memory ownership and uncertainty state does not survive a restart.

## Credentials and Workload Identity Federation

The standard `entrypoint.sh` requires `GCP_CREDENTIALS`, `GCP_PROJECT_ID`,
`GCP_ZONE` and `PODVM_IMAGE_NAME`. `GCP_CREDENTIALS` accepts either a
`service_account` key JSON (the existing mode) or an `external_account`
credential configuration for Workload Identity Federation. Other credential
types are rejected. The entrypoint exports the same JSON through
`GOOGLE_APPLICATION_CREDENTIALS` for clients using ADC; no command override is
needed.

For file-sourced Kubernetes OIDC federation, configure the identity pool/provider
and IAM permissions first. Mount a projected ServiceAccount token in the CAA
container, set its audience to the provider's expected audience, and make the
credential configuration's `credential_source.file` point to that path. A
`service_account_impersonation_url` can select an IAM service account when the
federated principal is authorized to impersonate it. The credential configuration
does not contain a service account private key.

Use an administrator-managed Secret for `GCP_CREDENTIALS`. For Helm, reference an
existing Secret using `secrets.mode=reference` and `secrets.existingSecretName`,
and supply the projected token mount in your deployment configuration. The chart
does not create the identity pool/provider, IAM bindings or a GCP token projection.
Keep the credential JSON and tokens out of Helm values, ConfigMaps, logs and Git.

Credential configurations must come from a trusted administrator-controlled
source. In particular, review the token exchange/impersonation URLs and the
credential source before deployment; the type allowlist does not validate them.
See Google's [Kubernetes federation setup](https://cloud.google.com/iam/docs/workload-identity-federation-with-kubernetes)
and [credential configuration validation guidance](https://cloud.google.com/docs/authentication/external/externally-sourced-credentials).

This is separate from CSI driver authentication. An empty `GCP_CREDENTIALS`
still does not enable metadata ADC through the standard entrypoint.
