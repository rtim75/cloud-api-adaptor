#!/usr/bin/env bash
# Copyright Confidential Containers Contributors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

artifact=${1:-}
repository=${2:-}
source_ref=${3:-}
guest_ref=${4:-}
arch=${5:-}
destination=${6:?output binary is required}
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

# A mutable tag cannot identify the exact downstream subject being verified.
# The initial signed producer matches the reviewed x86_64 GCP release target.
if [[ ! "$artifact" =~ ^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$ ]] ||
   [[ ! "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] ||
   [[ ! "$source_ref" =~ ^[a-f0-9]{40}$ ]] ||
   [[ ! "$guest_ref" =~ ^[a-f0-9]{40}$ ]] || [[ "$arch" != x86_64 ]]; then
    echo 'GCP verified CDH requires an OCI sha256 digest, repository, source commits and x86_64 target' >&2
    exit 1
fi
destination=$(realpath -m -- "$destination")
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/gcp-cdh-provenance.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
cd "$work_dir"

# Use the same GitHub verifier as native providers, with the same certificate
# claims and our exact producer identity. Let gh handle multiple OCI bundles
# (for example after a producer rerun); never select an arbitrary first bundle.
gh attestation verify "oci://$artifact" --repo "$repository" --bundle-from-oci \
    --signer-workflow "$repository/.github/workflows/gcp_cdh.yaml" \
    --cert-identity "https://github.com/$repository/.github/workflows/gcp_cdh.yaml@refs/heads/main" \
    --source-digest "$source_ref" --signer-digest "$source_ref" \
    --source-ref refs/heads/main --deny-self-hosted-runners --format json > verified.json
jq -e --arg ref "$source_ref" '
    length > 0 and all(.[]; .verificationResult.signature.certificate |
        .sourceRepositoryDigest == $ref and .githubWorkflowSHA == $ref and
        .githubWorkflowRef == "refs/heads/main" and .runnerEnvironment == "github-hosted" and
        (.githubWorkflowTrigger == "push" or .githubWorkflowTrigger == "workflow_dispatch"))
' verified.json > /dev/null
oras pull "$artifact" --output "$work_dir/payload"
binary="$work_dir/payload/confidential-data-hub"
info="$binary.build-info"
if [[ ! -f "$binary" || -L "$binary" || ! -f "$info" || -L "$info" ]]; then
    echo 'Verified GCP CDH artifact is missing regular binary/build-info files' >&2
    exit 1
fi

# Both files are covered by the signed OCI digest. Bind that artifact to this
# checkout's patch/dependency/target too; never execute or source build-info.
require_line() {
    if [[ $(grep -Fxc -- "$1" "$info") != 1 ]]; then
        echo 'Verified GCP CDH build-info does not match the requested build inputs' >&2
        exit 1
    fi
}
require_line "guest-components=$guest_ref"
require_line 'header=inline'
require_line 'features=bin,ttrpc,kbs'
require_line "patch-sha256=$(sha256sum "$script_dir/../patches/gcp-cdh-inline-header.patch" | cut -d ' ' -f 1)"
require_line "binary-sha256=$(sha256sum "$binary" | cut -d ' ' -f 1)"
require_line "caa-source=$source_ref"
require_line "arch=$arch"
if [[ $(grep -c '^rustc 1\.90\.0 (' "$info") != 1 ]] || [[ $(wc -l < "$info") != 8 ]]; then
    echo 'Verified GCP CDH build-info has unexpected toolchain or fields' >&2
    exit 1
fi
install -D -m0755 "$binary" "$destination"
install -D -m0644 "$info" "$destination.build-info"
