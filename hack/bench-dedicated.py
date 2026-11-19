#!/usr/bin/env python3
"""Prepare historical calibration and a reviewed, dedicated-host paired run.

No command approves a host or calibration, replaces the committed baseline,
changes the legacy hardware limits, or marks a release qualified.
"""

import argparse
from contextlib import contextmanager
import fcntl
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import platform
import re
import statistics
import subprocess
import sys


ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("dedicated_paired", ROOT / "hack/bench-paired.py")
paired = importlib.util.module_from_spec(spec)
spec.loader.exec_module(paired)
reference, runner = paired.reference, paired.runner
LATER = "9cc2ae0423070fc514c94a25ab5b92e3816889e7"
CLASS = "shared-cloud-sandbox-v1"
CALIBRATED = ("cache", "upstream", "udp", "dot", "quic", "persist")


def read(path):
    return json.loads(Path(path).read_text())


def sha(value):
    return hashlib.sha256(value).hexdigest()


def host_identity():
    """Stable guest identity; physical isolation still requires operator evidence."""
    cpu_fields = {"processor", "vendor_id", "model name", "cpu family", "model",
                  "stepping", "physical id", "core id", "siblings", "cpu cores", "flags"}
    cpu = [line.strip() for line in Path("/proc/cpuinfo").read_text().splitlines()
           if ":" in line and line.split(":", 1)[0].strip() in cpu_fields]
    paths = ("/etc/os-release", "/proc/sys/kernel/random/boot_id", "/proc/self/cgroup",
             "/sys/fs/cgroup/cpu.max", "/sys/fs/cgroup/cpuset.cpus.effective",
             "/sys/fs/cgroup/memory.max")
    return {"machine_id_sha256": sha(Path("/etc/machine-id").read_bytes()),
            "hostname": platform.node(), "kernel": platform.release(), "machine": platform.machine(),
            "cpu": cpu, "affinity": sorted(os.sched_getaffinity(0)),
            "memory_total": next(line for line in Path("/proc/meminfo").read_text().splitlines()
                                 if line.startswith("MemTotal:")),
            "system": {name: Path(name).read_text() for name in paths if Path(name).exists()},
            "storage": subprocess.check_output(["findmnt", "-n", "-o", "SOURCE,FSTYPE,OPTIONS", "-T", str(ROOT)]).decode().strip(),
            "go_env": json.loads(subprocess.check_output(["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH",
                                                         "GOEXPERIMENT", "GOAMD64", "CGO_ENABLED"]))}


def approved(document, purpose):
    if document.get("decision") != "accepted" or not all(
            isinstance(document.get(key), str) and document[key].strip()
            for key in ("reviewed_by", "reviewed_at", "review_evidence")):
        raise ValueError(purpose + " requires recorded human review")


def validate_host(document):
    approved(document, "dedicated host")
    for key in ("physical_host", "vm_id", "cpu_reservation", "isolation_evidence", "guest_image"):
        if not isinstance(document.get(key), str) or not document[key].strip():
            raise ValueError("missing host evidence: " + key)
    identity = document["identity"]
    if identity != host_identity():
        raise ValueError("dedicated host identity changed")
    go = identity["go_env"]
    if (go["GOVERSION"], go["GOOS"], go["GOARCH"]) != ("go1.27.4", "linux", "amd64"):
        raise ValueError("dedicated method requires Go 1.27.4 on Linux amd64")
    if go["CGO_ENABLED"] != "0" or go["GOEXPERIMENT"]:
        raise ValueError("dedicated method requires CGO_ENABLED=0 and no Go experiments")
    if len(identity["affinity"]) < 6:
        raise ValueError("six benchmark processors must be available")


def expected_metrics():
    expected = {}
    for directory in ("ubicloud8-20261108", "ubicloud8-sockets-persist-20261108"):
        provenance = read(ROOT / "bench/calibration" / directory / "provenance.json")
        for scope, data in provenance["scopes"].items():
            expected[scope] = {(metric["name"], metric["unit"]) for metric in data["metrics"]}
    expected["blocking-zone"] = set(vectors((ROOT / "bench/baseline.txt").read_text(), samples=None))
    return expected


def vectors(report, samples=5):
    """Extract vectors after the native Go gate validates report syntax and CPU."""
    values = {}
    for line in report.splitlines():
        if not line.startswith("Benchmark"):
            continue
        fields = line.split()
        if not fields[0].endswith("-6") or len(fields) < 4 or len(fields) % 2:
            raise ValueError("malformed metric row")
        for index in range(2, len(fields), 2):
            value = float(fields[index])
            if not math.isfinite(value) or value < 0:
                raise ValueError("invalid metric value")
            values.setdefault((fields[0][:-2], fields[index + 1]), []).append(value)
    if not values or (samples is not None and any(len(v) != samples for v in values.values())):
        raise ValueError("incomplete five-process metric vectors")
    return values


