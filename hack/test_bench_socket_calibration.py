"""Verify the first hosted socket/persistence calibration and every original sample."""

import hashlib
import json
from pathlib import Path
import re
import statistics
import unittest

ROOT = Path(__file__).resolve().parent.parent
DATA = ROOT / "bench/calibration/ubicloud8-sockets-persist-20261108"


class SocketCalibration(unittest.TestCase):
    def test_complete_original_recording_and_limits(self):
        evidence = json.loads((DATA / "provenance.json").read_text())
        self.assertEqual(evidence["source"], "9cc2ae0423070fc514c94a25ab5b92e3816889e7")
        self.assertEqual(evidence["runner_class"], "ubicloud-standard-8-ubuntu-2404")
        self.assertEqual((evidence["samples"], evidence["procs"]), (5, 6))
        self.assertFalse(evidence["qualification"])
        self.assertEqual(set(evidence["scopes"]), {"udp", "dot", "quic", "persist"})
        for name, digest in evidence["workload_files"].items():
            self.assertEqual(hashlib.sha256((ROOT / name).read_bytes()).hexdigest(), digest,
                             "workload changed: review calibration before replacing evidence")
        expected = {"udp": (2, 10), "dot": (4, 20), "quic": (4, 12), "persist": (4, 16)}
        for scope, data in evidence["scopes"].items():
            with self.subTest(scope=scope):
                report = (DATA / (scope + ".txt")).read_bytes()
                self.assertEqual(hashlib.sha256(report).hexdigest(), data["report_sha256"])
                values = {}
                for line in report.decode().splitlines():
                    if line.startswith("Benchmark"):
                        fields = line.split()
                        self.assertTrue(fields[0].endswith("-6"))
                        for i in range(2, len(fields), 2):
                            values.setdefault((fields[0][:-2], fields[i + 1]), []).append(float(fields[i]))
                self.assertEqual((len({name for name, _ in values}), len(values)), expected[scope])
                limits = (ROOT / "bench" / (scope + "-limits.txt")).read_bytes()
                self.assertEqual(hashlib.sha256(limits).hexdigest(), data["limits_sha256"])
                rules = {tuple(line.split()[:3]): line.split()[3:] for line in limits.decode().splitlines()
                         if line and not line.startswith("#")}
                self.assertEqual(len(rules), 2 * len(values))
                for metric in data["metrics"]:
                    name, unit = metric["name"], metric["unit"]
                    vector = values[name, unit]
                    self.assertEqual(len(vector), 5)
                    self.assertEqual(vector, metric["values"])
                    pattern = "^" + re.escape(name) + "$"
                    self.assertIn(("require", pattern, unit), rules)
                    median = statistics.median(vector)
                    if unit in ("failed", "lost"):
                        self.assertEqual(vector, [0] * 5)
                        self.assertIn(("zero", pattern, unit), rules)
                    elif unit in ("q/s", "MB/s", "entries"):
                        self.assertAlmostEqual(float(rules["minimum", pattern, unit][0]), median * .8)
                    else:
                        noise = 25 if unit == "ns/op" else 64 if unit == "B/op" else 0
                        bound = median + max(median * .2, noise)
                        self.assertAlmostEqual(float(rules["limit", pattern, unit][0]), bound)
                pids = set()
                self.assertEqual(len(data["receipts"]), 6)
                for name, digest in data["receipts"].items():
                    raw = (DATA / name).read_bytes()
                    self.assertEqual(hashlib.sha256(raw).hexdigest(), digest)
                    receipt = json.loads(raw)
                    self.assertEqual(receipt["child_exit_code"], 0)
                    self.assertIsNone(receipt["stop_reason"])
                    self.assertTrue(receipt["group_reaped"])
                    if not name.endswith("-gate.json"):
                        pids.add(receipt["pid"])
                        argv = receipt["argv"]
                        self.assertEqual(argv[argv.index("-count") + 1], "1")
                        self.assertEqual(argv[argv.index("-benchtime") + 1], "5s" if scope == "udp" else "1s")
                self.assertEqual(len(pids), 5)


if __name__ == "__main__":
    unittest.main()
