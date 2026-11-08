"""Check fresh-process selection, durable failures and baseline provenance."""

from contextlib import redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("bench_release", Path(__file__).with_name("bench-release.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


class ReleaseBenchmarks(unittest.TestCase):
    def execute_fixture(self, output, failed_sample=None, stop_sample=None):
        calls = []

        def measure(argv, stem, procs, timeout=1860):
            calls.append((stem.name, argv, procs))
            code = int(stem.name == failed_sample)
            stem.with_suffix(".log").write_text("cpu: fixture\nBenchmarkX-6 1 10 ns/op\n")
            if stem.name.endswith("-gate"):
                code = int("FAIL:" in Path(argv[-1]).read_text())
            return {"child_exit_code": code,
                    "stop_reason": "disk_floor" if stem.name == stop_sample else None}

        with patch.object(runner, "source_identity", return_value={"source": "hash"}), \
             patch.object(runner, "digest", return_value="hash"), \
             patch.object(runner.subprocess, "check_output", side_effect=[b"", b"head\n", b"go fixture\n"]), \
             patch.object(runner, "measure", side_effect=measure), \
             patch.dict(runner.os.environ, {}, clear=True), redirect_stdout(io.StringIO()):
            code = runner.execute(output, "reviewed-class", 6)
        return code, calls

    def test_every_suite_has_five_fresh_commands_and_failed_sample_is_retained(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "measurement"
            code, calls = self.execute_fixture(output, failed_sample="udp-2")
            self.assertEqual(code, 1)
            self.assertEqual([name for name, _, _ in calls[:40]],
                             [f"{name}-{n}" for n in range(1, 6) for name in runner.SUITES])
            self.assertEqual(len(calls), 40 + len(runner.REPORTS))
            for _, argv, procs in calls[:40]:
                self.assertEqual(procs, 6)
                self.assertEqual(argv[argv.index("-count") + 1], "1")
            self.assertIn("FAIL:", (output / "udp.txt").read_text())
            self.assertNotIn("FAIL:", (output / "persist.txt").read_text())
            result = json.loads((output / "result.json").read_text())
            self.assertTrue(result["measurement_complete"])
            self.assertFalse(result["release_qualified"])

    def test_resource_stop_holds_remaining_sequence_and_does_not_replace_output(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "measurement"
            with self.assertRaisesRegex(ValueError, "remaining sequence held"):
                self.execute_fixture(output, stop_sample="cache-1")
            result = json.loads((output / "result.json").read_text())
            self.assertFalse(result["measurement_complete"])
            self.assertFalse((output / "upstream-1.log").exists())
            before = (output / "result.json").read_bytes()
            with self.assertRaises(FileExistsError):
                self.execute_fixture(output)
            self.assertEqual(before, (output / "result.json").read_bytes())

    def test_native_nonzero_completion_is_durable_before_enrichment(self):
        with tempfile.TemporaryDirectory() as directory:
            stem = Path(directory) / "sample"
            with patch.object(runner, "free_bytes", return_value=4 * runner.GIB):
                result = runner.measure([sys.executable, "-c", "raise SystemExit(7)"], stem, 6, 10)
            saved = json.loads(stem.with_suffix(".json").read_text())
            self.assertEqual(saved["child_exit_code"], 7)
            self.assertEqual(saved, result)
            self.assertTrue(saved["group_reaped"])
            self.assertIsNone(saved["stop_reason"])
            self.assertGreater(saved["wall_seconds"], 0)

    def test_baseline_class_inventory_and_hash_must_match(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            plan = {"runner_class": "reviewed-class", "procs": 6, "samples": 5,
                    "commands": runner.commands(), "platform": ["Linux", "x86_64"],
                    "go_version": "go fixture", "limits_sha256": "unchanged", "workload_inputs": {"bench_test.go": "original"}}
            runner.save(output / "plan.json", plan)
            reports = {}
            for name in runner.REPORTS:
                path = output / (name + ".txt")
                path.write_text("retained result\n")
                reports[name] = runner.digest(path)
            outcome = {"measurement_complete": True, "exit_code": 0, "reports": reports}
            runner.save(output / "result.json", outcome)
            runner.validate_baseline(output, plan)
            for key, wrong in (("runner_class", "other"), ("procs", 2), ("samples", 1),
                               ("go_version", "different"), ("limits_sha256", "changed"),
                               ("commands", {}), ("workload_inputs", {})):
                with self.subTest(key=key), self.assertRaisesRegex(ValueError, key):
                    runner.validate_baseline(output, dict(plan, **{key: wrong}))
            runner.save(output / "result.json", dict(outcome, exit_code=1))
            with self.assertRaisesRegex(ValueError, "failed"):
                runner.validate_baseline(output, plan)
            runner.save(output / "result.json", outcome)
            (output / "udp.txt").write_text("replacement sample\n")
            with self.assertRaisesRegex(ValueError, "report changed"):
                runner.validate_baseline(output, plan)


if __name__ == "__main__":
    unittest.main()
