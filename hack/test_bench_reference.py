"""Exercise historical-reference controls without executing any benchmarks."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("reference", Path(__file__).with_name("bench-reference.py"))
reference = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reference)


class ReferenceControls(unittest.TestCase):
    def simulate(self, root, failing=None, stopped=None, changed=False):
        checkout, output = root / "checkout", root / "output"
        checkout.mkdir()
        calls = []

        def head(path):
            return reference.REFERENCE if path == checkout else "trusted-control"

        def source():
            return {"fixture": "changed" if changed and calls else "original"}

        def measure(argv, stem, procs, timeout=1860):
            self.assertEqual(procs, 6)
            calls.append((stem.name, Path.cwd(), argv))
            native = argv[1] == "test"
            self.assertEqual(Path.cwd(), checkout if native else reference.CONTROL)
            self.assertEqual(argv.count("-count"), 1 if native else 0)
            if native:
                self.assertEqual(argv[argv.index("-count") + 1], "1")
            stem.with_suffix(".log").write_text("cpu: fixture\nBenchmarkFixture-6 1 1 ns/op\n")
            receipt = {"child_exit_code": 1 if stem.name == failing else 0,
                       "stop_reason": "disk_floor" if stem.name == stopped else None}
            reference.runner.save(stem.with_suffix(".json"), receipt)
            return receipt

        with mock.patch.object(reference, "checkout_head", side_effect=head), \
                mock.patch.object(reference.runner, "source_identity", side_effect=source), \
                mock.patch.object(reference.subprocess, "check_output", return_value=b"go version go1.27.3 linux/amd64\n"), \
                mock.patch.object(reference.runner, "measure", side_effect=measure):
            if stopped or changed:
                with self.assertRaisesRegex(ValueError, "resource stop|source changed"):
                    reference.execute(checkout, output)
            else:
                self.assertEqual(reference.execute(checkout, output), int(failing is not None))
        return calls, output

    def test_all_twenty_fresh_commands_and_three_checks(self):
        with tempfile.TemporaryDirectory() as temp:
            calls, output = self.simulate(Path(temp))
            self.assertEqual([c[0] for c in calls[:20]], [f"{name}-{i}" for i in range(1, 6) for name in reference.SCOPES])
            self.assertEqual(len(calls), 23)
            result = json.loads((output / "result.json").read_text())
            self.assertTrue(result["measurement_complete"])
            self.assertFalse(result["release_qualified"])
            self.assertEqual(set(result["reports"]), set(reference.REPORTS))

    def test_native_and_gate_failures_cannot_be_replaced(self):
        for failing in ("zone-2", "cache-gate"):
            with self.subTest(failing=failing), tempfile.TemporaryDirectory() as temp:
                calls, output = self.simulate(Path(temp), failing=failing)
                self.assertEqual(len(calls), 23)
                self.assertEqual(sum(c[0] == failing for c in calls), 1)
                self.assertEqual(json.loads((output / "result.json").read_text())["exit_code"], 1)
                if failing == "zone-2":
                    self.assertIn("FAIL: go test exited non-zero", (output / "blocking-zone.txt").read_text())

    def test_resource_stop_and_source_change_hold_remaining_sequence(self):
        for mode in ("stop", "changed"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temp:
                calls, output = self.simulate(Path(temp), stopped="blocking-1" if mode == "stop" else None, changed=mode == "changed")
                self.assertEqual(len(calls), 1)
                result = json.loads((output / "result.json").read_text())
                self.assertFalse(result["measurement_complete"])
                self.assertEqual(result["exit_code"], 1)

    def test_wrong_reference_refuses_before_go_or_output(self):
        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp) / "output"
            with mock.patch.object(reference, "checkout_head", return_value="candidate"), \
                    mock.patch.object(reference.subprocess, "check_output") as process:
                with self.assertRaisesRegex(ValueError, "exact original baseline commit"):
                    reference.execute(Path(temp), output)
                process.assert_not_called()
            self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
