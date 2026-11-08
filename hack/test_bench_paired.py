"""Check paired sequencing and failure retention without measuring Go code."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("paired", Path(__file__).with_name("bench-paired.py"))
paired = importlib.util.module_from_spec(spec)
spec.loader.exec_module(paired)


class PairedControls(unittest.TestCase):
    def simulate(self, root, reference_failure=False, candidate_failure=False,
                 compare_failure=False, stopped=False, changed=False, tampered=False):
        checkout, output = root / "checkout", root / "output"
        checkout.mkdir()
        calls = []

        def recording(directory, reports, scopes, code):
            directory.mkdir()
            hashes = {}
            for name in reports:
                path = directory / (name + ".txt")
                path.write_text("cpu: fixture\nBenchmarkOriginal-6 1 100 ns/op\n")
                hashes[name] = paired.runner.digest(path)
            names = [f"{name}-{i}" for i in range(1, 6) for name in scopes]
            names += [name + "-gate" for name in reports]
            for name in names:
                paired.runner.save(directory / (name + ".json"), {
                    "child_exit_code": code if name == "blocking-zone-gate" else 0,
                    "stop_reason": None, "group_reaped": True})
            for name in ("plan", "source"):
                paired.runner.save(directory / (name + ".json"), {})
            paired.runner.save(directory / "result.json", {
                "measurement_complete": True, "exit_code": code, "reports": hashes})

        def original(path, directory):
            calls.append("reference")
            self.assertEqual(path, checkout)
            if stopped:
                raise ValueError("resource stop")
            recording(directory, paired.reference.REPORTS, paired.reference.SCOPES, int(reference_failure))
            return int(reference_failure)

        def candidate(directory, runner_class, procs):
            calls.append("candidate")
            self.assertEqual((runner_class, procs), ("ubicloud-standard-8-ubuntu-2404", 6))
            recording(directory, paired.runner.REPORTS, paired.runner.SUITES, int(candidate_failure))
            if tampered:
                (output / "reference/cache.txt").write_text("replacement")
            return int(candidate_failure)

        def measure(argv, stem, procs, timeout):
            calls.append(stem.name)
            self.assertEqual(procs, 6)
            self.assertEqual(argv[argv.index("-threshold") + 1], "20")
            self.assertEqual(argv[argv.index("-samples") + 1], "5")
            self.assertIn("-strict-cpu", argv)
            self.assertEqual(Path(argv[-1]).parent, output / "candidate")
            self.assertEqual(Path(argv[argv.index("-baseline") + 1]).parent, output / "reference")
            return {"child_exit_code": int(compare_failure), "stop_reason": None, "group_reaped": True}

        with mock.patch.object(paired.reference, "checkout_head", side_effect=lambda p: paired.reference.REFERENCE if p == checkout else "control"), \
                mock.patch.object(paired.runner, "source_identity", side_effect=lambda: {"source": "changed" if calls and changed else "original"}), \
                mock.patch.object(paired.reference, "execute", side_effect=original), \
                mock.patch.object(paired.runner, "execute", side_effect=candidate), \
                mock.patch.object(paired.runner, "measure", side_effect=measure):
            if stopped or changed or tampered:
                with self.assertRaisesRegex(ValueError, "resource stop|source changed|recording changed"):
                    paired.execute(checkout, output)
            else:
                code = paired.execute(checkout, output)
                self.assertEqual(code, int(reference_failure or candidate_failure or compare_failure))
        return calls, json.loads((output / "result.json").read_text())

    def test_one_pair_and_unchanged_strict_comparisons(self):
        with tempfile.TemporaryDirectory() as temp:
            calls, result = self.simulate(Path(temp))
            self.assertEqual(calls, ["reference", "candidate", "blocking-zone-compare", "cache-compare", "upstream-compare"])
            self.assertTrue(result["measurement_complete"])
            self.assertTrue(result["comparison_eligible"])
            self.assertFalse(result["release_qualified"])

    def test_failed_reference_is_not_accepted_as_baseline(self):
        with tempfile.TemporaryDirectory() as temp:
            calls, result = self.simulate(Path(temp), reference_failure=True)
            self.assertEqual(calls, ["reference", "candidate"])
            self.assertEqual(result["reference_exit"], 1)
            self.assertEqual(result["exit_code"], 1)
            self.assertFalse(result["comparison_eligible"])
            self.assertEqual(result["comparisons"], {})

    def test_candidate_and_comparison_failures_remain_failures(self):
        for mode in ("candidate_failure", "compare_failure"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temp:
                calls, result = self.simulate(Path(temp), **{mode: True})
                self.assertEqual(len(calls), 5)
                self.assertEqual(result["exit_code"], 1)
                self.assertFalse(result["release_qualified"])

    def test_resource_source_and_artifact_failures_hold_remaining_work(self):
        for mode in ("stopped", "changed", "tampered"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as temp:
                calls, result = self.simulate(Path(temp), **{mode: True})
                self.assertEqual(calls, ["reference", "candidate"] if mode == "tampered" else ["reference"])
                self.assertFalse(result["measurement_complete"])
                self.assertEqual(result["exit_code"], 1)


if __name__ == "__main__":
    unittest.main()
