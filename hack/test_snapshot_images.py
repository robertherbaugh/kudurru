"""Tests for hack/snapshot-images.sh against a fake docker.

python3 -B -m unittest discover -s hack -p test_snapshot_images.py

The fake keeps the daemon's refs in a file, answers `images --format ...
<repository>` like docker (repository filter included), `run ... <ref>
version` from a per-ref file and `rmi <ref>` by deleting the line; every call
is logged. The real goreleaser names are used: goreleaser.ko.local with the
snapshot version, the -debug variant and a digest tag per image.
"""

import os
import pathlib
import stat
import subprocess
import tempfile
import textwrap
import unittest

SCRIPT = pathlib.Path(__file__).resolve().parent / "snapshot-images.sh"
VERSION = "0.1.0-rc.5-SNAPSHOT-3709a4e6"
DOMAIN = "goreleaser.ko.local"

FAKE_DOCKER = textwrap.dedent(
    """\
    #!/usr/bin/env bash
    state=$FAKE_DOCKER_STATE
    echo "$*" >>"$state/calls"
    case "$1" in
    images)
        repo=${@: -1}
        grep "^$repo:" "$state/images" || true
        ;;
    run)
        ref=${@: -2:1}
        key=$(printf '%s' "$ref" | tr '/:' '__')
        if [ -f "$state/version-$key" ]; then cat "$state/version-$key"; else echo "Unable to find image '$ref' locally" >&2; exit 125; fi
        ;;
    rmi)
        grep -qxF "$2" "$state/images" || { echo "No such image: $2" >&2; exit 1; }
        grep -vxF "$2" "$state/images" >"$state/images.new"; mv "$state/images.new" "$state/images"
        echo "Untagged: $2"
        ;;
    *) echo "fake docker: unexpected $*" >&2; exit 2 ;;
    esac
    """
)

FAKE_VOLUME_CHECK = textwrap.dedent(
    """\
    #!/usr/bin/env bash
    echo "volume-check $*" >>"$FAKE_DOCKER_STATE/calls"
    echo "FAIL $1: plain base"
    exit 1
    """
)


def executable(path, text):
    path.write_text(text)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)


class SnapshotImagesTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = pathlib.Path(self.tmp.name)
        self.state = self.dir / "state"
        self.state.mkdir()
        (self.state / "calls").write_text("")
        executable(self.dir / "docker", FAKE_DOCKER)
        executable(self.dir / "volume-check", FAKE_VOLUME_CHECK)
        self.env = dict(
            os.environ,
            DOCKER=str(self.dir / "docker"),
            VOLUME_CHECK=str(self.dir / "volume-check"),
            FAKE_DOCKER_STATE=str(self.state),
        )

    def tearDown(self):
        self.tmp.cleanup()

    def daemon(self, *refs):
        (self.state / "images").write_text("".join(r + "\n" for r in refs))

    def prints(self, ref, line):
        key = ref.replace("/", "_").replace(":", "_")
        (self.state / f"version-{key}").write_text(line + "\n")

    def run_script(self, *args):
        return subprocess.run(
            ["bash", str(SCRIPT), *args], env=self.env, capture_output=True, text=True, timeout=60
        )

    def images(self):
        return (self.state / "images").read_text().split()

    def snapshot(self):
        """The refs goreleaser v2.18.3 loads for the two kos entries."""
        main, debug = f"{DOMAIN}:{VERSION}", f"{DOMAIN}:{VERSION}-debug"
        self.prints(main, f"kudurru {VERSION} (3709a4e6, go1.27.3, linux/amd64)")
        self.prints(debug, f"kudurru {VERSION} (3709a4e6, go1.27.3, linux/amd64)")
        return [main, debug, f"{DOMAIN}:{'a' * 64}", f"{DOMAIN}:{'b' * 64}"]

    def test_list_only_the_local_domain(self):
        self.daemon("ghcr.io/robertherbaugh/kudurru:0.1.0", f"{DOMAIN}:old", "alpine:3.24")
        got = self.run_script("list")
        self.assertEqual(got.returncode, 0, got.stderr)
        self.assertEqual(got.stdout.split(), [f"{DOMAIN}:old"])

    def test_list_empty_daemon_is_not_a_failure(self):
        self.daemon()
        got = self.run_script("list")
        self.assertEqual((got.returncode, got.stdout), (0, ""), got.stderr)

    def test_check_versions_and_removes_only_what_the_run_created(self):
        others = ["ghcr.io/robertherbaugh/kudurru:0.1.0", f"{DOMAIN}:someone-else", "kudurru-dnsdist:vip-fix-b8fd96f"]
        self.daemon(*others)
        before = self.dir / "before"
        before.write_text(self.run_script("list").stdout)
        self.daemon(*others, *self.snapshot())
        got = self.run_script("check", str(before), VERSION)
        self.assertEqual(got.returncode, 0, got.stdout + got.stderr)
        self.assertIn(f"ok   {DOMAIN}:{VERSION}: kudurru {VERSION} ", got.stdout)
        self.assertIn(f"ok   {DOMAIN}:{VERSION}-debug: kudurru {VERSION} ", got.stdout)
        calls = (self.state / "calls").read_text()
        self.assertIn(f"volume-check {DOMAIN}:{VERSION} {DOMAIN}:{VERSION}-debug", calls)
        self.assertIn("volume-check exit 1, not counted", got.stdout)
        self.assertEqual(sorted(self.images()), sorted(others))

    def test_check_fails_without_the_images(self):
        # A goreleaser that exited 0 but loaded nothing (or under another
        # name) must not pass.
        self.daemon("ghcr.io/robertherbaugh/kudurru:0.1.0")
        before = self.dir / "before"
        before.write_text("")
        got = self.run_script("check", str(before), VERSION)
        self.assertEqual(got.returncode, 1, got.stdout)
        self.assertIn(f"FAIL {DOMAIN}:{VERSION}: not in the daemon", got.stdout)
        self.assertIn(f"FAIL {DOMAIN}:{VERSION}-debug: not in the daemon", got.stdout)
        self.assertEqual(self.images(), ["ghcr.io/robertherbaugh/kudurru:0.1.0"])

    def test_check_fails_on_a_wrong_version_and_still_removes(self):
        self.daemon()
        before = self.dir / "before"
        before.write_text("")
        refs = self.snapshot()
        self.daemon(*refs)
        self.prints(refs[1], "kudurru 0.1.0-rc.5 (00000000, go1.27.3, linux/amd64)")
        got = self.run_script("check", str(before), VERSION)
        self.assertEqual(got.returncode, 1, got.stdout)
        self.assertIn(f"FAIL {DOMAIN}:{VERSION}-debug: kudurru 0.1.0-rc.5 ", got.stdout)
        self.assertEqual(self.images(), [])

    def test_check_removes_a_version_tag_that_existed_before(self):
        # The same commit snapshotted twice: the run overwrote the tag, so it
        # is this run's image and goes too.
        refs = self.snapshot()
        self.daemon(refs[0])
        before = self.dir / "before"
        before.write_text(self.run_script("list").stdout)
        self.daemon(*refs)
        got = self.run_script("check", str(before), VERSION)
        self.assertEqual(got.returncode, 0, got.stdout)
        self.assertEqual(self.images(), [])

    def test_release_local_checks_uses_the_helper(self):
        text = (SCRIPT.parent / "release-local-checks.sh").read_text()
        self.assertIn("hack/snapshot-images.sh list", text)
        self.assertIn("hack/snapshot-images.sh check", text)
        self.assertNotIn("ghcr.io/robertherbaugh/kudurru:.*SNAPSHOT", text)


if __name__ == "__main__":
    unittest.main()
