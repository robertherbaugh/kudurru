"""Exercise the D412 process order and failure controls without timing Go."""

import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("paired_interleaved", Path(__file__).with_name("bench-paired.py"))
paired = importlib.util.module_from_spec(spec)
spec.loader.exec_module(paired)


class InterleavedControls(unittest.TestCase):
    def simulate(self, root, failure=None, stop=False, changed=False, tampered=False, toolchain=False,
                 compare_tampered=False):
        checkout, output = root / "checkout", root / "output"
        checkout.mkdir()
        calls, native = [], []

        def measure(argv, stem, procs, timeout=1860):
            source = stem.parent.name
            calls.append((source, stem.name))
            # The full order is durable before the first native invocation.
            plan = json.loads((output / "plan.json").read_text())
            self.assertEqual(plan["sequence"], [list(row) for row in paired.interleaved_sequence()])
            self.assertEqual(procs, 6)
            is_native = argv[:2] == ["go", "test"]
            if is_native:
                scope, number = stem.name.rsplit("-", 1)
                self.assertEqual(argv, paired.runner.commands()[scope])
                self.assertEqual(Path.cwd(), checkout if source == "reference" else paired.reference.CONTROL)
                native.append((source, scope, int(number)))
            else:
                self.assertIn("-strict-cpu", argv)
                for key, value in (("-samples", "5"), ("-procs", "6"), ("-threshold", "20")):
                    self.assertEqual(argv[argv.index(key) + 1], value)
                name = stem.name.removesuffix("-gate").removesuffix("-compare")
                self.assertEqual(argv[argv.index("-limits") + 1], paired.runner.LIMIT_FILES[name])
            failed = ((failure == "reference" and calls[-1] == ("reference", "blocking-1")) or
                      (failure == "candidate" and calls[-1] == ("candidate", "zone-1")) or
                      (failure == "gate" and stem.name == "cache-gate") or
                      (failure == "compare" and stem.name == "cache-compare"))
            receipt = {"child_exit_code": int(failed), "group_reaped": True,
                       "stop_reason": "fixture-stop" if stop else None}
            stem.with_suffix(".log").write_text("cpu: fixture\nBenchmarkOriginal-6 1 100 ns/op\n")
            paired.runner.save(stem.with_suffix(".json"), receipt)
            if tampered and len(native) == 2:
                (output / "reference/blocking-1.log").write_text("replaced")
            if compare_tampered and stem.name == "blocking-zone-compare":
                (output / "reference/blocking-1.log").write_text("replaced after recording")
            return receipt

        with mock.patch.object(paired.reference, "checkout_head", side_effect=lambda p: paired.reference.REFERENCE if p == checkout else "control"), \
                mock.patch.object(paired.runner, "source_identity", side_effect=lambda: {"source": "changed" if native and changed else "original"}), \
                mock.patch.object(paired.subprocess, "check_output", side_effect=lambda argv: b"different" if toolchain and Path.cwd() == checkout else b"go version fixture"), \
                mock.patch.object(paired.runner, "measure", side_effect=measure):
            if stop or changed or tampered or toolchain or compare_tampered:
                with self.assertRaisesRegex(ValueError, "stopped|source changed|recording changed|toolchains differ"):
                    paired.execute(checkout, output, "interleaved")
            else:
                code = paired.execute(checkout, output, "interleaved")
                self.assertEqual(code, int(failure is not None))
        return output, calls, native, json.loads((output / "result.json").read_text())

    def test_adjacent_pairs_alternate_order_and_keep_all_original_commands(self):
        with tempfile.TemporaryDirectory() as temp:
            output, calls, native, result = self.simulate(Path(temp))
            self.assertEqual(native, paired.interleaved_sequence())
            self.assertEqual(len(native), 60)
            self.assertEqual(len(set(native)), 60)
            self.assertEqual(native[:8], [("reference", "blocking", 1), ("candidate", "blocking", 1),
                                        ("candidate", "zone", 1), ("reference", "zone", 1),
                                        ("reference", "cache", 1), ("candidate", "cache", 1),
                                        ("candidate", "upstream", 1), ("reference", "upstream", 1)])
            self.assertEqual(native[8:10], [("candidate", "blocking", 2), ("reference", "blocking", 2)])
            self.assertEqual(native[40:44], [("candidate", name, 1) for name in ("udp", "dot", "quic", "persist")])
            self.assertEqual(len(calls), 73)  # sixty native, ten absolute, three relative
            self.assertTrue(result["measurement_complete"])
            self.assertTrue(result["comparison_eligible"])
            self.assertFalse(result["release_qualified"])
            self.assertEqual((output / "reference/blocking-zone.txt").read_text().count("BenchmarkOriginal"), 10)

    def test_native_gate_and_comparison_failures_are_retained_without_retries(self):
        for failure in ("reference", "candidate", "gate", "compare"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temp:
                output, calls, native, result = self.simulate(Path(temp), failure=failure)
                self.assertEqual(len(native), 60)
                self.assertEqual(len(set(calls)), len(calls))
                self.assertEqual(result["exit_code"], 1)
                self.assertTrue(result["measurement_complete"])
                if failure in ("reference", "gate"):
                    self.assertFalse(result["comparison_eligible"])
                    self.assertEqual(result["comparisons"], {})
                if failure == "reference":
                    self.assertIn("FAIL: go test exited non-zero", (output / "reference/blocking-zone.txt").read_text())

    def test_stops_source_changes_tampering_and_toolchain_mismatch_hold_sequence(self):
        for mode, count in (("stop", 1), ("changed", 1), ("tampered", 2), ("toolchain", 0), ("compare_tampered", 60)):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temp:
                _, _, native, result = self.simulate(Path(temp), **{mode: True})
                self.assertEqual(len(native), count)
                self.assertFalse(result["measurement_complete"])
                self.assertEqual(result["exit_code"], 1)

    def test_external_corpus_override_is_refused(self):
        with tempfile.TemporaryDirectory() as temp, mock.patch.dict(os.environ, KUDURRU_BLOCKING_BENCH_LISTS="other"):
            with self.assertRaisesRegex(ValueError, "corpus override"):
                self.simulate(Path(temp))


if __name__ == "__main__":
    unittest.main()
