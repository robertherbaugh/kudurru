#!/usr/bin/env python3
"""Measure the immutable reference and candidate sequentially on one VM.

The selected runner pool contains different CPUs. A single invocation keeps
both recordings on one host without selecting a CPU or replacing any sample.
A failed reference remains ineligible for comparison; nothing resets a baseline.
"""

import argparse
import importlib.util
import json
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


def execute(checkout, output):
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
                "samples": runner.SAMPLES, "sequence": ["reference", "candidate", "comparison"],
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
    args = parser.parse_args()
    try:
        return execute(args.checkout.resolve(), args.output.resolve())
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print("Paired benchmark measurement failed:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
