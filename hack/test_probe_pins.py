"""The alpine probe pins of the image checks follow the images' alpine stages.

hack/hostnetwork-check.sh and hack/volume-check.sh run their probe container
from the alpine image of the build stages in images/base/Dockerfile and
images/hostnetwork/Dockerfile. Dependabot bumps only the Dockerfile FROM
lines, so this test fails its pull request until the scripts follow.
"""

from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parent.parent
PIN = re.compile(r"alpine:[0-9][0-9.]*@sha256:[0-9a-f]{64}")
DOCKERFILES = ("images/base/Dockerfile", "images/hostnetwork/Dockerfile")
SCRIPTS = ("hack/hostnetwork-check.sh", "hack/volume-check.sh")


def pins(path):
    return set(PIN.findall((ROOT / path).read_text()))


class ProbePinsTest(unittest.TestCase):
    def test_the_dockerfiles_agree_on_one_alpine(self):
        stages = set().union(*(pins(f) for f in DOCKERFILES))
        self.assertEqual(len(stages), 1, stages)
        for f in DOCKERFILES:
            self.assertTrue(pins(f), f"{f} pins no alpine image")

    def test_the_probe_pins_follow_the_dockerfiles(self):
        stages = set().union(*(pins(f) for f in DOCKERFILES))
        for script in SCRIPTS:
            with self.subTest(script=script):
                self.assertEqual(pins(script), stages, f"{script}: bump the probe with {', '.join(DOCKERFILES)}")


if __name__ == "__main__":
    unittest.main()
