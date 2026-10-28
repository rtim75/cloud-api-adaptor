#!/usr/bin/env python3
# Copyright Confidential Containers Contributors
# SPDX-License-Identifier: Apache-2.0
"""Check image tree selection with local files; no Docker or cloud access."""

import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


PODVM = Path(__file__).resolve().parents[1]


def gcp_tree_recipe():
    lines = (PODVM / "Dockerfile.mkosi").read_text().splitlines()
    for index, line in enumerate(lines):
        if line.startswith('RUN if [ "$CLOUD_PROVIDER" = "gcp" ]; then'):
            recipe = line.removeprefix("RUN ")
            while recipe.endswith("\\"):
                index += 1
                recipe = recipe[:-1] + lines[index].strip()
            return recipe
    raise AssertionError("GCP image tree preparation is missing")


class ImageTreeTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.native = self.root / "resources/binaries-tree"
        self.gcp = self.root / "resources/binaries-tree-gcp"
        self.write(self.native, "usr/local/bin/confidential-data-hub", "upstream")
        self.write(self.gcp, "usr/local/bin/confidential-data-hub", "inline")
        self.write(self.gcp, "usr/local/bin/confidential-data-hub.build-info", "header=inline\n")

    def write(self, tree, name, content):
        path = tree / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    def prepare(self, provider):
        return subprocess.run(
            ["sh", "-c", gcp_tree_recipe()], cwd=self.root,
            env={**os.environ, "CLOUD_PROVIDER": provider},
            capture_output=True, text=True,
        )

    def test_gcp_image_preserves_customization_in_generated_tree(self):
        policy = "etc/kata-opa/allow-all.rego"
        self.write(self.native, policy, "package agent_policy\nExecProcessRequest := true\n")
        self.write(self.gcp, policy, "package agent_policy\nExecProcessRequest := false\n")
        self.write(self.gcp, "etc/custom-ca.pem", "synthetic trust root\n")
        result = self.prepare("gcp")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.native / policy).read_text(), (self.gcp / policy).read_text())
        self.assertEqual((self.native / "etc/custom-ca.pem").read_text(), "synthetic trust root\n")
        self.assertEqual((self.native / "usr/local/bin/confidential-data-hub").read_text(), "inline")

    def test_gcp_does_not_import_native_tree(self):
        self.write(self.native, "usr/local/bin/native-only", "native")
        result = self.prepare("gcp")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.native / "usr/local/bin/native-only").exists())
        self.assertEqual((self.native / "usr/local/bin/confidential-data-hub").read_text(), "inline")

    def test_missing_gcp_build_marker_fails_before_replacing_tree(self):
        (self.gcp / "usr/local/bin/confidential-data-hub.build-info").unlink()
        result = self.prepare("gcp")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.native / "usr/local/bin/confidential-data-hub").read_text(), "upstream")

    def test_native_providers_keep_their_tree(self):
        self.write(self.gcp, "etc/gcp-only", "gcp")
        self.write(self.native, "etc/custom-config", "native customization")
        for provider in ("", "aws", "azure"):
            with self.subTest(provider=provider):
                result = self.prepare(provider)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((self.native / "usr/local/bin/confidential-data-hub").read_text(), "upstream")
                self.assertEqual((self.native / "etc/custom-config").read_text(), "native customization")
                self.assertFalse((self.native / "etc/gcp-only").exists())


class BuildSelectionTests(unittest.TestCase):
    def test_provider_arguments_trees_and_tags(self):
        for provider in ("", "aws", "azure", "gcp"):
            with self.subTest(provider=provider):
                result = subprocess.run(
                    ["make", "--no-print-directory", "-n", "podvm-binaries", "image",
                     f"CLOUD_PROVIDER={provider}", "ARCH=x86_64", "AUTHFILE=", "DOCKER_OPTS="],
                    cwd=PODVM, capture_output=True, text=True,
                )
                self.assertEqual(result.returncode, 0, result.stderr)
                gcp = provider == "gcp"
                self.assertEqual("--build-arg CLOUD_PROVIDER=gcp" in result.stdout, gcp)
                self.assertEqual('dest="./resources/binaries-tree-gcp"' in result.stdout, gcp)
                self.assertEqual("podvm-binaries-ubuntu-amd64-gcp:" in result.stdout, gcp)
                self.assertEqual("podvm-generic-ubuntu-amd64-gcp:" in result.stdout, gcp)
                selected_tree = "./resources/binaries-tree" + ("-gcp" if gcp else "")
                self.assertIn(f"rm -rf {selected_tree}\n", result.stdout)

    def test_native_build_restores_upstream_cdh_after_gcp(self):
        for provider in ("", "aws", "azure"):
            with self.subTest(provider=provider), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                output = root / "files/usr/local/bin/confidential-data-hub"
                output.parent.mkdir(parents=True)
                output.write_text("patched-gcp")
                marker = output.with_suffix(".build-info")
                marker.write_text("header=inline\n")
                payload = b"fixture-upstream-cdh\n"
                with tarfile.open(root / "confidential-data-hub.tar.xz", "w:xz") as archive:
                    entry = tarfile.TarInfo("confidential-data-hub")
                    entry.size = len(payload)
                    entry.mode = 0o755
                    archive.addfile(entry, io.BytesIO(payload))
                mock_bin = root / "bin"
                mock_bin.mkdir()
                oras = mock_bin / "oras"
                oras.write_text(
                    '#!/bin/sh\ncase "$1" in\n'
                    'resolve) echo sha256:fixture ;;\n'
                    'pull) exit 0 ;;\n'
                    '*) exit 1 ;;\nesac\n'
                )
                oras.chmod(0o700)
                result = subprocess.run(
                    ["make", "--no-print-directory", "-f", str(PODVM / "Makefile.inc"),
                     "binaries", f"BINARIES={output}", f"CONFIDENTIAL_DATA_HUB={output}",
                     f"CLOUD_PROVIDER={provider}", "ARCH=x86_64", "IMAGE_NAME=fixture",
                     "VERIFY_PROVENANCE=no"],
                    cwd=root, env={**os.environ, "PATH": str(mock_bin) + os.pathsep + os.environ["PATH"]},
                    capture_output=True, text=True,
                )
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(output.read_bytes(), payload)
                self.assertFalse(marker.exists())


if __name__ == "__main__":
    unittest.main()