def profile(scope, values, expected):
    if set(values) != expected:
        raise ValueError("historical metric inventory differs: " + scope)
    lines = ["# Dedicated host: historical calibration only; human review required."]
    for (name, unit), vector in sorted(values.items()):
        if len(vector) != 5 or any(not math.isfinite(v) or v < 0 for v in vector):
            raise ValueError("invalid calibration vector")
        pattern = "^" + re.escape(name) + "$"
        lines.append(f"require {pattern} {unit}")
        median = statistics.median(vector)
        if unit in ("failed", "lost"):
            if any(vector):
                raise ValueError("historical loss/failure cannot be calibrated away")
            lines.append(f"zero {pattern} {unit}")
        elif unit in ("q/s", "MB/s", "entries"):
            lines.append(f"minimum {pattern} {unit} {median * .8:.12g}")
        else:
            noise = 25 if unit == "ns/op" else 64 if unit == "B/op" else 0
            lines.append(f"limit {pattern} {unit} {median + max(median * .2, noise):.12g}")
    return "\n".join(lines) + "\n"


def inventory(directory):
    return {str(path.relative_to(directory)): runner.digest(path)
            for path in sorted(directory.rglob("*")) if path.is_file() and path.name != "manifest.json"}


def verify_calibration(directory):
    manifest = read(directory / "manifest.json")
    if inventory(directory) != manifest["files"]:
        raise ValueError("calibration evidence changed")
    outcome, plan = read(directory / "result.json"), read(directory / "plan.json")
    if not outcome["measurement_complete"] or outcome["exit_code"] != 0:
        raise ValueError("failed calibration is ineligible")
    if plan["sources"] != {"original": reference.REFERENCE, "later": LATER}:
        raise ValueError("calibration source identity differs")
    if (plan["procs"], plan["samples"], plan["commands"]) != (6, 5, runner.commands()):
        raise ValueError("calibration workload plan differs")
    if plan["legacy_limits"] != {scope: runner.digest(ROOT / path) for scope, path in runner.LIMIT_FILES.items()}:
        raise ValueError("original limit identities changed")
    for scope, argv in runner.commands().items():
        pids = set()
        for number in range(1, 6):
            receipt = read(directory / f"{scope}-{number}.json")
            if (receipt["argv"] != argv or receipt["child_exit_code"] != 0 or
                    receipt["stop_reason"] is not None or not receipt["group_reaped"]):
                raise ValueError("calibration native receipt is ineligible")
            pids.add(receipt["pid"])
        if len(pids) != 5:
            raise ValueError("five distinct calibration processes are required")
    for report in runner.REPORTS:
        receipt = read(directory / (report + "-shape.json"))
        if receipt["child_exit_code"] != 0 or receipt["stop_reason"] is not None or not receipt["group_reaped"]:
            raise ValueError("calibration shape/product gate failed")
    expected = expected_metrics()
    for scope in CALIBRATED:
        actual = profile(scope, vectors((directory / (scope + ".txt")).read_text()), expected[scope])
        if (directory / (scope + "-limits.txt")).read_text() != actual:
            raise ValueError("profile is not derived from historical vectors")
    return manifest


