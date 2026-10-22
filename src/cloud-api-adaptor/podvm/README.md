# Pod VM images

[mkosi](https://github.com/systemd/mkosi) builds a bootable OS image from scratch. This way, we have full control over every detail of the image, especially over the image format and boot process. On the long run, we will implement fully, bit-by-bit reproducible images with mkosi, and use measured boot and an immutable root FS to ensure the image integrity through remote attestation.

## Prerequisites

There are various ways to install mkosi documented on the [project page](https://github.com/systemd/mkosi). Different distributions also package mkosi in their repositories, alongside the dependencies.

Refer to the [CI workflow](../../../.github/workflows/podvm_mkosi.yaml) to see which additional tools are required to build an image.

### Building the image

```sh
make # this will rebuild the builder, the binaries and the OS image
```

> [!WARNING]
> The `make` (and `make podvm-binaries`) targets will remove all customization made
> in the generated binary tree: `resources/binaries-tree-gcp` for GCP, or
> `resources/binaries-tree` otherwise. Add custom files after building binaries
> and before building the image.

```sh
make image # this will only rebuild the pod VM image
```

The default built pod VM image have support for only filesystem attester.

Set the `TEE_PLATFORM` variable to build the image with specific attester support.

For example, this will build the pod VM image with support for SNP attester.

```sh
TEE_PLATFORM=snp make image
```

Likewise `TEE_PLATFORM=az-cvm-vtpm` will enable support for Azure vTPM attesters.

Refer to the following [doc](https://github.com/confidential-containers/guest-components/tree/main?tab=readme-ov-file#build) for accepted values of TEE_PLATFORM.

### GCP guest images

Set `CLOUD_PROVIDER=gcp` for both binary and image builds. GCP uses a patched
CDH that stores LUKS2 headers on the persistent disk so they survive PodVM
recreation and disk copies. Generic builds use upstream CDH.

Run from this directory on a builder matching the target architecture; the GCP
CDH build does not support cross-compilation. For an x86_64 TDX target:

```sh
make CLOUD_PROVIDER=gcp TEE_PLATFORM=tdx podvm-binaries
# Add optional files to resources/binaries-tree-gcp before the next step.
make CLOUD_PROVIDER=gcp TEE_PLATFORM=tdx image
```

Choose `TEE_PLATFORM` for the VM/TEE, image and disk type. Use
`CLOUD_PROVIDER=gcp` for `image-debug` or `image-sftp` as well. Default GCP
binary/image container tags have a `-gcp` suffix. Binaries are exported to
`resources/binaries-tree-gcp`; the OS image is `build/system.raw`. The QCOW2
filename is shared with generic builds, so save outputs from different
providers separately.

Before building the image, inspect
`resources/binaries-tree-gcp/usr/local/bin/confidential-data-hub.build-info`.
It records the guest-components commit, `header=inline`, Cargo features, Rust
version, and patch/binary SHA256 hashes. Compare its `binary-sha256`
with:

```sh
sha256sum resources/binaries-tree-gcp/usr/local/bin/confidential-data-hub
```

The build-info file is unsigned. `VERIFY_PROVENANCE=yes` rejects the GCP build
because upstream OCI attestations do not cover the patch. If signed provenance
is required, provide it through your own build/signing process. The build script
rejects guest-components revisions for which the patch has not been reviewed.

This image supports newly initialized disks and existing inline LUKS2 disks.
Disks with detached headers require separate migration. Preserve their headers
and KBS key references; do not attach them as empty volumes. A replacement image
cannot reconstruct a lost header.

### Upload the image to the desired cloud provider

You can upload the image with the tool of your choice, but the recommended way is using [uplosi](https://github.com/edgelesssys/uplosi). Follow the uplosi readme to configure your upload for the desired cloud provider. Then run:

```sh
# Using -i and a imageVersionFile to increment the image version after the upload.
uplosi -i build/system.raw
```

If you want to use the image with libvirt, run the following to convert to qcow2 format:

```sh
qemu-img convert -f raw -O qcow2 build/system.raw build/system.qcow2
```

## Debug image

There is a debug variant of the image that provides a specific configuration to debug things within
the podvm. It has additional packages installed that are commonly needed for debugging.
Further, the image has access through the serial console enabled, you can access it through the portal
of the cloud provider.

```sh
make debug # this will rebuild the builder, the binaries and the OS image
```

```sh
make image-debug # this will only rebuild the OS image
```

Set `TEE_PLATFORM` as explained previously, to add support for specific attester in the debug image.

Notice that building a debug image will overwrite any previous existing debug or production image.

For using SSH, create a file `resources/authorized_keys` with your SSH public key. Ensure the permissions
are set to `0400` for the `authorized_keys` file. SSH access is only possible for the `root` user.

## Testing the image

To verify the podvm image is bootable and responsive before deploying to
cloud one can use a simple [smoketest](./hack/smoke_test.sh). It
uses libvirt to run the image and kata-agent-ctl to establish connection
to the podvm.

> [!WARNING]
> This script is intended for CI testing, expects tools in-place and contains
> only a rough cleanup. Use it on a disposable machine only.
>
> The smoke-test won't deploy tls certificates so the image must be built with
> `-disable-tls`:
>
>     mkdir -p ./resources/binaries-tree/etc/default
>     echo "TLS_OPTIONS=-disable-tls" > ./resources/binaries-tree/etc/default/agent-protocol-forwarder
>
> For GCP, place this file under `resources/binaries-tree-gcp/etc/default`
> instead, and build with `CLOUD_PROVIDER=gcp`.
>
> Do not forget to re-enable it before sending the image to production.

## Custom image configuration

For all providers, add custom files to the generated binary tree after
`make podvm-binaries` and before `make image`, using paths relative to the guest
root. Choose the tree for the provider:

| Provider | Binary tree |
| --- | --- |
| Generic, AWS, Azure | `resources/binaries-tree` |
| GCP (`CLOUD_PROVIDER=gcp`) | `resources/binaries-tree-gcp` |

For example, replace the GCP guest's default agent policy after the binary build:

```sh
cp your-policy.rego resources/binaries-tree-gcp/etc/kata-opa/allow-all.rego
make CLOUD_PROVIDER=gcp image
```

An image-only build preserves these files. A new `podvm-binaries` build replaces
the selected tree, so keep the originals outside it and reapply them afterward.
GCP image builds use only the GCP tree; custom files in `resources/binaries-tree`
are not imported. When adapting a generic build workspace for GCP, copy only
your custom files into the freshly generated GCP tree, not the generic binaries.

Enable custom systemd units through presets. Use the
[system post-install hook](mkosi.images/system/mkosi.postinst) for links that
need to be created during image construction.

Run the offline tree-selection and customization checks with
`python3 hack/test-gcp-image.py`. These checks use temporary files and Make
dry-runs; they do not build or boot a guest image.

To add packages, create `mkosi.images/system/mkosi.conf.d/ubuntu-extra.conf`:
```ini
[Match]
Distribution=ubuntu

[Content]
Packages=
    cowsay
```

## Limitations

The following limitations apply to these images. Notice that the limitations are intentional to
reduce complexity of configuration and CI and shall not be seen as open to-dos.

- Deployed images cannot be customized with cloud-init. Runtime configuration data is retrieved
  from IMDS via the project's `process-user-data` tool.

## Build s390x image
We can use the mkosi **ToolsTree** feature defined in `mkosi.conf` to download latest tools automatically:
```
[Runtime]
ToolsTree=default
```
And install **mkosi** from the repository:
```sh
git clone -b v26 https://github.com/systemd/mkosi
ln -s $PWD/mkosi/bin/mkosi /usr/local/bin/mkosi
mkosi --version
```
Another issue is s390x does not support UEFI. Instead, we can first use **mkosi** to build non-bootable system files, then use **zipl** to generate the bootloader and finally create the bootable image.

It requires a **s390x host** to build s390x image with make commands:
```
TEE_PLATFORM=se-attester make podvm-binaries
make image
# SE_BOOT=true make image
# make image-debug
# SE_BOOT=true make image-debug
```

The final output is `build/podvm-s390x.qcow2` or `build/podvm-s390x-se.qcow2`, which can be used as the Pod VM image in libvirt environment.
