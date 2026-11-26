"""hack/go-modules.sh retries transient proxy errors and nothing else.

A fake `go` (GO=...) replays one scripted result per call, so each case
counts how many attempts the script made for a given failure.
"""

from pathlib import Path
import os
import stat
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "hack" / "go-modules.sh"

# Call N prints $FAKE/N.out and exits with $FAKE/N.status (default 0). An
# install with GOBIN set writes a program there that echoes its arguments.
# `go env GOVERSION` is answered without counting as a call.
FAKE_GO = r"""#!/usr/bin/env bash
set -eu
if [ "$1" = env ]; then echo go1.99.0; exit 0; fi
n=$(( $(wc -l < "$FAKE/calls") + 1 ))
printf '%s|GOBIN=%s\n' "$*" "${GOBIN:-}" >> "$FAKE/calls"
[ -f "$FAKE/$n.out" ] && cat "$FAKE/$n.out" >&2
status=$(cat "$FAKE/$n.status" 2>/dev/null || echo 0)
if [ "$status" = 0 ] && [ "$1" = install ] && [ -n "${GOBIN:-}" ]; then
  printf '#!/bin/sh\necho "tool ran: $*"\nexit 7\n' > "$GOBIN/tool"
  chmod +x "$GOBIN/tool"
fi
exit "$status"
"""

STREAM_RESET = (
    'go: github.com/danielgtaylor/huma/v2@v2.41.0: read "https://proxy.golang.org/github.com/'
    'danielgtaylor/huma/v2/@v/v2.41.0.zip": stream error: stream ID 637; INTERNAL_ERROR; received from peer\n'
)
BAD_GATEWAY = 'go: example.com/m@v1.0.0: reading "https://proxy.golang.org/example.com/m/@v/v1.0.0.zip": 502 Bad Gateway\n'
CHECKSUM = (
    "verifying github.com/cloudflare/cloudflare-go/v7@v7.15.0: checksum mismatch\n"
    "\tdownloaded: h1:aaaa\n\tgo.sum:     h1:bbbb\n\nSECURITY ERROR\n"
)
UNKNOWN_REVISION = "go: example.com/m@v9.9.9: invalid version: unknown revision v9.9.9\n"
NOT_FOUND = (
    'go: example.com/m@v1.0.0: reading "https://proxy.golang.org/example.com/m/@v/v1.0.0.info": 404 Not Found\n'
    "\tserver response: not found: example.com/m@v1.0.0: invalid version: unknown revision v1.0.0\n"
)
COMPILE_ERROR = "# example.com/tool\n./main.go:5:2: undefined: missing\n"


class GoModulesTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.fake = self.dir / "fake"
        self.fake.mkdir()
        (self.fake / "calls").write_text("")
        self.go = self.dir / "go"
        self.go.write_text(FAKE_GO)
        self.go.chmod(self.go.stat().st_mode | stat.S_IXUSR)

    def tearDown(self):
        self.tmp.cleanup()

    def results(self, *results):
        for n, (status, output) in enumerate(results, start=1):
            (self.fake / f"{n}.status").write_text(str(status))
            (self.fake / f"{n}.out").write_text(output)

    def run_script(self, *args, env=None):
        environ = {
            **os.environ,
            "GO": str(self.go),
            "FAKE": str(self.fake),
            "GO_MODULES_BACKOFF": "0",
            "GO_MODULES_TOOLS": str(self.dir / "tools"),
        }
        environ.pop("GOBIN", None)
        environ.update(env or {})
        return subprocess.run(
            ["bash", str(SCRIPT), *args], env=environ, capture_output=True, text=True, timeout=60
        )

    def calls(self):
        return (self.fake / "calls").read_text().splitlines()

    def test_success_needs_one_attempt(self):
        proc = self.run_script("download")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(self.calls(), ["mod download|GOBIN="])

    def test_a_stream_reset_is_retried(self):
        self.results((1, STREAM_RESET), (0, ""))
        proc = self.run_script("download")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(len(self.calls()), 2)
        self.assertIn("retrying", proc.stderr)
        self.assertIn("stream error", proc.stderr)

    def test_a_5xx_is_retried(self):
        self.results((1, BAD_GATEWAY), (1, BAD_GATEWAY), (0, ""))
        proc = self.run_script("download")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(len(self.calls()), 3)

    def test_retries_are_bounded(self):
        self.results(*[(1, STREAM_RESET)] * 5)
        proc = self.run_script("download")
        self.assertEqual(proc.returncode, 1)
        self.assertEqual(len(self.calls()), 3)
        self.assertIn("giving up", proc.stderr)

    def test_the_attempt_count_is_configurable(self):
        self.results(*[(1, STREAM_RESET)] * 5)
        proc = self.run_script("download", env={"GO_MODULES_ATTEMPTS": "1"})
        self.assertEqual(proc.returncode, 1)
        self.assertEqual(len(self.calls()), 1)

    def test_permanent_failures_fail_on_the_first_attempt(self):
        for name, output in {
            "checksum mismatch": CHECKSUM,
            "unknown revision": UNKNOWN_REVISION,
            "404": NOT_FOUND,
            "compile error": COMPILE_ERROR,
            "no output": "",
        }.items():
            with self.subTest(name):
                (self.fake / "calls").write_text("")
                self.results((1, output), (0, ""))
                proc = self.run_script("download")
                self.assertEqual(proc.returncode, 1, proc.stderr)
                self.assertEqual(len(self.calls()), 1)
                self.assertNotIn("retrying", proc.stderr)

    def test_a_permanent_error_wins_over_a_transient_one(self):
        self.results((1, STREAM_RESET + CHECKSUM), (0, ""))
        proc = self.run_script("download")
        self.assertEqual(proc.returncode, 1)
        self.assertEqual(len(self.calls()), 1)

    def test_the_exit_status_is_kept(self):
        self.results((3, COMPILE_ERROR))
        proc = self.run_script("download")
        self.assertEqual(proc.returncode, 3)

    def test_download_passes_modules_through(self):
        proc = self.run_script("download", "example.com/m@v1.0.0")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(self.calls(), ["mod download example.com/m@v1.0.0|GOBIN="])

    def test_install_keeps_the_callers_gobin(self):
        gobin = self.dir / "bin"
        gobin.mkdir()
        self.results((1, STREAM_RESET), (0, ""))
        proc = self.run_script("install", "example.com/tool@v1.0.0", env={"GOBIN": str(gobin)})
        self.assertEqual(proc.returncode, 0, proc.stderr)
        self.assertEqual(self.calls(), [f"install example.com/tool@v1.0.0|GOBIN={gobin}"] * 2)
        self.assertTrue((gobin / "tool").exists())

    def test_run_retries_the_install_and_runs_the_tool_once(self):
        self.results((1, STREAM_RESET), (0, ""))
        proc = self.run_script("run", "example.com/tool@latest", "-x", "a b")
        # The tool's own exit status, after one run.
        self.assertEqual(proc.returncode, 7, proc.stderr)
        self.assertEqual(proc.stdout, "tool ran: -x a b\n")
        calls = self.calls()
        self.assertEqual(len(calls), 2)
        gobin = calls[0].split("|GOBIN=")[1]
        self.assertTrue(gobin)
        self.assertFalse(Path(gobin).exists(), "the temporary GOBIN is removed")
        # @latest is not kept: the next run installs again.
        proc = self.run_script("run", "example.com/tool@latest")
        self.assertEqual(proc.returncode, 7, proc.stderr)
        self.assertEqual(len(self.calls()), 3)

    def test_run_keeps_a_pinned_tool_per_go_version(self):
        self.results((1, STREAM_RESET), (0, ""))
        proc = self.run_script("run", "example.com/cmd/tool@v1.2.3", "first")
        self.assertEqual(proc.returncode, 7, proc.stderr)
        self.assertEqual(proc.stdout, "tool ran: first\n")
        self.assertEqual(len(self.calls()), 2)
        kept = self.dir / "tools" / "go1.99.0" / "example.com/cmd/tool@v1.2.3" / "tool"
        self.assertTrue(kept.exists())
        self.assertEqual([p.name for p in (self.dir / "tools").iterdir()], ["go1.99.0"], "no install directory is left")
        proc = self.run_script("run", "example.com/cmd/tool@v1.2.3", "second")
        self.assertEqual(proc.returncode, 7, proc.stderr)
        self.assertEqual(proc.stdout, "tool ran: second\n")
        self.assertEqual(len(self.calls()), 2, "the kept tool is not installed again")

    def test_run_does_not_run_after_a_failed_install(self):
        self.results((1, COMPILE_ERROR))
        proc = self.run_script("run", "example.com/tool@v1.0.0")
        self.assertEqual(proc.returncode, 1)
        self.assertNotIn("tool ran", proc.stdout)

    def test_run_needs_a_version(self):
        proc = self.run_script("run", "example.com/tool")
        self.assertEqual(proc.returncode, 2)
        self.assertEqual(self.calls(), [])

    def test_usage(self):
        for args in ((), ("fetch",), ("install",)):
            with self.subTest(args=args):
                self.assertEqual(self.run_script(*args).returncode, 2)


if __name__ == "__main__":
    unittest.main()
