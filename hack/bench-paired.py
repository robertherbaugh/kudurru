#!/usr/bin/env python3
"""Measure the immutable reference and candidate on one VM.

The selected runner pool contains different CPUs. A single invocation keeps
both recordings on one host without selecting a CPU or replacing any sample.
A failed reference remains ineligible for comparison; nothing resets a baseline.
"""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys


spec = importlib.util.spec_from_file_location("bench_reference", Path(__file__).with_name("bench-reference.py"))
reference = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reference)
runner = reference.runner


def verify_recording(directory, reports, commands):
    """Check complete immutable evidence before admitting it to comparison."""
    outcome = json.loads((directory / "result.json").read_text())
    if not outcome["measurement_complete"] or set(outcome["reports"]) != set(reports):
        raise ValueError("recording is incomplete")
    identities = {}
    for name, expected in outcome["reports"].items():
        path = directory / (name + ".txt")
        if runner.digest(path) != expected:
            raise ValueError("recording report changed: " + name)
        identities[path] = expected
    successful = outcome["exit_code"] == 0
    names = [f"{name}-{i}" for i in range(1, runner.SAMPLES + 1) for name in commands]
    names += [name + "-gate" for name in reports]
    for name in names:
        path = directory / (name + ".json")
        receipt = json.loads(path.read_text())
        if receipt["stop_reason"] is not None or not receipt["group_reaped"]:
            raise ValueError("recording stopped or left descendants: " + name)
        successful &= receipt["child_exit_code"] == 0
        identities[path] = runner.digest(path)
    for name in ("plan.json", "result.json", "source.json"):
        path = directory / name
        identities[path] = runner.digest(path)
    return successful, identities


def interleaved_sequence():
    """D412: adjacent matched scopes, reversing first source each sample."""
    sequence = []
    for number in range(1, runner.SAMPLES + 1):
        for index, scope in enumerate(reference.SCOPES):
            sources = ("reference", "candidate") if (number + index) % 2 else ("candidate", "reference")
            sequence.extend((source, scope, number) for source in sources)
    sequence.extend(("candidate", scope, number)
                    for number in range(1, runner.SAMPLES + 1)
                    for scope in runner.SUITES if scope not in reference.SCOPES)
    return sequence


