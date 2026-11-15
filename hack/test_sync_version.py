"""Release synchronization regressions; no network or publishing."""

import contextlib
import importlib.util
import io
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("sync_version", Path(__file__).with_name("sync-version.py"))
sync = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sync)


class VersionSyncTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        fixture = {
            "VERSION": "0.2.0\n",
            "images/dnsdist/Dockerfile": "FROM powerdns/dnsdist-21:2.1.3@sha256:abc\n",
            "images/keepalived/Dockerfile": "ARG KEEPALIVED_VERSION=2.4.3\n",
            "deploy/helm/kudurru/Chart.yaml": "name: kudurru\nversion: 0.1.0\nappVersion: 0.1.0\n",
            "deploy/helm/kudurru-crds/Chart.yaml": "name: kudurru-crds\nversion: 0.1.0\nappVersion: 0.1.0\n",
            "deploy/helm/kudurru/values.yaml": "edge:\n  dnsdistVersion: 2.1.2\nvip:\n  keepalivedVersion: 2.4.3\n",
            "deploy/compose/single/docker-compose.yml": "image: ghcr.io/robertherbaugh/kudurru:0.1.0\n",
            "README.md": "First release: v0.1.0\nhelm install kudurru oci://ghcr.io/robertherbaugh/charts/kudurru --version 0.1.0\n",
            "CHANGELOG.md": "# Changelog\n\n## [Unreleased]\n\n### Fixed\n\n- Preserve this note.\n\n## [0.1.0] - 2026-10-18\n\nOriginal release notes.\n",
            "dev-docs/history.md": "ghcr.io/robertherbaugh/kudurru:0.1.0\n",
        }
        for name, text in fixture.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text)
        subprocess.run(["git", "init", "-q", str(self.root)], check=True)
        subprocess.run(["git", "add", "."], cwd=self.root, check=True)
        self.root_patch = mock.patch.object(sync, "ROOT", self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)

    def snapshot(self):
        return {p: p.read_bytes() for p in self.root.rglob("*") if p.is_file() and ".git" not in p.parts}

    def call(self, *args):
        with contextlib.redirect_stdout(io.StringIO()):
            return sync.main(list(args))

    def test_check_is_read_only_then_sync_is_idempotent(self):
        before = self.snapshot()
        # Actual Helm generation is exercised by make version-check. Here the
        # renderer is held out so write/check and failure boundaries are isolated.
        with mock.patch.object(sync, "render"):
            self.assertEqual(self.call("--check"), 1)
            self.assertEqual(self.snapshot(), before)
            self.assertEqual(self.call(), 0)
            after = self.snapshot()
            self.assertEqual(self.call(), 0)
            self.assertEqual(self.call("--check"), 0)
            self.assertEqual(self.snapshot(), after)
        self.assertIn("version: 0.2.0\n", sync.read(self.root, sync.CHARTS[0]))
        self.assertIn("--version 0.2.0", sync.read(self.root, "README.md"))
        self.assertIn("First release: v0.1.0", sync.read(self.root, "README.md"))
        self.assertEqual(sync.read(self.root, "dev-docs/history.md"), "ghcr.io/robertherbaugh/kudurru:0.1.0\n")
        changelog = sync.read(self.root, "CHANGELOG.md")
        self.assertEqual(changelog.count("## [0.2.0]"), 1)
        self.assertIn("- Preserve this note.", changelog)
        self.assertTrue(changelog.endswith("## [0.1.0] - 2026-10-18\n\nOriginal release notes.\n"))

    def test_invalid_input_and_failed_renderer_do_not_write(self):
        before = self.snapshot()
        for value in ("v0.2.0", "0.2.0-rc.1", "01.2.0", "", "../../bad", "1.2.3\nextra"):
            with self.assertRaises(ValueError):
                self.call("--version", value)
            self.assertEqual(self.snapshot(), before)
        with mock.patch.object(sync, "render", side_effect=ValueError("renderer failed")):
            with self.assertRaises(ValueError):
                self.call("--version", "0.3.0")
        self.assertEqual(self.snapshot(), before)

    def test_concurrent_input_edit_is_preserved(self):
        def edit(*_args):
            (self.root / "README.md").write_text("Concurrent user edit\n")
        with mock.patch.object(sync, "render", side_effect=edit):
            with self.assertRaisesRegex(ValueError, "file changed"):
                self.call()
        self.assertEqual(sync.read(self.root, "README.md"), "Concurrent user edit\n")
        self.assertIn("version: 0.1.0", sync.read(self.root, sync.CHARTS[0]))

    def test_exact_artifact_tags_and_supervisor_versions(self):
        programs = sync.programs(self.root)
        for version in ("0.2.0", "0.2.0-rc.3", "0.0.0-SNAPSHOT-abcdef", "0.1.0-SNAPSHOT-123abc", "0.2.0-rc.1-SNAPSHOT-abcdef"):
            self.assertEqual(sync.artifact_version("v" + version, "0.2.0"), version)
            text = "\n".join([
                "ghcr.io/robertherbaugh/kudurru:0.1.0-rc.1-hostnetwork",
                "ghcr.io/robertherbaugh/kudurru:0.1.0-debug",
                "ghcr.io/robertherbaugh/kudurru-dnsdist:2.1.2-k0.1.0",
                "ghcr.io/robertherbaugh/kudurru-keepalived:2.4.2-k0.1.0",
                "https://github.com/robertherbaugh/kudurru/releases/download/v0.1.0/install.yaml",
                "github.com/robertherbaugh/kudurru/deploy/kustomize/overlays/metallb?ref=v0.2.0",
            ])
            result = sync.references(text, version, programs)
            self.assertIn(f"kudurru:{version}-hostnetwork", result)
            self.assertIn(f"kudurru:{version}-debug", result)
            self.assertIn(f"kudurru-dnsdist:2.1.3-k{version}", result)
            self.assertIn(f"kudurru-keepalived:2.4.3-k{version}", result)
            self.assertIn(f"/download/v{version}/install.yaml", result)
            self.assertIn(f"/deploy/kustomize/overlays/metallb?ref=v{version}", result)
        for bad in ("0.1.0", "0.3.0-rc.1", "0.2.0-rc.0", "0.2.0-rc.01", "../../bad"):
            with self.assertRaises(ValueError):
                sync.artifact_version(bad, "0.2.0")

    def test_pins_unrelated_versions_and_local_secrets_are_preserved(self):
        text = "\n".join([
            "ghcr.io/robertherbaugh/kudurru:0.1.0@sha256:abcdef",
            "ghcr.io/robertherbaugh/kudurru:dev-54",
            "oci://ghcr.io/robertherbaugh/charts/kudurru --version 0.1.0-dev",
            "github.com/robertherbaugh/kudurru/config/overlays/metallb?ref=v0.1.0",
            "other/image:0.1.0",
            "API v1, CRDs v1alpha1, package 0.0.0, dependency 0.1.0",
        ])
        self.assertEqual(sync.references(text, "0.2.0", sync.programs(self.root)), text)
        secret = self.root / "deploy/compose/single/secrets/local"
        secret.parent.mkdir()
        secret.write_text("untracked local fixture")
        destination = self.root / "staged"
        sync.stage(self.root, destination, sync.tracked_files(self.root), {})
        self.assertFalse((destination / "deploy/compose/single/secrets").exists())
        self.assertTrue((destination / "deploy/compose/single/docker-compose.yml").exists())


if __name__ == "__main__":
    unittest.main()
