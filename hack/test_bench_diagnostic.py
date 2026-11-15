"""Ensure diagnostics preserve failures without becoming qualification evidence."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("bench_diagnostic", Path(__file__).with_name("bench-diagnostic.py"))
diagnostic = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diagnostic)


class DiagnosticControls(unittest.TestCase):
    def simulate(self, root, failed=False, stop=False, changed=False):
        checkout, output = root / "checkout", root / "output"
        checkout.mkdir()
        calls = []

        def measure(argv, stem, procs, timeout):
            plan = json.loads((output / "plan.json").read_text())
            self.assertEqual(plan["commands"], diagnostic.sequence(output))
            self.assertEqual(plan["qualification_gates"], [])
            self.assertFalse(plan["release_qualified"])
            self.assertEqual(argv[:2], ["go", "test"])
            self.assertEqual(procs, 6)
            self.assertEqual(timeout, 660)
            self.assertEqual(Path.cwd(), checkout if stem.name.startswith("reference") else diagnostic.reference.CONTROL)
            calls.append(stem.name)
            receipt = {"child_exit_code": int(failed and len(calls) == 1),
                       "stop_reason": "fixture-stop" if stop else None, "group_reaped": True}
            stem.with_suffix(".log").write_text("native diagnostic output\n")
            diagnostic.runner.save(stem.with_suffix(".json"), receipt)
            if "-trace" in argv:
                Path(argv[argv.index("-trace") + 1]).write_bytes(b"trace fixture")
            return receipt

        with mock.patch.object(diagnostic.reference, "checkout_head", side_effect=lambda p: diagnostic.reference.REFERENCE if p == checkout else "candidate"), \
                mock.patch.object(diagnostic.runner, "source_identity", side_effect=lambda: {"source": "changed" if changed and calls else "original"}), \
                mock.patch.object(diagnostic.subprocess, "check_output", return_value=b"go version fixture"), \
                mock.patch.object(diagnostic, "host_state", return_value={"fixture": True}), \
                mock.patch.object(diagnostic.runner, "measure", side_effect=measure):
            if stop or changed:
                with self.assertRaisesRegex(ValueError, "resource stop|source changed"):
                    diagnostic.execute(checkout, output)
            else:
                self.assertEqual(diagnostic.execute(checkout, output), int(failed))
        return calls, json.loads((output / "result.json").read_text()), output

    def test_fixed_diagnostic_sequence_never_qualifies_or_reads_baseline(self):
        with tempfile.TemporaryDirectory() as temp:
            calls, result, output = self.simulate(Path(temp))
            self.assertEqual(calls, ["reference-1", "candidate-1", "candidate-2", "reference-2", "reference-trace", "candidate-trace"])
            self.assertTrue(result["diagnostic_complete"])
            self.assertFalse(result["release_qualified"])
            self.assertNotIn("comparisons", result)
            for name, digest in result["artifacts"].items():
                self.assertEqual(diagnostic.runner.digest(output / name), digest)

    def test_failed_native_process_retained_without_replay(self):
        with tempfile.TemporaryDirectory() as temp:
            calls, result, _ = self.simulate(Path(temp), failed=True)
            self.assertEqual(len(calls), 6)
            self.assertEqual(len(set(calls)), 6)
            self.assertEqual(result["commands"]["reference-1"], 1)
            self.assertEqual(result["exit_code"], 1)
            self.assertTrue(result["diagnostic_complete"])
            self.assertFalse(result["release_qualified"])

    def test_resource_stop_and_source_change_hold_remaining_commands(self):
        for mode in ("stop", "changed"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temp:
                calls, result, _ = self.simulate(Path(temp), **{mode: True})
                self.assertEqual(len(calls), 1)
                self.assertFalse(result["diagnostic_complete"])
                self.assertFalse(result["release_qualified"])
                self.assertEqual(result["exit_code"], 1)


if __name__ == "__main__":
    unittest.main()
