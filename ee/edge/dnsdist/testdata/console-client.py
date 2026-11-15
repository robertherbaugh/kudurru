#!/usr/bin/env python3
"""Disposable wrapper for generated-supervisor acceptance, not a wire client.

Only the pinned external dnsdist executable handles console commands. No
upstream source is embedded. All daemon diagnostics stay in the owned fixture.
"""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import uuid

root = Path(os.environ["KUDURRU_F43_LAB"])
config = json.loads((root / "fixture.json").read_text())
docker = ["docker", "--host", "unix:///tmp/kudurru-lab-docker.sock"]
name = config["name"]
args = sys.argv[1:]
common = ["--network", "host", "--user", f"{os.getuid()}:{os.getgid()}",
          "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
          "--label", "io.kudurru.f43=" + name,
          "--mount", f"type=bind,src={root},dst={root},readonly", "--entrypoint", "dnsdist"]
if "--supervised" in args:
    if "--uid" in args:
        # Initial root TLS loading needs its ordinary DAC capability before
        # dnsdist drops to the configured UID; the dropped child owns snapshots.
        common += ["--cap-add", "SETUID", "--cap-add", "SETGID", "--cap-add", "DAC_OVERRIDE"]
    with (root / "child.private.log").open("ab") as output:
        os.chmod(output.name, 0o600)
        child = subprocess.Popen(docker + ["run", "--rm", "--name", name] + common +
                                 [config["image"]] + args, stdout=output, stderr=output)
        def stop(_signum, _frame):
            subprocess.run(docker + ["stop", "--time", "2", name], stdout=output,
                           stderr=output, timeout=10, check=False)
        def crash(_signum, _frame):
            subprocess.run(docker + ["kill", name], stdout=output,
                           stderr=output, timeout=10, check=False)
        signal.signal(signal.SIGUSR1, crash)
        signal.signal(signal.SIGTERM, stop)
        signal.signal(signal.SIGINT, stop)
        sys.exit(child.wait())
elif "--check-config" in args:
    sys.exit(subprocess.run(docker + ["run", "--rm", "--name", name + "-check-" + uuid.uuid4().hex[:8]] +
                            common + [config["image"]] + args, check=False).returncode)
else:
    result = subprocess.run(docker + ["exec", name, "dnsdist"] + args,
                            capture_output=True, check=False, timeout=10)
    script = args[-1]
    phase = "reload" if "reloadAllCertificates()" in script else "remove" if "rmServer(" in script else "stage" if "newServer(" in script else "none"
    with (root / "console-evidence.jsonl").open("a") as evidence:
        evidence.write(json.dumps({"phase": phase, "exit": result.returncode,
            "ack": b"KUDURRU_ACK_" in result.stdout, "waiting": b"waiting" in result.stdout,
            "diagnoses": [word for word in ["setAuto", "setHealthCheckResponseValidator", "newServer", "isUp", "syntax", "pool", "nil value", "not supported", "unable", "Unknown", "Lua", "member", "does not exist", "attempt to call", "nil", "Unable to convert", "syntax error", "stack traceback", "runtime", "configuration", "cannot", "Cannot", "only", "startup", "parameter", "function", "Unable", "convert", "not allowed"] if word.encode() in result.stdout + result.stderr]}) + "\n")
    fault = root / ("lose-ack-" + phase)
    if fault.exists():
        fault.unlink()
        sys.stdout.write("unacknowledged")
    else:
        sys.stdout.buffer.write(result.stdout)
    sys.stderr.buffer.write(result.stderr)
    sys.exit(result.returncode)
