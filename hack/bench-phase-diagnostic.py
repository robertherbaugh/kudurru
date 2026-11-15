#!/usr/bin/env python3
"""Temporary phase-aligned upstream diagnostics. Never qualify a release."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

spec = importlib.util.spec_from_file_location("reference", Path(__file__).with_name("bench-reference.py"))
reference = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reference)
runner = reference.runner

# Linux-only instrumentation in a Go overlay; both original checkouts stay
# unchanged. CPU receipts exclude compilation, warmups and fake-server cleanup.
HELPER = r'''
type diagnosticPhaseSnapshot struct {
    UnixNS int64 `json:"unix_ns"`
    UserUS int64 `json:"user_us"`
    SystemUS int64 `json:"system_us"`
    VoluntarySwitches int64 `json:"voluntary_switches"`
    InvoluntarySwitches int64 `json:"involuntary_switches"`
    ProcStat string `json:"proc_stat"`
    CPUStat string `json:"cgroup_cpu_stat"`
    Pressure string `json:"cpu_pressure"`
}

func diagnosticSnapshot(b *testing.B) diagnosticPhaseSnapshot {
    b.Helper()
    read := func(path string) string { data, err := os.ReadFile(path); if err != nil { return "unavailable" }; return string(data) }
    stat := read("/proc/stat")
    cpu := read("/sys/fs/cgroup/cpu.stat")
    pressure := read("/proc/pressure/cpu")
    var r syscall.Rusage
    if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err != nil { b.Fatal(err) }
    return diagnosticPhaseSnapshot{UnixNS:time.Now().UnixNano(),
        UserUS:r.Utime.Sec*1000000+r.Utime.Usec,
        SystemUS:r.Stime.Sec*1000000+r.Stime.Usec,
        VoluntarySwitches:r.Nvcsw, InvoluntarySwitches:r.Nivcsw,
        ProcStat:stat, CPUStat:cpu, Pressure:pressure}
}

func diagnosticPhaseStart(b *testing.B) func() {
    b.Helper()
    before := diagnosticSnapshot(b)
    return func() {
        after := diagnosticSnapshot(b)
        record := struct {
            Name string `json:"name"`
            Iterations int `json:"iterations"`
            Before diagnosticPhaseSnapshot `json:"before"`
            After diagnosticPhaseSnapshot `json:"after"`
        }{b.Name(),b.N,before,after}
        data,err := json.MarshalIndent(record,"","  "); if err != nil { b.Fatal(err) }
        if err := os.WriteFile(os.Getenv("KUDURRU_DIAGNOSTIC_PHASE_FILE"),data,0600); err != nil { b.Fatal(err) }
    }
}
'''


def overlay(root, destination):
    original = root / "internal/upstream/bench_test.go"
    source = original.read_text()
    replacements = [
        ('"context"', '"context"\n"encoding/json"\n"os"\n"runtime/trace"\n"syscall"'),
        ('ctx := context.Background()', 'ctx, phaseTask := trace.NewTask(context.Background(), b.Name())\n defer phaseTask.End()'),
        ('b.ReportAllocs()', 'b.ReportAllocs()\nphaseEnd := diagnosticPhaseStart(b)'),
        ('start := time.Now()', 'region := trace.StartRegion(ctx, "exchange")\nstart := time.Now()'),
        ('lat = append(lat, time.Since(start))', 'lat = append(lat, time.Since(start))\nregion.End()'),
        ('slices.Sort(lat)', 'phaseEnd()\nslices.Sort(lat)'),
    ]
    for old, new in replacements:
        if source.count(old) != 1:
            raise ValueError("unexpected benchmark instrumentation point: " + old)
        source = source.replace(old, new)
    instrumented = destination / "bench_test.go"
    instrumented.write_text(source + HELPER)
    subprocess.run(["gofmt", "-w", str(instrumented)], check=True)
    replacement = destination / "overlay.json"
    runner.save(replacement, {"Replace": {str(original): str(instrumented)}})
    return replacement


def sequence(output):
    # Adjacent source comparisons, alternating first source on the second pair.
    order = [("reference", "strict"), ("candidate", "strict"),
             ("reference", "rtt_weighted"), ("candidate", "rtt_weighted"),
             ("candidate", "strict"), ("reference", "strict"),
             ("candidate", "rtt_weighted"), ("reference", "rtt_weighted")]
    commands = []
    for number, (source, strategy) in enumerate(order, 1):
        label = f"{number:02}-{source}-{strategy}"
        commands.append({"label": label, "source": source,
            "phase": str(output / (label + "-phase.json")),
            "argv": ["env", "KUDURRU_DIAGNOSTIC_PHASE_FILE=" + str(output / (label + "-phase.json")),
                     str(output / source / "upstream.test"), "-test.run=^$",
                     "-test.bench=^BenchmarkStrategies/fast/" + strategy + "$",
                     "-test.benchtime=1s", "-test.count=1", "-test.timeout=10m",
                     "-test.trace=" + str(output / (label + ".trace"))]})
    return commands


def execute(checkout, output):
    if reference.checkout_head(checkout) != reference.REFERENCE or checkout == reference.CONTROL:
        raise ValueError("immutable original reference required")
    roots = {"reference": checkout, "candidate": reference.CONTROL}
    if output == checkout or checkout in output.parents:
        raise ValueError("diagnostic output must be outside historical source")
    identities, versions = {}, {}
    for name, root in roots.items():
        with reference.at(root):
            identities[name] = runner.source_identity()
            versions[name] = subprocess.check_output(["go", "version"], text=True).strip()
    if len(set(versions.values())) != 1 or "go1.27.4 " not in versions["candidate"]:
        raise ValueError("phase diagnostic requires identical Go 1.27.4 toolchains")
    output.mkdir(parents=True, exist_ok=False)
    commands = sequence(output)
    plan = {"kind": "phase-upstream-diagnostic-v1", "release_qualified": False,
            "qualification_gates": [], "runner_class": reference.RUNNER_CLASS, "procs": 6,
            "go_versions": versions, "allowed_cpus": sorted(os.sched_getaffinity(0)),
            "heads": {name: reference.checkout_head(root) for name, root in roots.items()},
            "commands": commands, "purpose": "Resolve compilation confounding in earlier host counters. Eight predeclared instrumented fast strict/RTT leaf processes, phase-only CPU/steal counters and per-exchange trace regions. No limits/baseline, no retries or outcome selection."}
    runner.save(output / "plan.json", plan)
    for name in ("/proc/cpuinfo", "/proc/self/cgroup", "/sys/fs/cgroup/cpu.max"):
        try:
            data = Path(name).read_text()
        except OSError as error:
            data = str(type(error).__name__)
        (output / (name.replace("/", "_") + ".txt")).write_text(data)
    result = {"release_qualified": False, "exit_code": 1, "complete": False, "commands": {}}
    runner.save(output / "result.json", result)

    def unchanged():
        for name, root in roots.items():
            with reference.at(root):
                if runner.source_identity() != identities[name] or reference.checkout_head(root) != plan["heads"][name]:
                    raise ValueError("source changed; remaining diagnostics held")

    for name, root in roots.items():
        runner.save(output / (name + "-source.json"), identities[name])
        destination = output / name
        destination.mkdir()
        replacement = overlay(root, destination)
        argv = ["go", "test", "-c", "-overlay=" + str(replacement),
                "-o", str(destination / "upstream.test"), "./internal/upstream"]
        with reference.at(root):
            receipt = runner.measure(argv, output / (name + "-build"), 6, 660)
        if receipt["child_exit_code"] or receipt["stop_reason"]:
            raise ValueError(name + " diagnostic build failed; no replacement attempt")
        unchanged()
    failed = False
    for command in commands:
        unchanged()
        label = command["label"]
        print("START", label, flush=True)
        receipt = runner.measure(command["argv"], output / label, 6, 660)
        result["commands"][label] = receipt["child_exit_code"]
        runner.save(output / "result.json", result)
        print("COMPLETE", label, receipt["child_exit_code"], flush=True)
        failed |= receipt["child_exit_code"] != 0
        if receipt["stop_reason"]:
            raise ValueError("resource stop; remaining diagnostics held")
        if receipt["child_exit_code"] == 0:
            phase = json.loads(Path(command["phase"]).read_text())
            if phase["iterations"] <= 0 or phase["after"]["unix_ns"] <= phase["before"]["unix_ns"]:
                raise ValueError("malformed successful phase receipt")
    unchanged()
    result.update(exit_code=int(failed), complete=True)
    result["artifacts"] = {str(p.relative_to(output)): runner.digest(p) for p in output.rglob("*")
                           if p.is_file() and p.name != "result.json"}
    runner.save(output / "result.json", result)
    return int(failed)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--checkout", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    try:
        sys.exit(execute(args.checkout.resolve(), args.output.resolve()))
    except (OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        print("Phase diagnostic held:", error, file=sys.stderr)
        sys.exit(1)
