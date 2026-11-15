#!/usr/bin/env python3
"""Measure the original benchmark reference on the selected release runner.

This never replaces bench/baseline.txt or makes the candidate its own baseline.
The historical checkout supplies unchanged production and benchmark sources;
the current checkout supplies only process controls and the strict Go gate.
"""

import argparse
from contextlib import contextmanager
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys


REFERENCE = "0cc9565e9a878f8223fc1cee5201eb7eca8aacd2"
RUNNER_CLASS = "ubicloud-standard-8-ubuntu-2404"
PROCS = 6
SCOPES = ("blocking", "zone", "cache", "upstream")
REPORTS = {"blocking-zone": ("blocking", "zone"),
           "cache": ("cache",), "upstream": ("upstream",)}
CONTROL = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("bench_release", CONTROL / "hack/bench-release.py")
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


@contextmanager
def at(path):
    previous = Path.cwd()
    os.chdir(path)
    try:
        yield
    finally:
        os.chdir(previous)


def checkout_head(path):
    with at(path):
        if subprocess.check_output(["git", "status", "--porcelain", "--untracked-files=all"]):
            raise ValueError("reference and control checkouts must be clean")
        return subprocess.check_output(["git", "rev-parse", "HEAD"]).decode().strip()


def execute(reference, output):
    if reference == CONTROL or checkout_head(reference) != REFERENCE:
        raise ValueError("reference must be the exact original baseline commit")
    control_head = checkout_head(CONTROL)
    if output == reference or reference in output.parents:
        raise ValueError("artifacts must be outside the historical checkout")
    if any(os.environ.get(name) for name in ("KUDURRU_BLOCKING_BENCH_LISTS", "KUDURRU_BLOCKING_MEMORY_HELPER",
                                           "KUDURRU_BLOCKING_MEMORY_HELPER_SUFFIX_FIRST")):
        raise ValueError("external benchmark corpus override is not allowed")
    with at(CONTROL):
        control_source = runner.source_identity()
        version = subprocess.check_output(["go", "version"]).decode().strip()
    with at(reference):
        source = runner.source_identity()
        if subprocess.check_output(["go", "version"]).decode().strip() != version:
            raise ValueError("reference and control toolchains differ")
    commands = {name: argv for name, argv in runner.commands().items() if name in SCOPES}
    plan = {"head": REFERENCE, "control_head": control_head,
            "runner_class": RUNNER_CLASS, "procs": PROCS, "samples": runner.SAMPLES,
            "go_version": version, "commands": commands,
            "limits_sha256": runner.digest(CONTROL / "bench/limits.txt"),
            "scope_limits_sha256": {name: runner.digest(CONTROL / path) for name, path in runner.LIMIT_FILES.items()}}
    output.mkdir(parents=True, exist_ok=False)
    runner.save(output / "plan.json", plan)
    runner.save(output / "source.json", source)
    runner.save(output / "control-source.json", control_source)
    result = {"mode": "historical-reference", "measurement_complete": False,
              "release_qualified": False, "exit_code": 1, "reports": {}}
    runner.save(output / "result.json", result)

    def unchanged():
        with at(reference):
            if runner.source_identity() != source or checkout_head(reference) != REFERENCE:
                raise ValueError("historical source changed; remaining sequence held")
        with at(CONTROL):
            if runner.source_identity() != control_source or checkout_head(CONTROL) != control_head:
                raise ValueError("measurement control changed; remaining sequence held")

    outcomes = {}
    failed = False
    for number in range(1, runner.SAMPLES + 1):
        for name, argv in commands.items():
            unchanged()
            label = f"{name}-{number}"
            print("START", label, flush=True)
            with at(reference):
                receipt = runner.measure(argv, output / label, PROCS)
            outcomes[label] = receipt["child_exit_code"]
            failed |= receipt["child_exit_code"] != 0
            print("COMPLETE", label, receipt["child_exit_code"], flush=True)
            if receipt["stop_reason"]:
                raise ValueError("resource stop; remaining reference sequence held")
    unchanged()
    for report, scopes in REPORTS.items():
        path = output / (report + ".txt")
        with path.open("x") as stream:
            for number in range(1, runner.SAMPLES + 1):
                for scope in scopes:
                    label = f"{scope}-{number}"
                    stream.write((output / (label + ".log")).read_text())
                    if outcomes[label] != 0:
                        stream.write("\nFAIL: go test exited non-zero\n")
        result["reports"][report] = runner.digest(path)
        argv = ["go", "run", "./hack/benchgate", "-samples", str(runner.SAMPLES),
                "-procs", str(PROCS), "-strict-cpu", "-threshold", "20"]
        if report in runner.LIMIT_FILES:
            argv += ["-limits", runner.LIMIT_FILES[report]]
        with at(CONTROL):
            receipt = runner.measure([*argv, str(path)], output / (report + "-gate"), PROCS, 300)
        failed |= receipt["child_exit_code"] != 0 or receipt["stop_reason"] is not None
        if receipt["stop_reason"]:
            raise ValueError("resource stop; remaining reference checks held")
    unchanged()
    result.update(measurement_complete=True, exit_code=int(failed))
    runner.save(output / "result.json", result)
    return int(failed)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--checkout", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    try:
        return execute(args.checkout.resolve(), args.output.resolve())
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print("Historical benchmark reference failed:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
