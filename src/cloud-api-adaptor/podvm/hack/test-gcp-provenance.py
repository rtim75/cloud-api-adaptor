#!/usr/bin/env python3
# Copyright Confidential Containers Contributors
# SPDX-License-Identifier: Apache-2.0
"""Contract tests for GCP CDH Make rules and consumer using mocked gh/oras."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

PODVM = Path(__file__).resolve().parents[1]
REPO = PODVM.parents[2]
SOURCE = "a" * 40
UPSTREAM = "dcd55ea16ea4f4bcb87c1420f16af92e9eb15f2f"
ARTIFACT = "ghcr.io/qlad-secure/cloud-api-adaptor/gcp-cdh-branch-test@sha256:" + "b" * 64
REPOSITORY = "qlad-secure/cloud-api-adaptor"


class ProvenanceTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory()
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.payload = self.root / "payload"
        self.payload.mkdir()
        binary = self.payload / "confidential-data-hub"
        binary.write_bytes(b"offline binary fixture\n")
        patch_hash = hashlib.sha256((PODVM / "patches/gcp-cdh-inline-header.patch").read_bytes()).hexdigest()
        self.info = [f"guest-components={UPSTREAM}", "header=inline", "features=bin,ttrpc,kbs",
                     f"patch-sha256={patch_hash}", f"binary-sha256={hashlib.sha256(binary.read_bytes()).hexdigest()}",
                     "rustc 1.90.0 (fixture)", f"caa-source={SOURCE}", "arch=x86_64"]
        self.write_info()
        self.claims = {"digest": SOURCE, "workflowDigest": SOURCE,
                       "workflowTrigger": "workflow_dispatch", "workflowRef": "refs/heads/denys/gcp-provider",
                       "runner": "github-hosted"}
        self.write_claims()
        mock = self.root / "bin"
        mock.mkdir()
        common = '''#!/usr/bin/python3
import json, os, shutil, sys
from pathlib import Path
args = sys.argv[1:]
with open(os.environ["TRACE"], "a") as trace:
    trace.write(json.dumps({"tool": Path(sys.argv[0]).name, "args": args, "cwd": os.getcwd()}) + "\\n")
'''
        (mock / "oras").write_text(common + '''
if args[0] == "resolve":
    print("sha256:" + "b" * 64)
elif args[0] == "discover":
    print(json.dumps({"manifests": [{"artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json", "digest": "sha256:" + "c" * 64}]}))
elif args[:2] == ["manifest", "fetch"]:
    print(json.dumps({"content": {"layers": [{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json", "digest": "sha256:" + "d" * 64}]}}))
elif args[:2] == ["blob", "fetch"]:
    Path(args[args.index("--output") + 1]).write_text("offline bundle fixture")
elif args[0] == "pull":
    if os.environ.get("PULL_FAIL"):
        sys.exit(8)
    assert args[1] == os.environ["ARTIFACT"]
    output = Path(args[args.index("--output") + 1])
    output.mkdir(parents=True, exist_ok=True)
    for source in Path(os.environ["PAYLOAD"]).iterdir():
        shutil.copyfile(source, output / source.name)
else:
    sys.exit(99)
''')
        (mock / "gh").write_text(common + '''
assert args[:2] == ["attestation", "verify"]
assert args[2] == "oci://" + os.environ["ARTIFACT"]
if os.environ.get("EXPECT_SIGNER", "yes") == "yes":
    assert args[args.index("--repo") + 1] == os.environ["REPOSITORY"]
    assert args[args.index("--signer-workflow") + 1] == os.environ["REPOSITORY"] + "/.github/workflows/gcp_cdh.yaml"
    assert args[args.index("--cert-identity") + 1] == "https://github.com/" + os.environ["REPOSITORY"] + "/.github/workflows/gcp_cdh.yaml@refs/heads/denys/gcp-provider"
    assert args[args.index("--source-digest") + 1] == "a" * 40
    assert args[args.index("--signer-digest") + 1] == "a" * 40
    assert args[args.index("--source-ref") + 1] == "refs/heads/denys/gcp-provider"
    assert "--deny-self-hosted-runners" in args and "--bundle-from-oci" in args
else:
    assert args[args.index("-R") + 1] == os.environ["REPOSITORY"]
    assert "--signer-workflow" not in args
if os.environ.get("VERIFY_FAIL"):
    sys.exit(7)
claims = json.loads(Path(os.environ["CLAIMS"]).read_text())
if os.environ.get("EXPECT_SIGNER", "yes") == "yes":
    certificate = {"sourceRepositoryDigest": claims["digest"], "githubWorkflowSHA": claims["workflowDigest"],
                   "githubWorkflowTrigger": claims["workflowTrigger"], "githubWorkflowRef": claims["workflowRef"],
                   "runnerEnvironment": claims["runner"]}
    result = [{"verificationResult": {"signature": {"certificate": certificate}}}]
    if os.environ.get("MULTIPLE_BUNDLES"):
        result *= 2
    if os.environ.get("EMPTY_BUNDLES"):
        result = []
    print(json.dumps(result))
else:
    print(json.dumps(claims))
''')
        for executable in mock.iterdir():
            executable.chmod(0o700)
        self.trace = self.root / "trace.jsonl"
        self.env = {"PATH": f"{mock}:/usr/local/bin:/usr/bin:/bin", "HOME": str(self.root),
                    "LC_ALL": "C", "TMPDIR": str(self.root), "PAYLOAD": str(self.payload),
                    "TRACE": str(self.trace), "CLAIMS": str(self.root / "claims.json"),
                    "ARTIFACT": ARTIFACT, "REPOSITORY": REPOSITORY, "MAKEFLAGS": "", "MFLAGS": ""}
        self.destination = self.root / "installed/confidential-data-hub"
        self.destination.parent.mkdir()
        self.destination.write_text("previous binary")
        self.destination.with_suffix(".build-info").write_text("previous build-info")

    def write_info(self):
        (self.payload / "confidential-data-hub.build-info").write_text("\n".join(self.info) + "\n")

    def write_claims(self):
        (self.root / "claims.json").write_text(json.dumps(self.claims))

    def events(self):
        return [json.loads(line) for line in self.trace.read_text().splitlines()] if self.trace.exists() else []

    def fetch(self, artifact=ARTIFACT, repository=REPOSITORY, source=SOURCE, arch="x86_64"):
        return subprocess.run(["bash", str(PODVM / "hack/fetch-gcp-cdh.sh"), artifact,
                               repository, source, UPSTREAM, arch, str(self.destination)],
                              cwd=self.root, env=self.env, capture_output=True, text=True, timeout=15)

    def assert_preserved(self):
        self.assertEqual(self.destination.read_text(), "previous binary")
        self.assertEqual(self.destination.with_suffix(".build-info").read_text(), "previous build-info")
        self.assertFalse(list(self.root.glob("gcp-cdh-provenance.*")))

    def test_signed_make_installs_verified_payload(self):
        result = subprocess.run(
            ["make", "--no-print-directory", "-f", str(PODVM / "Makefile.inc"), "binaries",
             f"BINARIES={self.destination}", f"CONFIDENTIAL_DATA_HUB={self.destination}",
             "CLOUD_PROVIDER=gcp", "VERIFY_PROVENANCE=yes", f"GCP_CDH_ARTIFACT={ARTIFACT}",
             f"GCP_CDH_REF={SOURCE}", f"GCP_CDH_REPOSITORY={REPOSITORY}",
             "ARCH=x86_64", "IMAGE_NAME=fixture"], cwd=self.root, env=self.env,
            capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.destination.read_bytes(), (self.payload / "confidential-data-hub").read_bytes())
        events = self.events()
        verify = next(i for i, e in enumerate(events) if e["tool"] == "gh")
        pull = next(i for i, e in enumerate(events) if e["args"][0] == "pull")
        self.assertLess(verify, pull)
        self.assertEqual(self.destination.stat().st_mode & 0o777, 0o755)
        self.assertFalse(list(self.root.glob("gcp-cdh-provenance.*")))

    def test_invalid_inputs_fail_before_registry_access(self):
        for changes in ({"artifact": "ghcr.io/example/cdh:latest"}, {"artifact": ""},
                        {"repository": "bad/repo/extra"}, {"source": "main"}, {"arch": "aarch64"}):
            with self.subTest(changes=changes):
                result = self.fetch(**changes)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.events(), [])
                self.assert_preserved()

    def test_invalid_signature_subject_or_signer_stops_before_pull(self):
        self.env["VERIFY_FAIL"] = "1"
        result = self.fetch()
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(any(e["tool"] == "gh" for e in self.events()))
        self.assertFalse(any(e["args"][0] == "pull" for e in self.events()))
        self.assert_preserved()

    def test_untrusted_claims_rejected(self):
        for field, value in (("digest", "e" * 40), ("workflowDigest", "f" * 40),
                             ("workflowTrigger", "pull_request"), ("workflowRef", "refs/heads/feature"),
                             ("workflowRef", "refs/heads/main"),
                             ("runner", "self-hosted")):
            with self.subTest(field=field):
                original = self.claims[field]
                self.claims[field] = value
                self.write_claims()
                result = self.fetch()
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(e["args"][0] == "pull" for e in self.events()))
                self.assert_preserved()
                self.claims[field] = original

    def test_wrong_payload_metadata_rejected(self):
        for index, replacement in enumerate(("guest-components=" + "e" * 40, "header=detached",
                                              "features=bin", "patch-sha256=" + "f" * 64,
                                              "binary-sha256=" + "0" * 64, "rustc 1.91.0 (fixture)",
                                              "caa-source=" + "e" * 40, "arch=aarch64")):
            with self.subTest(index=index):
                original = self.info[index]
                self.info[index] = replacement
                self.write_info()
                self.assertNotEqual(self.fetch().returncode, 0)
                self.assert_preserved()
                self.info[index] = original

    def test_missing_marker_rejected(self):
        (self.payload / "confidential-data-hub.build-info").unlink()
        self.assertNotEqual(self.fetch().returncode, 0)
        self.assert_preserved()

    def test_changed_binary_rejected(self):
        (self.payload / "confidential-data-hub").write_text("changed binary")
        self.assertNotEqual(self.fetch().returncode, 0)
        self.assert_preserved()

    def test_duplicate_metadata_rejected(self):
        self.info.append(self.info[0])
        self.write_info()
        self.assertNotEqual(self.fetch().returncode, 0)
        self.assert_preserved()

    def test_multiple_verified_attestations_supported(self):
        self.env["MULTIPLE_BUNDLES"] = "1"
        result = self.fetch()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_push_claims_accepted(self):
        self.claims["workflowTrigger"] = "push"
        self.write_claims()
        result = self.fetch()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_no_verified_attestation_rejected(self):
        self.env["EMPTY_BUNDLES"] = "1"
        self.assertNotEqual(self.fetch().returncode, 0)
        self.assert_preserved()

    def test_registry_failure_preserves_previous_output(self):
        self.env["PULL_FAIL"] = "1"
        self.assertNotEqual(self.fetch().returncode, 0)
        self.assert_preserved()

    def test_native_verifier_does_not_require_downstream_workflow(self):
        self.env["EXPECT_SIGNER"] = "no"
        self.claims["workflowRef"] = "refs/heads/main"
        self.write_claims()
        result = subprocess.run(["bash", str(PODVM.parent / "hack/verify-provenance.sh"),
                                 "-a", ARTIFACT, "-r", REPOSITORY, "-d", SOURCE, "-s"],
                                cwd=self.root, env=self.env, capture_output=True, text=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)


class ReleaseSelectionTests(unittest.TestCase):
    @staticmethod
    def workflow(name):
        # yq is already required by the PodVM Makefiles; avoid another test dependency.
        result = subprocess.run(["yq", "-o=json", ".", str(REPO / ".github/workflows" / name)],
                                capture_output=True, text=True, check=True)
        return json.loads(result.stdout)

    def test_release_contract_validation(self):
        workflow = self.workflow("podvm_mkosi.yaml")
        job = workflow["jobs"]["build-image"]
        validation = next(step["run"] for step in job["steps"] if step.get("name") == "Validate the selected image contract")
        for provider, arch, artifact, expected in (("", "arm64", "", 0), ("aws", "amd64", "", 0),
                                                   ("azure", "s390x", "", 0), ("gcp", "amd64", ARTIFACT, 0),
                                                   ("gcp", "arm64", ARTIFACT, 1), ("gcp", "amd64", "", 1),
                                                   ("gcp", "amd64", "ghcr.io/test/cdh:latest", 1),
                                                   ("typo", "amd64", "", 1)):
            with self.subTest(provider=provider, arch=arch, artifact=artifact):
                result = subprocess.run(["bash", "-e", "-c", validation], capture_output=True, text=True,
                                        env={"PATH": "/usr/bin:/bin", "CLOUD_PROVIDER": provider,
                                             "TARGET_ARCH": arch, "GCP_CDH_ARTIFACT": artifact})
                self.assertEqual(result.returncode == 0, expected == 0, result.stderr)
        for event in ("workflow_dispatch", "workflow_call"):
            self.assertEqual(workflow["on"][event]["inputs"]["cloud-provider"]["default"], "")
        build = next(step for step in job["steps"] if step.get("id") == "build_binaries")
        self.assertEqual(build["env"]["VERIFY_PROVENANCE"], "yes")
        self.assertTrue(any(step.get("uses", "").startswith("actions/attest@") for step in job["steps"]))

    def test_producer_pins_test_branch_source_and_attests_published_digest(self):
        workflow = self.workflow("gcp_cdh.yaml")
        self.assertEqual(set(workflow["on"]), {"workflow_dispatch", "push"})
        self.assertEqual(workflow["on"]["push"]["branches"], ["denys/gcp-provider"])
        job = workflow["jobs"]["publish"]
        self.assertEqual(job["if"], "github.ref == 'refs/heads/denys/gcp-provider'")
        checkout = next(step for step in job["steps"] if step.get("uses", "").startswith("actions/checkout@"))
        self.assertEqual(checkout["with"]["ref"], "${{ github.sha }}")
        attest = next(step for step in job["steps"] if step.get("uses", "").startswith("actions/attest@"))
        self.assertEqual(attest["with"]["subject-digest"], "${{ steps.publish.outputs.digest }}")
        self.assertTrue(attest["with"]["push-to-registry"])
        build = next(step["run"] for step in job["steps"] if step.get("id") == "build")
        self.assertIn('--test', build)
        self.assertIn('test "$(git rev-parse HEAD)" = "$GITHUB_SHA"', build)
        publish = next(step["run"] for step in job["steps"] if step.get("id") == "publish")
        self.assertIn('image="ghcr.io/${GITHUB_REPOSITORY,,}/gcp-cdh-branch-test"', publish)
        self.assertIn('"$image:branch-test-$GITHUB_SHA-amd64"', publish)

    def test_native_defaults_and_gcp_buildkit_secret_forwarding(self):
        for provider in ("", "aws", "azure", "gcp"):
            with self.subTest(provider=provider):
                result = subprocess.run(
                    ["make", "-n", "--no-print-directory", "podvm-binaries", f"CLOUD_PROVIDER={provider}",
                     "ARCH=x86_64", "AUTHFILE=/tmp/fixture-auth.json", "DOCKER_OPTS=",
                     f"GCP_CDH_ARTIFACT={ARTIFACT}", f"GCP_CDH_REF={SOURCE}"],
                    cwd=PODVM, capture_output=True, text=True, check=True)
                gcp = provider == "gcp"
                self.assertEqual("--build-arg GCP_CDH_ARTIFACT=" in result.stdout, gcp)
                self.assertEqual('--secret id=gcp-cdh-registry,src="/tmp/fixture-auth.json"' in result.stdout, gcp)
                if gcp:
                    self.assertIn(f'--build-arg GCP_CDH_REF="{SOURCE}"', result.stdout)
        publisher = self.workflow("podvm_publish.yaml")["jobs"]["podvm"]
        self.assertEqual(publisher["with"]["cloud-provider"], "${{ inputs.cloud-provider }}")
        self.assertEqual(publisher["with"]["gcp-cdh-artifact"], "${{ inputs.gcp-cdh-artifact }}")
        self.assertIn('fromJSON(\'["amd64","s390x","arm64"]\')', publisher["strategy"]["matrix"]["arch"])

    def test_builder_maps_host_authfile_to_secret_for_all_gcp_dependencies(self):
        # Execute the actual RUN body with a synthetic secret and make command.
        # This catches a host-only AUTHFILE path leaking into skopeo in the builder.
        lines = (PODVM / "Dockerfile.podvm_binaries").read_text().splitlines()
        index = next(i for i, line in enumerate(lines) if line.startswith("RUN --mount=type=secret,id=gcp-cdh-registry,"))
        body = []
        while lines[index].endswith("\\"):
            index += 1
            body.append(lines[index].removesuffix("\\").strip())
        recipe = " ".join(body)
        with tempfile.TemporaryDirectory() as scratch:
            root = Path(scratch)
            secret = root / "registry-config.json"
            secret.write_text("synthetic registry fixture")
            make = root / "make"
            make.write_text('#!/bin/sh\nprintf "%s" "$AUTHFILE"\n')
            make.chmod(0o700)
            recipe = recipe.replace("/root/.docker/config.json", str(secret))
            for provider in ("", "aws", "azure", "gcp"):
                with self.subTest(provider=provider):
                    original = "/host/registry-config.json"
                    result = subprocess.run(["sh", "-e", "-c", recipe], capture_output=True, text=True,
                                            env={"PATH": f"{root}:/usr/bin:/bin", "CLOUD_PROVIDER": provider,
                                                 "VERIFY_PROVENANCE": "yes", "AUTHFILE": original})
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout, str(secret) if provider == "gcp" else original)


if __name__ == "__main__":
    unittest.main()
