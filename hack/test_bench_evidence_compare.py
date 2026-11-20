"""Exercise the isolated, read-only retained evidence CLI."""

import csv
import hashlib
import io
import json
import os
from pathlib import Path
import statistics
import subprocess
import sys
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().with_name("bench-evidence-compare.py")
ROOT = SCRIPT.parent.parent
EVIDENCE = ROOT / "dev-docs/milestones/v0.2/evidence/2026-11-16-final-source"
ARCHIVE = EVIDENCE / "historical-calibration-inputs.tar.gz"
FINAL = EVIDENCE / "performance-corrected-metric-vectors.csv"
LIMITS = ROOT / "bench/limits.txt"
CNAME = "BenchmarkLookup100k/cname"


class EvidenceCompare(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        self.scratch = self.directory / "scratch"
        self.scratch.mkdir()

    def invoke(self, *args):
        return subprocess.run(
            [sys.executable, "-I", "-B", str(SCRIPT), *map(str, args)], cwd=self.directory,
            env={**os.environ, "TMPDIR": str(self.scratch)}, capture_output=True, text=True,
        )

    def comparison(self, *args):
        completed = self.invoke(*args)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        result = json.loads(completed.stdout)
        self.assertIs(result["release_qualified"], False)
        self.assertEqual(list(self.scratch.iterdir()), [])
        return result

    def metric(self, result, name, unit="p99-ns", report="blocking-zone"):
        return next(row for row in result["metrics"]
                    if (row["report"], row["name"], row["unit"]) == (report, name, unit))

    def test_committed_same_source_drift_and_headroom(self):
        def hashes():
            return [hashlib.sha256(path.read_bytes()).hexdigest() for path in (ARCHIVE, FINAL, LIMITS)]

        original = hashes()
        result = self.comparison(ARCHIVE, FINAL, "--limits", LIMITS)
        self.assertEqual(hashes(), original)
        self.assertEqual(list(self.directory.iterdir()), [self.scratch])
        self.assertEqual(result["summary"]["matched"], 219)
        time = result["summary"]["ns_time"]
        self.assertEqual(time["count"], 79)
        self.assertAlmostEqual(time["median_ratio"], 1.110836219693221)
        self.assertAlmostEqual(time["min_ratio"], 0.9298507462686567)
        self.assertAlmostEqual(time["max_ratio"], 1.8811214272710723)
        cname = self.metric(result, CNAME)
        self.assertEqual((cname["before_median"], cname["after_median"]), (1890, 2187))
        self.assertEqual(result["cname_p99_headroom"], {
            "limit_ns": 2000,
            "before": {"headroom_percent": 5.5, "min_sample_ns": 1706, "max_sample_ns": 2602},
            "after": {"headroom_percent": -9.35, "min_sample_ns": 1897, "max_sample_ns": 2724},
        })

    def test_committed_within_run_ratios(self):
        result = self.comparison(FINAL, FINAL, "--after-role", "candidate")
        self.assertEqual(result["summary"]["after_only"], 373)
        self.assertEqual(result["summary"]["ns_time"]["count"], 79)
        self.assertAlmostEqual(result["summary"]["ns_time"]["median_ratio"], 1.0045454545454546)
        self.assertAlmostEqual(self.metric(result, CNAME)["ratio"], 2363 / 2187)
        self.assertAlmostEqual(self.metric(result, "BenchmarkLookup100k/hit")["ratio"], 1295 / 1078)

    def write_csv(self, name, rows):
        path = self.directory / name
        with path.open("w", newline="", encoding="utf-8") as stream:
            writer = csv.writer(stream)
            writer.writerow(["role", "report", "name", "unit", *[f"sample_{sample}" for sample in range(1, 6)], "median"])
            writer.writerows(rows)
        return path

    def test_exact_report_name_unit_join_and_ratio_of_medians(self):
        first = [1, 2, 3, 4, 5]
        last = [81, 9, 1, 27, 3]

        def row(report, unit, values):
            return ["reference", report, "BenchmarkFixture", unit, *values, statistics.median(values)]

        before = self.write_csv("before.csv", [row("cache", "ns/op", first), row("zone", "ns/op", first),
                                               row("cache", "allocs/op", [0] * 5)])
        after = self.write_csv("after.csv", [row("cache", "ns/op", last), row("cache", "p99-ns", last),
                                             row("cache", "allocs/op", [1] * 5)])
        result = self.comparison(before, after)
        self.assertEqual(result["summary"]["matched"], 2)
        self.assertEqual(result["summary"]["before_only"], 1)
        self.assertEqual(result["summary"]["after_only"], 1)
        self.assertEqual(result["summary"]["ns_time"]["count"], 1)
        self.assertEqual(result["summary"]["ns_time"]["median_ratio"], 3)
        self.assertIsNone(self.metric(result, "BenchmarkFixture", "allocs/op", "cache")["ratio"])

    def test_invalid_csv_fails_without_a_partial_result(self):
        valid = ["reference", "cache", "BenchmarkFixture", "ns/op", 1, 2, 3, 4, 5, 3]
        for rows, message in (([valid, valid], "duplicate metric"),
                              ([valid[:-1] + [5]], "inconsistent retained median"),
                              ([valid[:-2] + ["nan", 3]], "invalid nonnegative"),
                              ([valid[:-1]], "malformed retained CSV row")):
            with self.subTest(message=message):
                path = self.write_csv("invalid.csv", rows)
                completed = self.invoke(path, FINAL)
                self.assertEqual(completed.returncode, 1)
                self.assertIn(message, completed.stderr)
                self.assertEqual(completed.stdout, "")

    def test_invalid_cname_limit_rules_fail_without_a_partial_result(self):
        rules = self.directory / "limits.txt"
        for case, text in (
            ("no matching rule", "limit ^BenchmarkLookup100k/ p50-ns 2000\n"),
            ("overlapping rules", "limit ^BenchmarkLookup100k/ p99-ns 2000\nlimit cname p99-ns 1500\n"),
            ("zero limit", "limit ^BenchmarkLookup100k/ p99-ns 0\n"),
        ):
            with self.subTest(case=case):
                rules.write_text(text, encoding="utf-8")
                completed = self.invoke(FINAL, FINAL, "--limits", rules)
                self.assertEqual(completed.returncode, 1)
                self.assertIn("expected one positive limit for BenchmarkLookup100k/cname p99-ns", completed.stderr)
                self.assertEqual(completed.stdout, "")

    def test_headroom_requires_cname_p99_in_both_recordings(self):
        missing = self.write_csv("missing-cname-p99.csv", [
            ["reference", "blocking-zone", CNAME, "p50-ns", 95, 97, 100, 103, 105, 100],
        ])
        for before, after in ((missing, FINAL), (FINAL, missing)):
            with self.subTest(before=before.name, after=after.name):
                completed = self.invoke(before, after, "--limits", LIMITS)
                self.assertEqual(completed.returncode, 1)
                self.assertIn("CNAME p99 is missing from one or both recordings", completed.stderr)
                self.assertEqual(completed.stdout, "")

    def test_historical_archive_rejects_candidate_role(self):
        for before, after, option in ((ARCHIVE, FINAL, "--before-role"), (FINAL, ARCHIVE, "--after-role")):
            with self.subTest(option=option):
                completed = self.invoke(before, after, option, "candidate")
                self.assertEqual(completed.returncode, 1)
                self.assertIn("historical .tar.gz with role reference", completed.stderr)
                self.assertEqual(completed.stdout, "")

    def write_archive(self, members):
        path = self.directory / "fixture.tar.gz"
        with tarfile.open(path, "w:gz") as archive:
            for name, payload in members.items():
                member = tarfile.TarInfo(name)
                if payload is None:
                    member.type = tarfile.SYMTYPE
                    member.linkname = str(FINAL)
                    archive.addfile(member)
                else:
                    member.size = len(payload)
                    archive.addfile(member, io.BytesIO(payload))
        return path

    def test_archive_extracts_only_reports_into_cleaned_scratch(self):
        row = b"BenchmarkFixture-6 1000 15 ns/op\n"
        archive = self.write_archive({"calibration/blocking-zone.txt": row * 5,
                                      "calibration/cache.txt": row * 5, "calibration/upstream.txt": row * 5,
                                      "../../escaped.txt": b"must not extract"})
        result = self.comparison(archive, archive)
        self.assertEqual(result["summary"]["ns_time"]["count"], 3)
        self.assertEqual(set(self.directory.iterdir()), {archive, self.scratch})

    def test_archive_rejects_unsafe_and_incomplete_reports(self):
        row = b"BenchmarkFixture-6 1000 15 ns/op\n"
        for name, payload, message in (("../blocking-zone.txt", row * 5, "unsafe"),
                                       ("calibration/blocking-zone.txt", None, "unsafe"),
                                       ("calibration/blocking-zone.txt", row * 4, "expected five samples")):
            with self.subTest(name=name, message=message):
                archive = self.write_archive({name: payload, "calibration/cache.txt": row * 5,
                                              "calibration/upstream.txt": row * 5})
                completed = self.invoke(archive, FINAL)
                self.assertEqual(completed.returncode, 1)
                self.assertIn(message, completed.stderr)
                self.assertEqual(completed.stdout, "")
                self.assertEqual(list(self.scratch.iterdir()), [])
