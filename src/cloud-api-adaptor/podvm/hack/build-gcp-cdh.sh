#!/usr/bin/env bash
# Copyright Confidential Containers Contributors
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

ref=${1:?guest-components commit is required}
arch=${2:?target architecture is required}
destination=${3:?output binary is required}
test_mode=${4:-}
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
patch_file="$script_dir/../patches/gcp-cdh-inline-header.patch"
# Updating the dependency requires reviewing this downstream storage patch too.
if [[ "$ref" != dcd55ea16ea4f4bcb87c1420f16af92e9eb15f2f ]]; then
    echo 'GCP CDH header patch has not been reviewed for this guest-components commit' >&2
    exit 1
fi
if [[ "$arch" != "$(uname -m)" ]]; then
    echo 'GCP CDH must be built on a builder matching the target architecture' >&2
    exit 1
fi
if [[ -n "$test_mode" && "$test_mode" != --test ]]; then
    echo 'Unknown GCP CDH build option' >&2
    exit 1
fi
destination=$(realpath -m -- "$destination")
build_dir="$script_dir/../build"
mkdir -p "$build_dir"
source_dir=$(mktemp -d "$build_dir/gcp-cdh.XXXXXX")
trap 'rm -rf -- "$source_dir"' EXIT

git -C "$source_dir" init -q
git -C "$source_dir" fetch -q --depth 1 https://github.com/confidential-containers/guest-components.git "$ref"
git -C "$source_dir" checkout -q --detach FETCH_HEAD
[[ "$(git -C "$source_dir" rev-parse HEAD)" == "$ref" ]]
git -C "$source_dir" apply --check "$patch_file"
git -C "$source_dir" apply "$patch_file"

export CARGO_TARGET_DIR
CARGO_TARGET_DIR=$(realpath -m -- "${CARGO_TARGET_DIR:-$build_dir/gcp-cdh-target}")
features=bin,ttrpc,kbs
target="$arch-unknown-linux-gnu"
cd "$source_dir"
cargo +1.90.0 build --locked --release --target "$target" -p confidential-data-hub \
    --no-default-features --features "$features" --bin ttrpc-cdh
if [[ "$test_mode" == --test ]]; then
    cargo +1.90.0 test --locked --release --target "$target" -p confidential-data-hub \
        --no-default-features --features "$features" gcp_persistent_header_tests -- --nocapture
fi
install -D -m0755 "$CARGO_TARGET_DIR/$target/release/ttrpc-cdh" "$destination"
{
    printf 'guest-components=%s\nheader=inline\nfeatures=%s\n' "$ref" "$features"
    printf 'patch-sha256=%s\n' "$(sha256sum "$patch_file" | cut -d ' ' -f 1)"
    printf 'binary-sha256=%s\n' "$(sha256sum "$destination" | cut -d ' ' -f 1)"
    rustc +1.90.0 --version
} > "$destination.build-info"
