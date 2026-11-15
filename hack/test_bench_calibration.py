"""Keep calibrated ceilings tied to the retained original measurements."""

import hashlib
import json
from pathlib import Path
import re
import statistics
import unittest


ROOT = Path(__file__).resolve().parent.parent
CALIBRATION = ROOT / "bench/calibration/ubicloud8-20261108"


class CalibrationEvidence(unittest.TestCase):
    def test_original_scope_evidence_and_unchanged_margin(self):
        evidence = json.loads((CALIBRATION / "provenance.json").read_text())
        self.assertEqual(evidence["source"], "0cc9565e9a878f8223fc1cee5201eb7eca8aacd2")
        self.assertEqual((evidence["samples"], evidence["procs"]), (5, 6))
        self.assertEqual(evidence["full_reference_exit"], 1)
        self.assertFalse(evidence["full_reference_qualified"])
        for scope, data in evidence["scopes"].items():
            with self.subTest(scope=scope):
                report = (CALIBRATION / (scope + ".txt")).read_bytes()
                self.assertEqual(hashlib.sha256(report).hexdigest(), data["report_sha256"])
                limits = (ROOT / "bench" / (scope + "-limits.txt")).read_bytes()
                self.assertEqual(hashlib.sha256(limits).hexdigest(), data["limits_sha256"])
                rules = {tuple(line.split()[1:3]): float(line.split()[3])
                         for line in limits.decode().splitlines() if line.startswith("limit ")}
                vectors = {}
                for line in report.decode().splitlines():
                    if line.startswith("Benchmark"):
                        fields = line.split()
                        self.assertTrue(fields[0].endswith("-6"))
                        for i in range(2, len(fields), 2):
                            vectors.setdefault((fields[0][:-2], fields[i + 1]), []).append(float(fields[i]))
                self.assertEqual(len(vectors), len(data["metrics"]))
                self.assertEqual(len(rules), len(vectors))
                for metric in data["metrics"]:
                    values = vectors[metric["name"], metric["unit"]]
                    self.assertEqual(values, metric["values"])
                    self.assertEqual(len(values), 5)
                    median = statistics.median(values)
                    unit = metric["unit"]
                    noise = 25 if unit == "ns/op" else 64 if unit == "B/op" else 0
                    expected = median + max(median * .20, noise)
                    self.assertAlmostEqual(metric["limit"], expected, places=6)
                    self.assertEqual(rules["^" + re.escape(metric["name"]) + "$", unit], metric["limit"])
                native_pids = set()
                for name, digest in data["receipts"].items():
                    raw = (CALIBRATION / name).read_bytes()
                    self.assertEqual(hashlib.sha256(raw).hexdigest(), digest)
                    receipt = json.loads(raw)
                    self.assertEqual(receipt["child_exit_code"], 0)
                    self.assertIsNone(receipt["stop_reason"])
                    self.assertTrue(receipt["group_reaped"])
                    if not name.endswith("-gate.json"):
                        native_pids.add(receipt["pid"])
                self.assertEqual(len(native_pids), 5)


if __name__ == "__main__":
    unittest.main()