def host_provenance():
    cpuinfo = Path("/proc/cpuinfo")
    return {"allowed_cpus": sorted(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else None,
            "cpuinfo": cpuinfo.read_text() if cpuinfo.exists() else None}


def record_interleaved(checkout, output, control_head, original_source, control_source, unchanged):
    """Use the original native commands and strict gates in D412 order."""
    if any(os.environ.get(name) for name in ("KUDURRU_BLOCKING_BENCH_LISTS", "KUDURRU_BLOCKING_MEMORY_HELPER",
                                           "KUDURRU_BLOCKING_MEMORY_HELPER_SUFFIX_FIRST")):
        raise ValueError("external benchmark corpus override is not allowed")
    with reference.at(reference.CONTROL):
        version = subprocess.check_output(["go", "version"]).decode().strip()
    with reference.at(checkout):
        if subprocess.check_output(["go", "version"]).decode().strip() != version:
            raise ValueError("reference and control toolchains differ")
    commands = runner.commands()
    recordings = {}
    identities = {}

    def retained():
        if any(runner.digest(path) != value for path, value in identities.items()):
            raise ValueError("recording changed during interleaved sequence")

    def remember(*paths):
        for path in paths:
            identities[path] = runner.digest(path)

    for source, root, head, manifest, reports, scopes in (
            ("reference", checkout, reference.REFERENCE, original_source, reference.REPORTS, reference.SCOPES),
            ("candidate", reference.CONTROL, control_head, control_source, runner.REPORTS, runner.SUITES)):
        directory = output / source
        directory.mkdir()
        plan = {"head": head, "control_head": control_head, "order": "interleaved-v1",
                "runner_class": reference.RUNNER_CLASS, "procs": reference.PROCS, "samples": runner.SAMPLES,
                "go_version": version, "commands": {name: commands[name] for name in scopes},
                "limits_sha256": runner.digest(reference.CONTROL / "bench/limits.txt"),
                "scope_limits_sha256": {name: runner.digest(reference.CONTROL / path)
                                        for name, path in runner.LIMIT_FILES.items()}}
        result = {"mode": source + "-interleaved", "measurement_complete": False,
                  "release_qualified": False, "exit_code": 1, "reports": {}}
        runner.save(directory / "plan.json", plan)
        runner.save(directory / "source.json", manifest)
        remember(directory / "plan.json", directory / "source.json")
        recordings[source] = {"root": root, "directory": directory, "reports": reports,
                              "result": result, "outcomes": {}, "failed": False}
        runner.save(directory / "result.json", result)
    for source, scope, number in interleaved_sequence():
        unchanged()
        retained()
        recording = recordings[source]
        label = f"{scope}-{number}"
        print("START", source, label, flush=True)
        with reference.at(recording["root"]):
            receipt = runner.measure(commands[scope], recording["directory"] / label, reference.PROCS)
        recording["outcomes"][label] = receipt["child_exit_code"]
        recording["failed"] |= receipt["child_exit_code"] != 0
        print("COMPLETE", source, label, receipt["child_exit_code"], flush=True)
        if receipt["stop_reason"] is not None or not receipt["group_reaped"]:
            raise ValueError("recording stopped or left descendants; remaining sequence held")
        retained()
        remember(recording["directory"] / (label + ".log"), recording["directory"] / (label + ".json"))
    for recording in recordings.values():
        for name, scopes in recording["reports"].items():
            unchanged()
            retained()
            path = recording["directory"] / (name + ".txt")
            with path.open("x") as stream:
                for number in range(1, runner.SAMPLES + 1):
                    for scope in scopes:
                        label = f"{scope}-{number}"
                        stream.write((recording["directory"] / (label + ".log")).read_text())
                        if recording["outcomes"][label] != 0:
                            stream.write("\nFAIL: go test exited non-zero\n")
            recording["result"]["reports"][name] = runner.digest(path)
            remember(path)
            argv = ["go", "run", "./hack/benchgate", "-samples", str(runner.SAMPLES),
                    "-procs", str(reference.PROCS), "-strict-cpu", "-threshold", "20",
                    "-limits", runner.LIMIT_FILES[name], str(path)]
            with reference.at(reference.CONTROL):
                receipt = runner.measure(argv, recording["directory"] / (name + "-gate"), reference.PROCS, 300)
            recording["failed"] |= receipt["child_exit_code"] != 0
            if receipt["stop_reason"] is not None or not receipt["group_reaped"]:
                raise ValueError("recording stopped or left descendants; remaining checks held")
            retained()
            remember(recording["directory"] / (name + "-gate.log"), recording["directory"] / (name + "-gate.json"))
        unchanged()
        recording["result"].update(measurement_complete=True, exit_code=int(recording["failed"]))
        runner.save(recording["directory"] / "result.json", recording["result"])
        remember(recording["directory"] / "result.json")
    retained()
    return tuple(recordings[source]["result"]["exit_code"] for source in ("reference", "candidate")), identities


def execute(checkout, output, order="sequential"):
    if order not in ("sequential", "interleaved"):
        raise ValueError("unknown paired recording order")
    if checkout == reference.CONTROL or reference.checkout_head(checkout) != reference.REFERENCE:
        raise ValueError("reference must be the exact original baseline commit")
    control_head = reference.checkout_head(reference.CONTROL)
    if output == checkout or checkout in output.parents:
        raise ValueError("artifacts must be outside the historical checkout")
    with reference.at(reference.CONTROL):
        control_source = runner.source_identity()
    with reference.at(checkout):
        original_source = runner.source_identity()
    output.mkdir(parents=True, exist_ok=False)
    result = {"mode": "paired", "measurement_complete": False, "release_qualified": False,
              "exit_code": 1, "reference_exit": None, "candidate_exit": None,
              "comparison_eligible": False, "comparisons": {}}
    runner.save(output / "result.json", result)
    runner.save(output / "plan.json", {"reference": reference.REFERENCE, "candidate": control_head,
                "runner_class": reference.RUNNER_CLASS, "procs": reference.PROCS,
                "samples": runner.SAMPLES, "order": order,
                "sequence": interleaved_sequence() if order == "interleaved" else ["reference", "candidate", "comparison"],
                "host": host_provenance(),
                "reference_source": original_source, "candidate_source": control_source})

    def unchanged():
        for path, head, source in ((checkout, reference.REFERENCE, original_source),
                                  (reference.CONTROL, control_head, control_source)):
            with reference.at(path):
                if reference.checkout_head(path) != head or runner.source_identity() != source:
                    raise ValueError("paired source changed; remaining sequence held")

    def retained(identities):
        if any(runner.digest(path) != value for path, value in identities.items()):
            raise ValueError("recording changed during paired sequence")

    with reference.at(reference.CONTROL):
        if order == "interleaved":
            exits, native_identities = record_interleaved(
                checkout, output, control_head, original_source, control_source, unchanged)
            result["reference_exit"], result["candidate_exit"] = exits
            eligible, identities = verify_recording(output / "reference", reference.REPORTS, reference.SCOPES)
            retained(native_identities)
            identities.update(native_identities)
        else:
            result["reference_exit"] = reference.execute(checkout, output / "reference")
            runner.save(output / "result.json", result)
            unchanged()
            eligible, identities = verify_recording(output / "reference", reference.REPORTS, reference.SCOPES)
            result["candidate_exit"] = runner.execute(output / "candidate", reference.RUNNER_CLASS, reference.PROCS)
        runner.save(output / "result.json", result)
        unchanged()
        retained(identities)
        candidate_ok, candidate_identities = verify_recording(output / "candidate", runner.REPORTS, runner.SUITES)
        identities.update(candidate_identities)
        result["comparison_eligible"] = eligible and result["reference_exit"] == 0
        failed = result["reference_exit"] != 0 or result["candidate_exit"] != 0 or not candidate_ok
        if result["comparison_eligible"]:
            for name in reference.REPORTS:
                unchanged()
                retained(identities)
                argv = ["go", "run", "./hack/benchgate", "-samples", str(runner.SAMPLES),
                        "-procs", str(reference.PROCS), "-strict-cpu", "-threshold", "20"]
                if name in runner.LIMIT_FILES:
                    argv += ["-limits", runner.LIMIT_FILES[name]]
                argv += ["-baseline", str(output / "reference" / (name + ".txt")),
                         str(output / "candidate" / (name + ".txt"))]
                receipt = runner.measure(argv, output / (name + "-compare"), reference.PROCS, 300)
                result["comparisons"][name] = receipt["child_exit_code"]
                runner.save(output / "result.json", result)
                failed |= receipt["child_exit_code"] != 0
                if receipt["stop_reason"] is not None or not receipt["group_reaped"]:
                    raise ValueError("comparison stopped or left descendants; remaining sequence held")
        else:
            failed = True
            result["comparison_refusal"] = "historical reference measurements or gates failed"
        unchanged()
        retained(identities)
    result.update(measurement_complete=True, exit_code=int(failed))
    runner.save(output / "result.json", result)
    return int(failed)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--checkout", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--order", choices=("sequential", "interleaved"), default="sequential")
    args = parser.parse_args()
    try:
        return execute(args.checkout.resolve(), args.output.resolve(), args.order)
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print("Paired benchmark measurement failed:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
