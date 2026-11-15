#!/usr/bin/env python3
"""Collect bounded upstream diagnostics; never qualify a release or replace a run."""

import argparse
import importlib.util
import os
from pathlib import Path
import subprocess
import sys

spec = importlib.util.spec_from_file_location("bench_reference", Path(__file__).with_name("bench-reference.py"))
reference = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reference)
runner = reference.runner


def host_state():
    """Retain counters rather than inferring a cause from a CPU model."""
    paths = ["/proc/cpuinfo", "/proc/stat", "/proc/loadavg", "/proc/pressure/cpu", "/proc/self/cgroup",
             "/sys/fs/cgroup/cpu.stat", "/sys/fs/cgroup/cpu.max", "/sys/fs/cgroup/cpu.pressure"]
    group = Path("/proc/self/cgroup")
    if group.exists():
        for line in group.read_text().splitlines():
            if line.startswith("0::"):
                parent = Path("/sys/fs/cgroup") / line[3:].lstrip("/")
                paths.extend(str(parent / name) for name in ("cpu.stat", "cpu.max", "cpu.pressure"))
    state = {}
    for name in paths:
        try:
            state[name] = Path(name).read_text()
        except OSError as error:
            state[name] = {"unavailable": type(error).__name__}
    state["allowed_cpus"] = sorted(os.sched_getaffinity(0))
    return state


def sequence(output):
    argv = ["go", "test", "./internal/upstream", "-run", "^$", "-bench",
            "^BenchmarkStrategies/(fast|slow-first)/(strict|rtt_weighted)$",
            "-count=1", "-benchtime=1s", "-timeout=10m"]
    commands = []
    for label, source in (("reference-1", "reference"), ("candidate-1", "candidate"),
                          ("candidate-2", "candidate"), ("reference-2", "reference"),
                          ("reference-trace", "reference"), ("candidate-trace", "candidate")):
        command = [*argv]
        if label.endswith("trace"):
            command += ["-trace", str(output / (label + ".trace"))]
        commands.append({"label": label, "source": source, "argv": command})
    return commands


def execute(checkout, output):
    if checkout == reference.CONTROL or reference.checkout_head(checkout) != reference.REFERENCE:
        raise ValueError("diagnostic requires the immutable historical reference")
    if output == checkout or checkout in output.parents:
        raise ValueError("output must be outside the historical checkout")
    heads = {"reference": reference.REFERENCE, "candidate": reference.checkout_head(reference.CONTROL)}
    roots = {"reference": checkout, "candidate": reference.CONTROL}
    sources, versions = {}, {}
    for name, root in roots.items():
        with reference.at(root):
            sources[name] = runner.source_identity()
            versions[name] = subprocess.check_output(["go", "version"]).decode().strip()
    if versions["reference"] != versions["candidate"]:
        raise ValueError("diagnostic toolchains differ")
    output.mkdir(parents=True, exist_ok=False)
    plan = {"kind": "upstream-diagnostic-v1", "release_qualified": False, "qualification_gates": [],
            "runner_class": reference.RUNNER_CLASS, "procs": reference.PROCS, "heads": heads,
            "go_versions": versions, "commands": sequence(output), "host_before": host_state(),
            "purpose": "Investigate failed D412 upstream timings. Two unprofiled processes per source and one trace each; no retries, selection, gates, or baseline changes. Trace overhead is not qualification evidence."}
    runner.save(output / "plan.json", plan)
    for name, source in sources.items():
        runner.save(output / (name + "-source.json"), source)
    result = {"kind": plan["kind"], "release_qualified": False, "diagnostic_complete": False,
              "exit_code": 1, "commands": {}}
    runner.save(output / "result.json", result)

    def unchanged():
        for name, root in roots.items():
            with reference.at(root):
                if runner.source_identity() != sources[name] or reference.checkout_head(root) != heads[name]:
                    raise ValueError("source changed; remaining diagnostics held")

    failed = False
    for item in plan["commands"]:
        unchanged()
        label = item["label"]
        runner.save(output / (label + "-host-before.json"), host_state())
        print("START", label, flush=True)
        with reference.at(roots[item["source"]]):
            receipt = runner.measure(item["argv"], output / label, reference.PROCS, 660)
        runner.save(output / (label + "-host-after.json"), host_state())
        result["commands"][label] = receipt["child_exit_code"]
        runner.save(output / "result.json", result)
        print("COMPLETE", label, receipt["child_exit_code"], flush=True)
        failed |= receipt["child_exit_code"] != 0
        if receipt["stop_reason"]:
            raise ValueError("resource stop; remaining diagnostics held")
    unchanged()
    result.update(diagnostic_complete=True, exit_code=int(failed), host_after=host_state())
    # Hash every raw receipt/log/trace and both source manifests. No baseline is read.
    result["artifacts"] = {p.name: runner.digest(p) for p in output.iterdir() if p.is_file() and p.name != "result.json"}
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
        print("Upstream diagnostic failed:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
