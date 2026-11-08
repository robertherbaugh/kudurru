#!/usr/bin/env python3
"""Collect five fresh processes per release benchmark suite, retaining failures.

Recording creates calibration evidence only. Comparison additionally requires
a separately reviewed baseline directory from the same named runner class.
Neither mode changes a committed baseline or chooses new absolute limits.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import subprocess
import sys
import time


SAMPLES = 5
GIB = 1024 ** 3
# Keep original corpora, durations and benchmark implementations. Separate
# reports prevent the blocking helper's ignore rules from hiding cache metrics.
SUITES = {
    "blocking": ("./internal/blocking/...", ".", "1s"),
    "zone": ("./internal/zone", "^BenchmarkLookup100k$", "1s"),
    "cache": ("./internal/cache", "^Benchmark(Get|MakeKey|Set|Memory|Growth)$", "1s"),
    "upstream": ("./internal/upstream", "^BenchmarkStrategies$", "1s"),
    "udp": ("./internal/dnsserver", "^BenchmarkUDPSockets$", "5s"),
    "dot": ("./internal/dnsserver", "^BenchmarkDoTSockets$", "1s"),
    "quic": ("./internal/dnsserver", "^BenchmarkQUICSockets$", "1s"),
    "persist": ("./internal/cache/persist", "^BenchmarkPersist200k$", "1s"),
}
REPORTS = {"blocking-zone": ("blocking", "zone"),
           **{name: (name,) for name in SUITES if name not in ("blocking", "zone")}}
LIMIT_FILES = {"blocking-zone": "bench/limits.txt", "cache": "bench/cache-limits.txt",
               "upstream": "bench/upstream-limits.txt"}
WORKLOAD_ROOTS = ("internal/blocking/", "internal/zone/", "internal/cache/",
                  "internal/upstream/", "internal/dnsserver/")


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def save(path, value):
    """Persist the numeric child outcome before parsing or source enrichment."""
    temporary = path.with_suffix(".tmp")
    with temporary.open("x") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)
    directory = os.open(path.parent, os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def free_bytes():
    stat = os.statvfs(".")
    return stat.f_frsize * stat.f_bavail


def group_exists(pid):
    try:
        os.killpg(pid, 0)
        return True
    except ProcessLookupError:
        return False


def signal_group(pid, sig):
    try:
        os.killpg(pid, sig)
    except ProcessLookupError:
        pass


def measure(argv, stem, procs, timeout=1860):
    log, receipt = stem.with_suffix(".log"), stem.with_suffix(".json")
    if log.exists() or receipt.exists():
        raise ValueError("measurement output already exists")
    if free_bytes() <= 3 * GIB:
        raise ValueError("measurement requires more than 3 GiB free")
    env = dict(os.environ, GOMAXPROCS=str(procs), GOFLAGS="-p=2", GOGC="100", GOMEMLIMIT="off")
    env.pop("GORACE", None)
    started = time.monotonic()
    result = {"argv": argv, "child_exit_code": None, "stop_reason": None,
              "minimum_free_bytes": free_bytes()}
    save(receipt, result)
    with log.open("xb") as output:
        process = subprocess.Popen(argv, stdout=output, stderr=subprocess.STDOUT,
                                   env=env, start_new_session=True)
        result["pid"] = process.pid
        save(receipt, result)
        while process.poll() is None:
            result["minimum_free_bytes"] = min(result["minimum_free_bytes"], free_bytes())
            if result["minimum_free_bytes"] <= 2 * GIB:
                result["stop_reason"] = "disk_floor"
            elif time.monotonic() - started >= timeout:
                result["stop_reason"] = "timeout"
            elif log.stat().st_size >= 2 * 1024 ** 2:
                result["stop_reason"] = "output_bound"
            if result["stop_reason"]:
                signal_group(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    signal_group(process.pid, signal.SIGKILL)
                break
            time.sleep(0.2)
        result.update(child_exit_code=process.wait(), wall_seconds=time.monotonic() - started,
                      post_free_bytes=free_bytes(), output_bytes=log.stat().st_size)
        result["minimum_free_bytes"] = min(result["minimum_free_bytes"], result["post_free_bytes"])
        save(receipt, result)
    # An unexpected descendant prevents the next measurement from starting.
    # The job retains its receipt and fails; it never retries that sample.
    if group_exists(process.pid):
        result["group_reaped"] = False
        save(receipt, result)
        signal_group(process.pid, signal.SIGTERM)
        limit = time.monotonic() + 5
        while group_exists(process.pid) and time.monotonic() < limit:
            time.sleep(0.1)
        if group_exists(process.pid):
            signal_group(process.pid, signal.SIGKILL)
        raise ValueError("measurement left descendants; remaining sequence held")
    result["group_reaped"] = True
    save(receipt, result)
    return result


def commands():
    return {name: ["go", "test", "-run", "^$", "-bench", pattern, "-benchmem",
                   "-benchtime", duration, "-count", "1", "-timeout", "30m", package]
            for name, (package, pattern, duration) in SUITES.items()}


def source_identity():
    files = subprocess.check_output(["git", "ls-files", "-z"]).decode().split("\0")
    return {name: digest(name) for name in files if name}


def validate_baseline(path, plan):
    baseline = json.loads((path / "plan.json").read_text())
    for key in ("runner_class", "procs", "samples", "commands", "platform", "go_version", "limits_sha256", "scope_limits_sha256", "workload_inputs"):
        if baseline[key] != plan[key]:
            raise ValueError("baseline differs in " + key)
    outcome = json.loads((path / "result.json").read_text())
    if not outcome["measurement_complete"] or outcome["exit_code"] != 0:
        raise ValueError("baseline measurements or gates failed")
    if set(outcome["reports"]) != set(REPORTS):
        raise ValueError("baseline suite inventory is incomplete")
    for name, expected in outcome["reports"].items():
        if digest(path / (name + ".txt")) != expected:
            raise ValueError("baseline report changed: " + name)
    return {name: digest(path / name) for name in
            ["plan.json", "result.json", *[name + ".txt" for name in REPORTS]]}


def execute(output, runner_class, procs, baseline=None):
    if any(os.environ.get(name) for name in ("KUDURRU_BLOCKING_BENCH_LISTS", "KUDURRU_BLOCKING_MEMORY_HELPER",
                                           "KUDURRU_BLOCKING_MEMORY_HELPER_SUFFIX_FIRST")):
        raise ValueError("external benchmark corpus override is not allowed")
    if subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=all"]):
        raise ValueError("release measurements require a clean committed checkout")
    source = source_identity()
    plan = {"runner_class": runner_class, "procs": procs, "samples": SAMPLES,
            "commands": commands(), "platform": [platform.system(), platform.machine()],
            "head": subprocess.check_output(["git", "rev-parse", "HEAD"]).decode().strip(),
            "go_version": subprocess.check_output(["go", "version"]).decode().strip(),
            "limits_sha256": digest("bench/limits.txt"),
            "scope_limits_sha256": {name: digest(path) for name, path in LIMIT_FILES.items()},
            "workload_inputs": {name: value for name, value in source.items()
                                if name.startswith(WORKLOAD_ROOTS) and
                                (name.endswith("_test.go") or "/testdata/" in name or "/internal/synth/" in name)}}
    if baseline is not None:
        baseline_hashes = validate_baseline(baseline, plan)
    output.mkdir(parents=True, exist_ok=False)
    save(output / "plan.json", plan)
    save(output / "source.json", source)
    result = {"mode": "compare" if baseline else "record", "measurement_complete": False,
              "release_qualified": False, "exit_code": 1, "reports": {}}
    save(output / "result.json", result)
    failed = False
    outcomes = {}
    for number in range(1, SAMPLES + 1):
        for name, argv in plan["commands"].items():
            if source_identity() != source:
                raise ValueError("source changed; remaining sequence held")
            label = f"{name}-{number}"
            print("START", label, flush=True)
            receipt = measure(argv, output / label, procs)
            outcomes[label] = receipt["child_exit_code"]
            print("COMPLETE", label, receipt["child_exit_code"], flush=True)
            failed |= receipt["child_exit_code"] != 0
            if receipt["stop_reason"]:
                raise ValueError("resource stop; remaining sequence held")
    if source_identity() != source:
        raise ValueError("source changed during measurement")
    for report, suites in REPORTS.items():
        path = output / (report + ".txt")
        with path.open("x") as stream:
            for number in range(1, SAMPLES + 1):
                for suite in suites:
                    label = f"{suite}-{number}"
                    stream.write((output / (label + ".log")).read_text())
                    if outcomes[label] != 0:
                        stream.write("\nFAIL: go test exited non-zero\n")
        result["reports"][report] = digest(path)
        argv = ["go", "run", "./hack/benchgate", "-samples", str(SAMPLES),
                "-procs", str(procs), "-strict-cpu", "-threshold", "20"]
        if report in LIMIT_FILES:
            argv += ["-limits", LIMIT_FILES[report]]
        if baseline is not None:
            if any(digest(baseline / name) != expected for name, expected in baseline_hashes.items()):
                raise ValueError("baseline changed during measurement")
            argv += ["-baseline", str(baseline / (report + ".txt"))]
        receipt = measure([*argv, str(path)], output / (report + "-gate"), procs, 300)
        failed |= receipt["child_exit_code"] != 0 or receipt["stop_reason"] is not None
        if receipt["stop_reason"]:
            raise ValueError("resource stop; remaining comparisons held")
    if source_identity() != source:
        raise ValueError("source changed during comparison")
    if baseline is not None and any(digest(baseline / name) != expected for name, expected in baseline_hashes.items()):
        raise ValueError("baseline changed during comparison")
    result.update(measurement_complete=True, exit_code=int(failed))
    # Runner labels and a passed relative gate do not calibrate the additional
    # socket/persistence absolute limits or establish other release requirements.
    save(output / "result.json", result)
    return int(failed)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--runner-class", required=True)
    parser.add_argument("--procs", required=True, type=int)
    parser.add_argument("--baseline", type=Path)
    args = parser.parse_args()
    if not args.runner_class.strip() or not 1 <= args.procs <= 64:
        parser.error("a named runner class and 1..64 processors are required")
    try:
        return execute(args.output.resolve(), args.runner_class, args.procs,
                       args.baseline.resolve() if args.baseline else None)
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print("Release benchmark collection failed:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
