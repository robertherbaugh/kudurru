#!/usr/bin/env python3
"""Compare retained benchmark medians offline; this is not a release gate."""

import argparse
import csv
import json
import math
from pathlib import Path, PurePosixPath
import re
import shutil
import statistics
import tarfile
import tempfile


SAMPLE_COLUMNS = tuple(f"sample_{number}" for number in range(1, 6))
CSV_COLUMNS = ("role", "report", "name", "unit", *SAMPLE_COLUMNS, "median")
HISTORICAL_REPORTS = ("blocking-zone", "cache", "upstream")
NS_TIME_UNITS = ("ns/op", "p50-ns", "p99-ns")
CNAME_P99 = ("blocking-zone", "BenchmarkLookup100k/cname", "p99-ns")


def number(value):
    parsed = float(value)
    if not math.isfinite(parsed) or parsed < 0:
        raise ValueError(f"invalid nonnegative metric value: {value!r}")
    return parsed


def read_csv(path, role):
    vectors = {}
    with path.open(newline="", encoding="utf-8") as source:
        rows = csv.DictReader(source, strict=True)
        if tuple(rows.fieldnames or ()) != CSV_COLUMNS:
            raise ValueError(f"{path}: unexpected retained CSV columns")
        for row in rows:
            if None in row or not all(row.values()) or row["role"] not in ("reference", "candidate"):
                raise ValueError(f"{path}:{rows.line_num}: malformed retained CSV row")
            if row["role"] != role:
                continue
            key = row["report"], row["name"], row["unit"]
            if key in vectors:
                raise ValueError(f"{path}:{rows.line_num}: duplicate metric {key}")
            samples = [number(row[column]) for column in SAMPLE_COLUMNS]
            if statistics.median(samples) != number(row["median"]):
                raise ValueError(f"{path}:{rows.line_num}: inconsistent retained median for {key}")
            vectors[key] = samples
    if not vectors:
        raise ValueError(f"{path}: no vectors for role {role}")
    return vectors


def read_native_report(path, report):
    vectors = {}
    with path.open(encoding="utf-8") as source:
        for line_number, line in enumerate(source, 1):
            if not line.startswith("Benchmark"):
                continue
            fields = line.split()
            match = re.fullmatch(r"(Benchmark\S+)-6", fields[0])
            if not match or len(fields) < 4 or len(fields) % 2 or not fields[1].isdigit():
                raise ValueError(f"{path.name}:{line_number}: malformed six-processor benchmark row")
            units = fields[3::2]
            if len(set(units)) != len(units):
                raise ValueError(f"{path.name}:{line_number}: duplicate benchmark unit")
            for value, unit in zip(fields[2::2], units):
                key = report, match[1], unit
                vectors.setdefault(key, []).append(number(value))
    if not vectors or any(len(samples) != len(SAMPLE_COLUMNS) for samples in vectors.values()):
        raise ValueError(f"{path.name}: expected five samples for every metric")
    return vectors


def read_historical(path):
    names = {f"{report}.txt" for report in HISTORICAL_REPORTS}
    vectors = {}
    with tempfile.TemporaryDirectory(prefix="bench-evidence-") as temporary, tarfile.open(path, "r:gz") as archive:
        directory = Path(temporary)
        members = {}
        parents = set()
        for member in archive:
            name = PurePosixPath(member.name)
            if name.name not in names:
                continue
            if not member.isfile() or name.is_absolute() or ".." in name.parts or name.name in members:
                raise ValueError(f"{path}: unsafe or duplicate report member {member.name!r}")
            members[name.name] = member
            parents.add(name.parent)
        if members.keys() != names or len(parents) != 1:
            raise ValueError(f"{path}: expected one set of historical reference reports in a single directory")
        for report in HISTORICAL_REPORTS:
            name = f"{report}.txt"
            extracted = directory / name
            with archive.extractfile(members[name]) as source, extracted.open("xb") as target:
                shutil.copyfileobj(source, target)
            vectors.update(read_native_report(extracted, report))
    return vectors


def read_recording(path, role):
    if path.suffix == ".csv":
        return read_csv(path, role)
    if path.name.endswith(".tar.gz") and role == "reference":
        return read_historical(path)
    raise ValueError(f"{path}: expected a retained CSV or a historical .tar.gz with role reference")


def cname_limit(path):
    _, benchmark, unit = CNAME_P99
    limits = []
    for line in path.read_text(encoding="utf-8").splitlines():
        fields = line.partition("#")[0].split()
        if len(fields) == 4 and fields[0] == "limit" and fields[2] == unit and re.search(fields[1], benchmark):
            limits.append(number(fields[3]))
    if len(limits) != 1 or limits[0] == 0:
        raise ValueError(f"{path}: expected one positive limit for {benchmark} {unit}")
    return limits[0]


def compare(before, after, limit=None):
    keys = before.keys() & after.keys()
    if not keys:
        raise ValueError("no matched metrics")
    rows = []
    ratios = []
    for report, name, unit in sorted(keys):
        key = report, name, unit
        old = statistics.median(before[key])
        new = statistics.median(after[key])
        ratio = new / old if old else None
        rows.append(dict(report=report, name=name, unit=unit, before_samples=before[key],
                         after_samples=after[key], before_median=old, after_median=new, ratio=ratio))
        if unit in NS_TIME_UNITS and ratio is not None:
            ratios.append(ratio)
    result = dict(release_qualified=False, metrics=rows, summary=dict(
        matched=len(keys), before_only=len(before) - len(keys), after_only=len(after) - len(keys),
        ns_time=dict(units=NS_TIME_UNITS, count=len(ratios),
                     median_ratio=statistics.median(ratios) if ratios else None,
                     min_ratio=min(ratios, default=None), max_ratio=max(ratios, default=None)),
    ))
    if limit is not None:
        if CNAME_P99 not in keys:
            raise ValueError("CNAME p99 is missing from one or both recordings")
        result["cname_p99_headroom"] = dict(limit_ns=limit)
        for label, samples in (("before", before[CNAME_P99]), ("after", after[CNAME_P99])):
            result["cname_p99_headroom"][label] = dict(
                headroom_percent=100 * (limit - statistics.median(samples)) / limit,
                min_sample_ns=min(samples), max_sample_ns=max(samples),
            )
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("before", type=Path, help="retained CSV or historical reference .tar.gz")
    parser.add_argument("after", type=Path, help="retained CSV or historical reference .tar.gz")
    for side in ("before", "after"):
        parser.add_argument(f"--{side}-role", choices=("reference", "candidate"), default="reference")
    parser.add_argument("--limits", type=Path, help="read CNAME p99 headroom limit from this rule file")
    args = parser.parse_args()
    try:
        result = compare(read_recording(args.before, args.before_role), read_recording(args.after, args.after_role),
                         cname_limit(args.limits) if args.limits else None)
        result["before"] = dict(path=str(args.before), role=args.before_role)
        result["after"] = dict(path=str(args.after), role=args.after_role)
        print(json.dumps(result, indent=2, allow_nan=False))
    except (OSError, EOFError, ValueError, csv.Error, tarfile.TarError, re.error) as error:
        parser.exit(1, f"{parser.prog}: {error}\n")


if __name__ == "__main__":
    main()