@contextmanager
def guarded(host_path, artifacts=None):
    """Serialize this host; bracket every original bounded process with guards."""
    host_path = host_path.resolve()
    document, host_hash = read(host_path), runner.digest(host_path)
    validate_host(document)
    if any(os.environ.get(key) for key in ("KUDURRU_BLOCKING_BENCH_LISTS", "KUDURRU_BLOCKING_MEMORY_HELPER",
                                          "KUDURRU_BLOCKING_MEMORY_HELPER_SUFFIX_FIRST")):
        raise ValueError("external benchmark corpus override is not allowed")
    retained = inventory(artifacts) if artifacts else None
    manifest_hash = runner.digest(artifacts / "manifest.json") if artifacts else None
    original = runner.measure

    def check():
        if runner.digest(host_path) != host_hash:
            raise ValueError("host review changed")
        validate_host(document)
        if artifacts and (inventory(artifacts) != retained or runner.digest(artifacts / "manifest.json") != manifest_hash):
            raise ValueError("reviewed calibration changed")

    def measure(*args, **kwargs):
        check()
        receipt = original(*args, **kwargs)
        check()
        return receipt

    with Path("/var/tmp/kudurru-dedicated-benchmark.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        runner.measure = measure
        try:
            check()
            yield check
            check()
        finally:
            runner.measure = original


def calibrate(original, later, host, output):
    if len({original, later, ROOT}) != 3:
        raise ValueError("three distinct clean source checkouts are required")
    for root, head in ((original, reference.REFERENCE), (later, LATER)):
        if reference.checkout_head(root) != head:
            raise ValueError("historical checkout identity differs")
        if output == root or root in output.parents:
            raise ValueError("artifacts must be outside historical checkouts")
    roots = {scope: original if scope in reference.SCOPES else later for scope in runner.SUITES}
    heads = {original: reference.REFERENCE, later: LATER, ROOT: reference.checkout_head(ROOT)}
    sources = {}
    version = subprocess.check_output(["go", "version"])
    for root, head in heads.items():
        with reference.at(root):
            if reference.checkout_head(root) != head or subprocess.check_output(["go", "version"]) != version:
                raise ValueError("historical checkout or toolchain differs")
            sources[root] = runner.source_identity()
    output.mkdir(parents=True, exist_ok=False)
    runner.save(output / "result.json", {"measurement_complete": False, "exit_code": 1, "release_qualified": False})
    runner.save(output / "host.json", read(host))
    runner.save(output / "plan.json", {"sources": {"original": reference.REFERENCE, "later": LATER},
                "control": heads[ROOT], "runner_class": CLASS, "procs": 6, "samples": 5,
                "commands": runner.commands(), "sequence": [(scope, i) for i in range(1, 6) for scope in runner.SUITES],
                "legacy_limits": {scope: runner.digest(ROOT / path) for scope, path in runner.LIMIT_FILES.items()}})
    for name, root in (("original", original), ("later", later), ("control", ROOT)):
        runner.save(output / (name + "-source.json"), sources[root])
    expected, outcomes, receipts = expected_metrics(), {}, {}

    def unchanged():
        for root, head in heads.items():
            with reference.at(root):
                if reference.checkout_head(root) != head or runner.source_identity() != sources[root]:
                    raise ValueError("calibration source changed")
        for path, digest in receipts.items():
            if runner.digest(path) != digest:
                raise ValueError("calibration recording changed")

    for path in output.iterdir():
        if path.name != "result.json":
            receipts[path] = runner.digest(path)
    for number in range(1, 6):
        for scope, argv in runner.commands().items():
            unchanged()
            label = f"{scope}-{number}"
            print("START historical", label, flush=True)
            with reference.at(roots[scope]):
                receipt = runner.measure(argv, output / label, 6)
            outcomes[label] = receipt["child_exit_code"]
            if receipt["stop_reason"] is not None or not receipt["group_reaped"]:
                raise ValueError("calibration stopped; remaining sequence held")
            for suffix in (".json", ".log"):
                path = output / (label + suffix)
                receipts[path] = runner.digest(path)
            unchanged()
            print("COMPLETE historical", label, receipt["child_exit_code"], flush=True)
    failed = any(outcomes.values())
    proposals = {}
    for report, scopes in runner.REPORTS.items():
        unchanged()
        path = output / (report + ".txt")
        with path.open("x") as stream:
            for number in range(1, 6):
                for scope in scopes:
                    label = f"{scope}-{number}"
                    stream.write((output / (label + ".log")).read_text())
                    if outcomes[label]:
                        stream.write("\nFAIL: go test exited non-zero\n")
        # Shape, count, CPU, native failures and fixed product requirements are
        # checked independently of the legacy hardware ceilings that failed.
        rules = (ROOT / "bench/limits.txt").read_text() if report == "blocking-zone" else ""
        rules += "\n" + "\n".join(f"require ^{re.escape(name)}$ {unit}" for name, unit in sorted(expected[report])) + "\n"
        shape = output / (report + "-shape-rules.txt")
        shape.write_text(rules)
        argv = ["go", "run", "./hack/benchgate", "-samples", "5", "-procs", "6", "-strict-cpu",
                "-threshold", "20", "-limits", str(shape), str(path)]
        with reference.at(ROOT):
            receipt = runner.measure(argv, output / (report + "-shape"), 6, 300)
        if receipt["stop_reason"] is not None or not receipt["group_reaped"]:
            raise ValueError("calibration gate stopped; remaining checks held")
        failed |= receipt["child_exit_code"] != 0
        if receipt["child_exit_code"] == 0 and report in CALIBRATED:
            proposals[report] = profile(report, vectors(path.read_text()), expected[report])
        for item in (path, shape, output / (report + "-shape.log"), output / (report + "-shape.json")):
            receipts[item] = runner.digest(item)
    unchanged()
    if not failed:
        for report, content in proposals.items():
            (output / (report + "-limits.txt")).write_text(content)
    runner.save(output / "result.json", {"measurement_complete": True, "exit_code": int(failed), "release_qualified": False})
    runner.save(output / "manifest.json", {"files": inventory(output)})
    return int(failed)


def validate_transition(host, calibration, approval, transition):
    """A new VM/boot requires its own human decision; never rewrite calibration."""
    document = read(transition)
    approved(document, "qualification host transition")
    expected = {"purpose": "qualification-host-transition",
                "calibration_manifest_sha256": runner.digest(calibration / "manifest.json"),
                "calibration_approval_sha256": runner.digest(approval),
                "previous_host_sha256": runner.digest(calibration / "host.json"),
                "qualification_host_sha256": runner.digest(host)}
    if any(document.get(key) != value for key, value in expected.items()):
        raise ValueError("host transition does not bind the reviewed hosts, calibration and approval")
    return document


def qualify(original, host, calibration, approval, output, transition=None):
    verify_calibration(calibration)
    review = read(approval)
    approved(review, "calibration")
    if review["calibration_manifest_sha256"] != runner.digest(calibration / "manifest.json"):
        raise ValueError("approval does not bind this calibration")
    transition_review = validate_transition(host, calibration, approval, transition) if transition else None
    if read(host) != read(calibration / "host.json") and transition_review is None:
        raise ValueError("qualification host differs from calibration")
    approval_hash = runner.digest(approval)
    transition_hash = runner.digest(transition) if transition else None
    original_measure = runner.measure

    def measure(*args, **kwargs):
        def check():
            if runner.digest(approval) != approval_hash:
                raise ValueError("calibration approval changed")
            if transition and runner.digest(transition) != transition_hash:
                raise ValueError("host transition approval changed")
        check()
        result = original_measure(*args, **kwargs)
        check()
        return result

    old_limits, old_class = runner.LIMIT_FILES, reference.RUNNER_CLASS
    runner.LIMIT_FILES = {"blocking-zone": str(ROOT / "bench/limits.txt"),
                          **{scope: str(calibration / (scope + "-limits.txt")) for scope in CALIBRATED}}
    reference.RUNNER_CLASS = CLASS
    runner.measure = measure
    try:
        output.mkdir(parents=True, exist_ok=False)
        runner.save(output / "dedicated-review.json", {"approval": review, "approval_sha256": approval_hash,
                    "calibration_manifest_sha256": review["calibration_manifest_sha256"], "host": read(host),
                    "host_transition": transition_review, "host_transition_sha256": transition_hash,
                    "release_qualified": False})
        return paired.execute(original, output / "paired", "interleaved")
    finally:
        runner.LIMIT_FILES, reference.RUNNER_CLASS, runner.measure = old_limits, old_class, original_measure


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("inspect-host", "calibrate", "review-template", "qualify"))
    parser.add_argument("--host-record", type=Path)
    parser.add_argument("--reference", type=Path)
    parser.add_argument("--later-reference", type=Path)
    parser.add_argument("--calibration", type=Path)
    parser.add_argument("--approval", type=Path)
    parser.add_argument("--host-transition-approval", type=Path,
                        help="separate human approval binding both hosts and the unchanged calibration")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        for key, value in vars(args).items():
            if isinstance(value, Path):
                setattr(args, key, value.resolve())
        if args.output.exists():
            raise ValueError("existing output is refused; retain every attempt")
        if args.phase == "inspect-host":
            runner.save(args.output, {"identity": host_identity(), "physical_host": "", "vm_id": "",
                        "cpu_reservation": "", "isolation_evidence": "", "guest_image": "",
                        "decision": "pending", "reviewed_by": "", "reviewed_at": "", "review_evidence": ""})
            return 0
        if args.phase == "review-template":
            verify_calibration(args.calibration)
            runner.save(args.output, {"calibration_manifest_sha256": runner.digest(args.calibration / "manifest.json"),
                        "decision": "pending", "reviewed_by": "", "reviewed_at": "", "review_evidence": ""})
            return 0
        with guarded(args.host_record, args.calibration if args.phase == "qualify" else None):
            if args.phase == "calibrate":
                return calibrate(args.reference, args.later_reference, args.host_record, args.output)
            return qualify(args.reference, args.host_record, args.calibration, args.approval, args.output,
                           args.host_transition_approval)
    except (OSError, ValueError, KeyError, TypeError, AttributeError, subprocess.CalledProcessError) as error:
        print("Dedicated benchmark held:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
