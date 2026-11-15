"""Check dedicated-host review, historical formulas and evidence integrity without timing."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("dedicated", Path(__file__).with_name("bench-dedicated.py"))
dedicated = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dedicated)


class DedicatedControls(unittest.TestCase):
    def test_historical_formula_matches_all_original_hardware_profiles(self):
        expected = dedicated.expected_metrics()
        for directory in ("ubicloud8-20261108", "ubicloud8-sockets-persist-20261108"):
            root = dedicated.ROOT / "bench/calibration" / directory
            for scope in dedicated.read(root / "provenance.json")["scopes"]:
                values = dedicated.vectors((root / (scope + ".txt")).read_text())
                profile = dedicated.profile(scope, values, expected[scope])
                original = (dedicated.ROOT / "bench" / (scope + "-limits.txt")).read_text()
                rules = lambda text: {tuple(line.split()[:3]): line.split()[3:] for line in text.splitlines()
                                      if line and not line.startswith("#")}
                actual, wanted = rules(profile), rules(original)
                for key, bound in wanted.items():
                    self.assertIn(key, actual)
                    if bound:
                        self.assertAlmostEqual(float(bound[0]), float(actual[key][0]), places=6)

    def test_missing_samples_metrics_nonfinite_and_loss_refuse_profile(self):
        expected = {("BenchmarkSocket", "lost")}
        for vector in ([0] * 4, [0, 0, 0, 0, 1], [float("nan")] * 5, [-1] * 5):
            with self.subTest(vector=vector), self.assertRaises(ValueError):
                dedicated.profile("udp", {("BenchmarkSocket", "lost"): vector}, expected)
        with self.assertRaisesRegex(ValueError, "inventory"):
            dedicated.profile("udp", {}, expected)

    def test_pending_host_and_changed_identity_refuse(self):
        host = {"identity": {"go_env": {"GOVERSION": "go1.27.4", "GOOS": "linux", "GOARCH": "amd64",
                                       "CGO_ENABLED": "0", "GOEXPERIMENT": ""},
                             "affinity": list(range(6))},
                **{key: "reviewed fixture" for key in ("physical_host", "vm_id", "cpu_reservation",
                                                       "isolation_evidence", "guest_image", "reviewed_by",
                                                       "reviewed_at", "review_evidence")}}
        with mock.patch.object(dedicated, "host_identity", return_value=host["identity"]):
            with self.assertRaisesRegex(ValueError, "human review"):
                dedicated.validate_host(host)
            host["decision"] = "accepted"
            dedicated.validate_host(host)
            host["identity"]["go_env"]["CGO_ENABLED"] = "1"
            with self.assertRaisesRegex(ValueError, "CGO_ENABLED=0"):
                dedicated.validate_host(host)
        with mock.patch.object(dedicated, "host_identity", return_value={}):
            with self.assertRaisesRegex(ValueError, "identity changed"):
                dedicated.validate_host(host)

    def test_manifest_tampering_failed_calibration_and_wrong_source_refuse(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            dedicated.runner.save(root / "result.json", {"measurement_complete": True, "exit_code": 1})
            dedicated.runner.save(root / "plan.json", {"sources": {}})
            dedicated.runner.save(root / "manifest.json", {"files": dedicated.inventory(root)})
            with self.assertRaisesRegex(ValueError, "failed calibration"):
                dedicated.verify_calibration(root)
            dedicated.runner.save(root / "result.json", {"measurement_complete": True, "exit_code": 0})
            with self.assertRaisesRegex(ValueError, "evidence changed"):
                dedicated.verify_calibration(root)
            dedicated.runner.save(root / "manifest.json", {"files": dedicated.inventory(root)})
            with self.assertRaisesRegex(ValueError, "source identity"):
                dedicated.verify_calibration(root)

    def test_no_candidate_command_before_bound_human_approval(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            approval = root / "approval.json"
            for document in ({"decision": "pending"}, {"decision": "accepted", "reviewed_by": "owner",
                              "reviewed_at": "2026-11-15", "review_evidence": "review", "calibration_manifest_sha256": "wrong"}):
                dedicated.runner.save(approval, document)
                with mock.patch.object(dedicated, "verify_calibration"), \
                        mock.patch.object(dedicated.runner, "digest", return_value="actual"), \
                        mock.patch.object(dedicated.paired, "execute") as execute:
                    with self.assertRaisesRegex(ValueError, "human review|bind"):
                        dedicated.qualify(root, root, root, approval, root / "output")
                    execute.assert_not_called()

    def test_guard_checks_after_process_and_restores_original_collector(self):
        with tempfile.TemporaryDirectory() as temp:
            host = Path(temp) / "host.json"
            dedicated.runner.save(host, {})
            calls = []
            def measure(*args, **kwargs):
                calls.append(1)
                host.write_text("changed")
                return {"child_exit_code": 0}
            with mock.patch.object(dedicated, "validate_host"), \
                    mock.patch.object(dedicated.runner, "measure", side_effect=measure) as original:
                with self.assertRaisesRegex(ValueError, "host review changed"):
                    with dedicated.guarded(host):
                        dedicated.runner.measure([], Path(temp) / "receipt", 6)
                self.assertIs(dedicated.runner.measure, original)
            self.assertEqual(calls, [1])

    def test_reference_identity_cannot_be_overwritten_by_reused_path(self):
        with self.assertRaisesRegex(ValueError, "distinct"):
            dedicated.calibrate(Path("/same"), Path("/same"), Path("/host"), Path("/output"))

    def simulate_calibration(self, directory, failure=False, stop=False):
        original, later, output = directory / "original", directory / "later", directory / "output"
        original.mkdir()
        later.mkdir()
        host = directory / "host.json"
        dedicated.runner.save(host, {})
        heads = {original: dedicated.reference.REFERENCE, later: dedicated.LATER, dedicated.ROOT: "control"}
        native = []
        expected = {name: {("Benchmark" + scope, "ns/op") for scope in scopes}
                    for name, scopes in dedicated.runner.REPORTS.items()}

        def measure(argv, stem, procs, timeout=1860):
            self.assertEqual(procs, 6)
            is_native = argv[:2] == ["go", "test"]
            if is_native:
                scope, number = stem.name.rsplit("-", 1)
                self.assertEqual(argv, dedicated.runner.commands()[scope])
                self.assertEqual(Path.cwd(), original if scope in dedicated.reference.SCOPES else later)
                native.append((scope, int(number)))
                text = "cpu: fixture\nBenchmark" + scope + "-6 1 100 ns/op\n"
            else:
                self.assertEqual(Path.cwd(), dedicated.ROOT)
                self.assertIn("-strict-cpu", argv)
                self.assertEqual(argv[argv.index("-samples") + 1], "5")
                text = "shape passed\n"
            receipt = {"argv": argv, "pid": len(native), "child_exit_code": int(failure and stem.name == "upstream-1"),
                       "stop_reason": "fixture-stop" if stop else None, "group_reaped": True}
            dedicated.runner.save(stem.with_suffix(".json"), receipt)
            stem.with_suffix(".log").write_text(text)
            return receipt

        with mock.patch.object(dedicated.reference, "checkout_head", side_effect=lambda path: heads[path]), \
                mock.patch.object(dedicated.runner, "source_identity", return_value={"fixture": "hash"}), \
                mock.patch.object(dedicated.subprocess, "check_output", return_value=b"go version go1.27.4"), \
                mock.patch.object(dedicated, "expected_metrics", return_value=expected), \
                mock.patch.object(dedicated.runner, "measure", side_effect=measure):
            if stop:
                with self.assertRaisesRegex(ValueError, "remaining sequence held"):
                    dedicated.calibrate(original, later, host, output)
            else:
                self.assertEqual(dedicated.calibrate(original, later, host, output), int(failure))
                self.assertFalse(dedicated.read(output / "result.json")["release_qualified"])
                if not failure:
                    dedicated.verify_calibration(output)
        return native, output

    def test_calibration_uses_only_historical_sources_and_preserves_all_failures(self):
        for failure in (False, True):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temp:
                native, output = self.simulate_calibration(Path(temp), failure=failure)
                self.assertEqual(native, [(scope, i) for i in range(1, 6) for scope in dedicated.runner.SUITES])
                self.assertEqual(len(native), 40)
                self.assertEqual(len(list(output.glob("*-limits.txt"))), 0 if failure else 6)
                if failure:
                    self.assertIn("FAIL: go test exited non-zero", (output / "upstream.txt").read_text())

    def test_calibration_resource_stop_holds_later_commands(self):
        with tempfile.TemporaryDirectory() as temp:
            native, output = self.simulate_calibration(Path(temp), stop=True)
            self.assertEqual(len(native), 1)
            self.assertFalse(dedicated.read(output / "result.json")["measurement_complete"])

    def test_reviewed_profile_wraps_original_interleaved_engine_and_restores_defaults(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            calibration = root / "calibration"
            calibration.mkdir()
            dedicated.runner.save(calibration / "host.json", {})
            dedicated.runner.save(calibration / "manifest.json", {})
            approval = root / "approval.json"
            dedicated.runner.save(approval, {"decision": "accepted", "reviewed_by": "owner",
                "reviewed_at": "2026-11-15", "review_evidence": "fixture review only",
                "calibration_manifest_sha256": dedicated.runner.digest(calibration / "manifest.json")})
            limits = dedicated.runner.LIMIT_FILES
            def execute(original, output, order):
                self.assertEqual(order, "interleaved")
                self.assertEqual(output, root / "qualification/paired")
                self.assertTrue((output.parent / "dedicated-review.json").is_file())
                self.assertEqual(dedicated.reference.RUNNER_CLASS, dedicated.CLASS)
                self.assertEqual(dedicated.runner.LIMIT_FILES["blocking-zone"], str(dedicated.ROOT / "bench/limits.txt"))
                for scope in dedicated.CALIBRATED:
                    self.assertEqual(dedicated.runner.LIMIT_FILES[scope], str(calibration / (scope + "-limits.txt")))
                return 1  # A failed paired run must remain failed.
            with mock.patch.object(dedicated, "verify_calibration"), mock.patch.object(dedicated.paired, "execute", side_effect=execute):
                self.assertEqual(dedicated.qualify(root, calibration / "host.json", calibration, approval, root / "qualification"), 1)
            self.assertIs(dedicated.runner.LIMIT_FILES, limits)
            self.assertEqual(dedicated.reference.RUNNER_CLASS, "ubicloud-standard-8-ubuntu-2404")


if __name__ == "__main__":
    unittest.main()
