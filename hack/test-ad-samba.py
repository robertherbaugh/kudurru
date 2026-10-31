#!/usr/bin/env python3
"""Run a disposable Samba KDC against the isolated AD DNS feasibility probe."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time
import uuid


ROOT = Path(__file__).resolve().parent.parent
LABEL = "io.kudurru.ad-samba-lab"


def execute(args, *, timeout=30, env=None, cwd=None, check=True):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout,
                            env=env, cwd=cwd, check=False)
    if check and result.returncode:
        # Never copy tool logs or process arguments into the report.
        raise RuntimeError(f"{Path(args[0]).name} failed with exit {result.returncode}")
    return result


def main():
    def interrupted(_number, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--docker-host", help="Docker socket; defaults to Docker's environment/context")
    parser.add_argument("--image", default="kudurru-samba-lab:local")
    parser.add_argument("--build", action="store_true", help="Build the test-only Samba image")
    parser.add_argument("--build-network", default="default", help="Network used only for package installation")
    parser.add_argument("--output", type=Path, default=ROOT / ".context/ad-samba-report.json")
    opts = parser.parse_args()
    env = dict(os.environ)
    if opts.docker_host:
        env["DOCKER_HOST"] = opts.docker_host
    run_id = uuid.uuid4().hex
    name = "kudurru-ad-" + run_id[:12]
    network_created = container_created = False
    report = {"result": "fail", "checks": [], "limitations": [
        "Samba AD with MIT/BIND clients; no Windows DC, member or DHCP qualification.",
        "The separate feasibility probe refuses every update; this is not production AD DNS support.",
        "One isolated Linux network namespace; no edge/VIP, replication or failover acceptance.",
    ]}
    started = time.monotonic()

    def docker(*args, **kwargs):
        return execute(["docker", *args], env=env, **kwargs)

    def cleanup(kind):
        inspected = docker(kind, "inspect", name, check=False)
        if inspected.returncode:
            return
        obj = json.loads(inspected.stdout)[0]
        labels = obj.get("Config", {}).get("Labels", {}) if kind == "container" else obj.get("Labels", {})
        if labels.get(LABEL) != run_id:
            raise RuntimeError("Refusing cleanup of a resource without this run's label")
        if kind == "container":
            docker("container", "rm", "--force", name)
        else:
            docker("network", "rm", name)

    try:
        info = json.loads(docker("info", "--format", "{{json .}}").stdout)
        if info["OSType"] != "linux":
            raise RuntimeError("The Samba lab requires a Linux Docker engine")
        architecture = {"x86_64": "amd64", "aarch64": "arm64", "amd64": "amd64", "arm64": "arm64"}.get(info["Architecture"])
        if architecture is None:
            raise RuntimeError("Unsupported Docker architecture")
        if opts.build:
            docker("build", "--network", opts.build_network, "--tag", opts.image,
                   str(ROOT / "hack/labs/ad-samba"), timeout=600)
        report["image_id"] = json.loads(docker("image", "inspect", opts.image).stdout)[0]["Id"]
        report["commit"] = execute(["git", "rev-parse", "HEAD"], cwd=ROOT).stdout.strip()
        report["lab_or_probe_modified"] = bool(execute([
            "git", "status", "--porcelain", "--", "dev-docs/spikes/ad-dns", "hack/labs/ad-samba", "hack/test-ad-samba.py"
        ], cwd=ROOT).stdout.strip())
        report["helper_sha256"] = hashlib.sha256((ROOT / "hack/labs/ad-samba/container.py").read_bytes()).hexdigest()
        with tempfile.TemporaryDirectory(prefix="kudurru-ad-samba-") as temporary:
            binary = Path(temporary) / "ad-dns-probe"
            execute(["go", "build", "-o", str(binary), "."], timeout=180,
                    env=dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=architecture, GOMAXPROCS="2"),
                    cwd=ROOT / "dev-docs/spikes/ad-dns")
            report["probe_sha256"] = hashlib.sha256(binary.read_bytes()).hexdigest()
            docker("network", "create", "--internal", "--label", f"{LABEL}={run_id}", name)
            network_created = True
            docker("create", "--name", name, "--label", f"{LABEL}={run_id}",
                   "--log-driver", "none", "--network", name, "--hostname", "dc01",
                   "--add-host", "probe.ad.lab.test:127.0.0.1",
                   "--tmpfs", "/lab-state:rw,nosuid,nodev,noexec,mode=0700,size=512m",
                   "--mount", f"type=bind,src={binary},dst=/opt/lab/ad-dns-probe,readonly",
                   "--mount", f"type=bind,src={ROOT}/hack/labs/ad-samba/container.py,dst=/opt/lab/container.py,readonly",
                   opts.image)
            container_created = True
            result = docker("start", "--attach", name, timeout=180, check=False)
            # Discard tool output except this first-party helper's final JSON.
            candidates = [line for line in result.stdout.splitlines() if line.startswith('{"result":')]
            if len(candidates) != 1:
                raise RuntimeError("Lab did not return its single sanitized report")
            report.update(json.loads(candidates[0]))
            if result.returncode or report.get("result") != "pass":
                report["result"] = "fail"
    except (RuntimeError, OSError, ValueError, KeyError, subprocess.TimeoutExpired) as exc:
        report["result"] = "fail"
        report["error_type"] = type(exc).__name__
    except KeyboardInterrupt:
        report["result"] = "interrupted"
    finally:
        cleanup_errors = []
        for kind, created in [("container", container_created), ("network", network_created)]:
            if created:
                try:
                    cleanup(kind)
                except (RuntimeError, OSError, ValueError, subprocess.TimeoutExpired):
                    cleanup_errors.append(kind)
        report["cleanup"] = "pass" if not cleanup_errors else "fail"
        if cleanup_errors:
            report["result"] = "fail"
            report["cleanup_failed_resources"] = cleanup_errors
        report["duration_seconds"] = round(time.monotonic() - started, 3)
        opts.output.parent.mkdir(parents=True, exist_ok=True)
        opts.output.write_text(json.dumps(report, indent=2) + "\n")
        print(json.dumps(report, indent=2))
    return 0 if report["result"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
