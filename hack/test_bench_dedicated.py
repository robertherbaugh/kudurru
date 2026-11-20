"""Check dedicated-host review, historical formulas and evidence integrity without timing."""

from contextlib import ExitStack, redirect_stderr
import importlib.util
import io
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("dedicated", Path(__file__).with_name("bench-dedicated.py"))
dedicated = importlib.util.module_from_spec(spec)
spec.loader.exec_module(dedicated)


class DedicatedControls(unittest.TestCase):
    def test_host_transition_requires_review_and_binds_every_input(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            calibration = root / "calibration"
            calibration.mkdir()
            for path in (root / "host.json", root / "approval.json", calibration / "host.json", calibration / "manifest.json"):
                dedicated.runner.save(path, {"fixture": str(path)})
            host, approval, transition = root / "host.json", root / "approval.json", root / "transition.json"
            expected = {"purpose": "qualification-host-transition",
                        "calibration_manifest_sha256": dedicated.runner.digest(calibration / "manifest.json"),
                        "calibration_approval_sha256": dedicated.runner.digest(approval),
                        "previous_host_sha256": dedicated.runner.digest(calibration / "host.json"),
                        "qualification_host_sha256": dedicated.runner.digest(host),
                        "decision": "accepted", "reviewed_by": "fixture owner", "reviewed_at": "2026-11-15",
                        "review_evidence": "explicit fixture review"}
            for key in expected:
                changed = dict(expected)
                changed[key] = "wrong" if key not in ("reviewed_by", "reviewed_at", "review_evidence") else ""
                dedicated.runner.save(transition, changed)
                with self.subTest(key=key), self.assertRaises(ValueError):
                    dedicated.validate_transition(host, calibration, approval, transition)
            dedicated.runner.save(transition, expected)
            self.assertEqual(dedicated.validate_transition(host, calibration, approval, transition), expected)

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
                self.assertEqual(original, root)
                self.assertEqual(order, "interleaved")
                self.assertEqual(output, root / "qualification/paired")
                self.assertEqual(list(output.parent.iterdir()), [output.parent / "dedicated-review.json"])
                expected_review = {
                    "approval": dedicated.read(approval), "approval_sha256": dedicated.runner.digest(approval),
                    "calibration_manifest_sha256": dedicated.runner.digest(calibration / "manifest.json"),
                    "host": {}, "host_transition": None, "host_transition_sha256": None, "release_qualified": False}
                self.assertEqual((output.parent / "dedicated-review.json").read_text(),
                                 json.dumps(expected_review, indent=2) + "\n")
                self.assertEqual(dedicated.reference.RUNNER_CLASS, dedicated.CLASS)
                self.assertEqual(dedicated.runner.LIMIT_FILES["blocking-zone"], str(dedicated.ROOT / "bench/limits.txt"))
                for scope in dedicated.CALIBRATED:
                    self.assertEqual(dedicated.runner.LIMIT_FILES[scope], str(calibration / (scope + "-limits.txt")))
                return 1  # A failed paired run must remain failed.
            with mock.patch.object(dedicated, "verify_calibration"), \
                    mock.patch.object(dedicated.runner, "measure") as measure, \
                    mock.patch.object(dedicated.paired, "execute", side_effect=execute) as paired:
                self.assertEqual(dedicated.qualify(root, calibration / "host.json", calibration, approval, root / "qualification"), 1)
                measure.assert_not_called()
                paired.assert_called_once_with(root, root / "qualification/paired", "interleaved")
                self.assertIs(dedicated.runner.measure, measure)
            self.assertIs(dedicated.runner.LIMIT_FILES, limits)
            self.assertEqual(dedicated.reference.RUNNER_CLASS, "ubicloud-standard-8-ubuntu-2404")


class HostEquivalencePreflight(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        directory = tempfile.TemporaryDirectory()
        cls.addClassCleanup(directory.cleanup)
        cls.gate = Path(directory.name) / "benchgate"
        subprocess.run(["go", "build", "-o", str(cls.gate), "./hack/benchgate"], cwd=dedicated.ROOT, check=True)

    def setUp(self):
        stack = ExitStack()
        self.addCleanup(stack.close)
        self.root = Path(stack.enter_context(tempfile.TemporaryDirectory()))
        self.original, self.calibration = self.root / "original", self.root / "calibration"
        self.original.mkdir()
        self.calibration.mkdir()
        self.host = self.calibration / "host.json"
        manifest = self.calibration / "manifest.json"
        self.approval, self.preflight_approval = self.root / "approval.json", self.root / "preflight.json"
        dedicated.runner.save(self.host, {})
        dedicated.runner.save(manifest, {})
        review = {"decision": "accepted", "reviewed_by": "fixture owner", "reviewed_at": "2026-11-15",
                  "review_evidence": "fixture review only"}
        manifest_hash = dedicated.runner.digest(manifest)
        dedicated.runner.save(self.approval, dict(review, calibration_manifest_sha256=manifest_hash))
        self.preflight_review = dict(review, purpose="host-equivalence-preflight",
                                     calibration_manifest_sha256=manifest_hash,
                                     calibration_approval_sha256=dedicated.runner.digest(self.approval),
                                     qualification_host_sha256=dedicated.runner.digest(self.host),
                                     method_sha256=dedicated.runner.digest(dedicated.__file__))
        dedicated.runner.save(self.preflight_approval, self.preflight_review)
        for report, metrics in {"cache": {("BenchmarkGet", "ns/op"): [100] * 5},
                                "upstream": {("BenchmarkStrategies/fast/strict", "p99-ns"): [100] * 5,
                                             ("BenchmarkStrategies/fast/strict", "q/s"): [100] * 5}}.items():
            (self.calibration / (report + "-limits.txt")).write_text(dedicated.profile(report, metrics, set(metrics)))
        self.rows = {"blocking": "BenchmarkEvaluate/exact-6 1 0 allocs/op",
                     "zone": "BenchmarkLookup100k/cname-6 1 2000 p99-ns",
                     "cache": "BenchmarkGet-6 1 125 ns/op",
                     "upstream": "BenchmarkStrategies/fast/strict-6 1 120 p99-ns 80 q/s"}
        self.heads = {self.original: dedicated.reference.REFERENCE, dedicated.ROOT: "control"}
        self.calls, self.failures = [], {}
        self.mutate_after = None
        self.limits, self.runner_class = dedicated.runner.LIMIT_FILES, dedicated.reference.RUNNER_CLASS
        stack.enter_context(mock.patch.object(dedicated, "verify_calibration"))
        stack.enter_context(mock.patch.object(dedicated, "validate_host"))
        stack.enter_context(mock.patch.object(dedicated.reference, "checkout_head", side_effect=self.heads.__getitem__))
        stack.enter_context(mock.patch.object(dedicated.subprocess, "check_output", return_value=b"go version fixture"))
        self.measure = stack.enter_context(mock.patch.object(dedicated.runner, "measure", side_effect=self.collect))
        self.paired = stack.enter_context(mock.patch.object(dedicated.paired, "execute", return_value=0))

    def collect(self, argv, stem, procs, timeout=1860):
        self.assertEqual(procs, 6)
        self.assertEqual(stem.parent.name, "preflight")
        self.calls.append(stem.name)
        gate_path = stem.parent / "benchgate"
        if stem.name == "benchgate-build":
            self.assertEqual(Path.cwd(), dedicated.ROOT)
            self.assertEqual(timeout, 300)
            self.assertEqual(argv, ["go", "build", "-o", str(gate_path), "./hack/benchgate"])
            code, text = 0, "fixture gate build\n"
        elif stem.name in self.rows:
            self.assertEqual(Path.cwd(), self.original)
            self.assertEqual(argv, dedicated.runner.commands()[stem.name])
            self.assertEqual(timeout, 1860)
            text = "cpu: fixture\n" + self.rows[stem.name] + "\n"
            code = 0
        else:
            self.assertEqual(Path.cwd(), dedicated.ROOT)
            self.assertEqual(timeout, 300)
            report = stem.name.removesuffix("-gate")
            rules = dedicated.ROOT / "bench/limits.txt" if report == "blocking-zone" else self.calibration / (report + "-limits.txt")
            path = stem.parent / (report + ".txt")
            self.assertEqual(argv, [str(gate_path), "-samples", "1", "-procs", "6",
                                    "-strict-cpu", "-distinguish-input-errors", "-limits", str(rules), str(path)])
            self.assertEqual(path.read_text(), "".join("cpu: fixture\n" + self.rows[scope] + "\n"
                                                       for scope in dedicated.reference.REPORTS[report]))
            gate = subprocess.run(argv, capture_output=True, text=True, check=False)
            code, text = gate.returncode, gate.stdout + gate.stderr
        receipt = {"argv": argv, "child_exit_code": code, "stop_reason": None, "group_reaped": True}
        receipt.update(self.failures.get(stem.name, {}))
        if stem.name == "benchgate-build" and receipt["child_exit_code"] == 0 and receipt["stop_reason"] is None:
            shutil.copy(self.gate, gate_path)
        stem.with_suffix(".log").write_text(text)
        dedicated.runner.save(stem.with_suffix(".json"), receipt)
        if stem.name == self.mutate_after:
            self.preflight_approval.write_text("changed after receipt")
        return receipt

    def run_qualify(self, name="qualification", phase="qualify"):
        output = self.root / name
        argv = ["bench-dedicated.py", phase, "--reference", str(self.original), "--host-record", str(self.host),
                "--calibration", str(self.calibration), "--approval", str(self.approval), "--output", str(output),
                "--host-equivalence-preflight", str(self.preflight_approval)]
        with mock.patch.object(dedicated.sys, "argv", argv), redirect_stderr(io.StringIO()) as stderr:
            code = dedicated.main()
        self.assertIs(dedicated.runner.LIMIT_FILES, self.limits)
        self.assertEqual(dedicated.reference.RUNNER_CLASS, self.runner_class)
        self.assertIs(dedicated.runner.measure, self.measure)
        return output, code, stderr.getvalue()

    def assert_result(self, output, outcome, gate_exits, measured, built=False):
        preflight = output / "preflight"
        receipts = {name + ".json": dedicated.runner.digest(preflight / (name + ".json")) for name in measured}
        self.assertEqual(dedicated.read(preflight / "result.json"), {
            "preflight_pass": outcome == "pass", "held": outcome in ("outside_bounds", "resource_stop"),
            "outcome": outcome, "counted": False, "release_qualified": False,
            "gate_exits": gate_exits, "receipt_sha256": receipts})
        reports = {name.removesuffix("-gate") + ".txt" for name in measured if name.endswith("-gate")}
        binaries = {"benchgate"} if built else set()
        self.assertEqual({path.name for path in preflight.iterdir()},
                         {"result.json", *binaries, *receipts, *reports, *(name + ".log" for name in measured)})
        if outcome != "pass":
            self.paired.assert_not_called()
            self.assertFalse((output / "paired").exists())

    def test_opt_in_requires_a_review_bound_to_every_input_before_any_measurement(self):
        for key in self.preflight_review:
            for missing in (False, True):
                with self.subTest(key=key, missing=missing):
                    changed = dict(self.preflight_review)
                    if missing:
                        del changed[key]
                    elif key == "decision":
                        changed[key] = "pending"
                    elif key.startswith("review"):
                        changed[key] = ""
                    else:
                        changed[key] = "wrong"
                    dedicated.runner.save(self.preflight_approval, changed)
                    output, code, stderr = self.run_qualify()
                    self.assertEqual(code, 1)
                    self.assertIn("Dedicated benchmark held: host-equivalence preflight", stderr)
                    self.assertFalse(output.exists())
                    self.measure.assert_not_called()
                    self.paired.assert_not_called()

    def test_flag_is_only_for_qualify(self):
        for phase in ("inspect-host", "calibrate", "review-template"):
            with self.subTest(phase=phase):
                output, code, stderr = self.run_qualify(phase=phase)
                self.assertEqual(code, 1)
                self.assertIn("only available with qualify", stderr)
                self.assertFalse(output.exists())
                self.measure.assert_not_called()

    def test_wrong_reference_refuses_before_measurement(self):
        self.heads[self.original] = "not-original"
        output, code, stderr = self.run_qualify()
        self.assertEqual(code, 1)
        self.assertIn("Dedicated benchmark refused: reference must be the exact original baseline commit", stderr)
        self.measure.assert_not_called()
        self.assert_result(output, "refused", {}, [])

    def test_dirty_control_or_toolchain_mismatch_refuses_before_measurement(self):
        def dirty_control(path):
            if path == dedicated.ROOT:
                raise ValueError("reference and control checkouts must be clean")
            return self.heads[path]

        with mock.patch.object(dedicated.reference, "checkout_head", side_effect=dirty_control):
            output, code, stderr = self.run_qualify("dirty")
            self.assertEqual(code, 1)
            self.assertIn("Dedicated benchmark refused: reference and control checkouts must be clean", stderr)
            self.assert_result(output, "refused", {}, [])
        with mock.patch.object(dedicated.subprocess, "check_output", side_effect=[b"control", b"original"]):
            output, code, stderr = self.run_qualify("toolchain")
            self.assertEqual(code, 1)
            self.assertIn("Dedicated benchmark refused: reference and control toolchains differ", stderr)
            self.assert_result(output, "refused", {}, [])
        self.measure.assert_not_called()

    def test_in_bounds_runs_original_scopes_and_gates_before_unchanged_paired_call(self):
        reports = dict.fromkeys(dedicated.reference.REPORTS, 0)
        expected_calls = ["benchgate-build", *dedicated.reference.SCOPES, *(report + "-gate" for report in reports)]
        paired_rows = "BenchmarkLookup100k/cname-6 1 900 p99-ns\n" * 5

        def execute(original, output, order):
            self.assertEqual(self.calls, expected_calls)
            self.assertEqual((original, order), (self.original, "interleaved"))
            self.assert_result(output.parent, "pass", reports, expected_calls, built=True)
            self.assertFalse(output.exists())
            (output / "reference").mkdir(parents=True)
            (output / "reference/blocking-zone.txt").write_text(paired_rows)
            return 1

        self.paired.side_effect = execute
        output, code, stderr = self.run_qualify()
        self.paired.assert_called_once_with(self.original, output / "paired", "interleaved")
        self.assertEqual(code, 1)
        self.assertEqual(stderr, "")
        self.assertEqual((output / "paired/reference/blocking-zone.txt").read_text(), paired_rows)
        self.assertEqual(list((output / "paired").rglob("*.txt")), [output / "paired/reference/blocking-zone.txt"])
        self.assertEqual(dedicated.read(output / "dedicated-review.json")["host_equivalence_preflight_approval_sha256"],
                         dedicated.runner.digest(self.preflight_approval))

    def test_fixed_ceiling_calibrated_ceiling_and_minimum_hold_and_retain_all_gates(self):
        for name, scope, text, report, finding in (
                ("fixed", "zone", "BenchmarkLookup100k/cname-6 1 2187 p99-ns", "blocking-zone", "exceeds the limit 2000"),
                ("calibrated", "upstream", "BenchmarkStrategies/fast/strict-6 1 121 p99-ns 80 q/s", "upstream", "exceeds the limit 120"),
                ("minimum", "upstream", "BenchmarkStrategies/fast/strict-6 1 120 p99-ns 79 q/s", "upstream", "below the minimum 80")):
            with self.subTest(name=name), mock.patch.dict(self.rows, {scope: text}):
                output, code, stderr = self.run_qualify(name)
                self.assertEqual(code, 1)
                self.assertEqual(stderr, "Dedicated benchmark held: host-equivalence preflight outside calibration bounds\n")
                reports = {key: int(key == report) for key in dedicated.reference.REPORTS}
                measured = ["benchgate-build", *dedicated.reference.SCOPES, *(key + "-gate" for key in reports)]
                self.assert_result(output, "outside_bounds", reports, measured, built=True)
                self.assertIn(finding, (output / "preflight" / (report + "-gate.log")).read_text())

    def test_failed_processes_and_resource_stops_have_distinct_outcomes(self):
        for label, key, value, outcome, message in (
                ("zone", "child_exit_code", 2, "refused", "reference process failed: zone (exit 2)"),
                ("zone", "group_reaped", False, "refused", "left descendants: zone"),
                ("zone", "stop_reason", "timeout", "resource_stop", "resource stop: zone (timeout)"),
                ("blocking-zone-gate", "stop_reason", "timeout", "resource_stop", "resource stop: blocking-zone-gate (timeout)")):
            with self.subTest(label=label, key=key), mock.patch.dict(self.failures, {label: {key: value}}):
                self.calls.clear()
                output, code, stderr = self.run_qualify(label + "-" + key)
                self.assertEqual(code, 1)
                prefix = "refused" if outcome == "refused" else "held"
                self.assertIn(f"Dedicated benchmark {prefix}: host-equivalence preflight {message}", stderr)
                measured = ["benchgate-build", *dedicated.reference.SCOPES, label] if label.endswith("-gate") else [
                    "benchgate-build", "blocking", "zone"]
                self.assertEqual(self.calls, measured)
                exits = {"blocking-zone": 0} if label.endswith("-gate") else {}
                self.assert_result(output, outcome, exits, measured, built=True)
                self.assertEqual(dedicated.read(output / "preflight" / (label + ".json"))[key], value)

    def test_gate_build_failure_refuses_before_reference_scopes(self):
        self.failures["benchgate-build"] = {"child_exit_code": 1}
        output, code, stderr = self.run_qualify()
        self.assertEqual(code, 1)
        self.assertIn("Dedicated benchmark refused: host-equivalence preflight could not build benchgate", stderr)
        self.assertEqual(self.calls, ["benchgate-build"])
        self.assert_result(output, "refused", {}, self.calls)
        self.assertEqual(dedicated.read(output / "preflight/benchgate-build.json")["child_exit_code"], 1)

    def test_gate_input_errors_are_not_bounds_holds(self):
        for name, row, finding in (
                ("malformed", "BenchmarkStrategies/fast/strict-6 1 x p99-ns 80 q/s", "bad value"),
                ("bad-shape", "BenchmarkStrategies/fast/strict-2 1 120 p99-ns 80 q/s", "expected GOMAXPROCS=6"),
                ("missing", "BenchmarkStrategies/fast/strict-6 1 121 p99-ns", "required metric absent")):
            with self.subTest(name=name), mock.patch.dict(self.rows, {
                    "zone": "BenchmarkLookup100k/cname-6 1 2187 p99-ns", "upstream": row}):
                output, code, stderr = self.run_qualify(name)
                self.assertEqual(code, 1)
                self.assertIn("Dedicated benchmark refused: host-equivalence preflight gate unable to assess upstream (exit 2)", stderr)
                reports = {"blocking-zone": 1, "cache": 0, "upstream": 2}
                measured = ["benchgate-build", *dedicated.reference.SCOPES, *(key + "-gate" for key in reports)]
                self.assert_result(output, "refused", reports, measured, built=True)
                self.assertIn(finding, (output / "preflight/upstream-gate.log").read_text())

    def test_gate_binary_change_refuses_and_preserves_gate_receipt(self):
        def collect(argv, stem, *args, **kwargs):
            receipt = self.collect(argv, stem, *args, **kwargs)
            if stem.name == "blocking-zone-gate":
                (stem.parent / "benchgate").write_text("changed gate fixture")
            return receipt

        self.measure.side_effect = collect
        output, code, stderr = self.run_qualify()
        self.assertEqual(code, 1)
        self.assertIn("Dedicated benchmark refused: host-equivalence preflight gate binary changed", stderr)
        measured = ["benchgate-build", *dedicated.reference.SCOPES, "blocking-zone-gate"]
        self.assertEqual(self.calls, measured)
        self.assert_result(output, "refused", {"blocking-zone": 0}, measured, built=True)

    def test_approval_mutation_refuses_and_preserves_receipts_and_output(self):
        self.mutate_after = "blocking"
        output, code, stderr = self.run_qualify()
        self.assertEqual(code, 1)
        self.assertIn("Dedicated benchmark refused: host-equivalence preflight approval changed", stderr)
        self.assertEqual(self.calls, ["benchgate-build", "blocking"])
        self.assert_result(output, "refused", {}, self.calls, built=True)
        retained = dedicated.inventory(output)
        _, code, stderr = self.run_qualify()
        self.assertEqual(code, 1)
        self.assertIn("existing output is refused", stderr)
        self.assertEqual(self.calls, ["benchgate-build", "blocking"])
        self.assertEqual(dedicated.inventory(output), retained)

    def test_source_change_refuses_and_preserves_receipts(self):
        def collect(argv, stem, *args, **kwargs):
            receipt = self.collect(argv, stem, *args, **kwargs)
            if stem.name == "blocking":
                self.heads[self.original] = "changed"
            return receipt

        self.measure.side_effect = collect
        output, code, stderr = self.run_qualify()
        self.assertEqual(code, 1)
        self.assertIn("Dedicated benchmark refused: source changed during host-equivalence preflight", stderr)
        self.assertEqual(self.calls, ["benchgate-build", "blocking"])
        self.assert_result(output, "refused", {}, self.calls, built=True)


if __name__ == "__main__":
    unittest.main()
