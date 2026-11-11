#!/usr/bin/env python3
"""Run the race suite, bounding the large command package by test inventory.

A few timing tests of the command package assert their strict bound only
without the race detector, which slows publication about tenfold. They run a
second time, without it, after the race batches (NORACE_TESTS).
"""

import argparse
import json
import math
import os
import re
import subprocess
import sys


COMMAND_PACKAGE = "github.com/robertherbaugh/kudurru/cmd/kudurru"
TEST_NAME = re.compile(r"(?:Test|Example|Fuzz)\w*\Z")
MAX_BATCH_NAMES = 100
# Command tests whose strict timing assertion is compiled in only without the
# race detector (cmd/kudurru/norace_test.go). Each must be in the inventory.
NORACE_TESTS = ("TestRunServeSQLiteLongApplyKeepsServingDNSAndReads",)


def inventory(output, package):
    """Accept names from Go's compiled test discovery, including fuzz seeds."""
    names = []
    passed = False
    for line in output.splitlines():
        event = json.loads(line)
        if event.get("Package") != package:
            continue
        if event.get("Action") == "pass" and "Test" not in event:
            passed = True
        if event.get("Action") != "output":
            continue
        value = event["Output"].rstrip("\n")
        if TEST_NAME.fullmatch(value):
            names.append(value)
        elif not re.fullmatch(r"ok\s+" + re.escape(package) + r"\s+.+", value):
            raise ValueError("unexpected compiled test inventory output")
    if not passed or not names or len(set(names)) != len(names):
        raise ValueError("incomplete, empty or duplicate compiled test inventory")
    return sorted(names)


def batches(names):
    """Spread sorted names across bounded batches; select every name once."""
    if not names or len(set(names)) != len(names):
        raise ValueError("test inventory must be nonempty and unique")
    count = math.ceil(len(names) / MAX_BATCH_NAMES)
    return [names[index::count] for index in range(count)]


def execute(patterns, timeout):
    env = dict(os.environ, CGO_ENABLED="1")
    listed = subprocess.run(
        ["go", "list", *patterns], env=env, text=True, stdout=subprocess.PIPE,
        check=True,
    )
    packages = listed.stdout.splitlines()
    if not packages or len(set(packages)) != len(packages):
        raise ValueError("package inventory must be nonempty and unique")
    flags = ["go", "test", "-race", "-count=1", "-timeout=" + timeout]
    failed = False
    if COMMAND_PACKAGE in packages:
        discovery = subprocess.run(
            [*flags, "-json", "-list=^(Test|Example|Fuzz)", COMMAND_PACKAGE],
            env=env, text=True, stdout=subprocess.PIPE, check=True,
        )
        names = inventory(discovery.stdout, COMMAND_PACKAGE)
        missing = sorted(set(NORACE_TESTS) - set(names))
        if missing:
            raise ValueError("timing tests not in the command inventory: "
                             + ", ".join(missing))
        groups = batches(names)
        print(f"{COMMAND_PACKAGE}: {len(names)} compiled tests/examples/fuzz "
              f"targets in {len(groups)} disjoint batches", flush=True)
        for number, group in enumerate(groups, 1):
            # Go identifiers contain no regexp metacharacters; anchored whole
            # top-level names also retain every nested subtest and fuzz seed.
            selection = "^(" + "|".join(group) + ")$"
            print(f"Command batch {number}/{len(groups)}: " + ", ".join(group),
                  flush=True)
            result = subprocess.run(
                [*flags, "-run=" + selection, COMMAND_PACKAGE], env=env,
                check=False,
            )
            failed = failed or result.returncode != 0
        print("Command timing tests without the race detector: "
              + ", ".join(NORACE_TESTS), flush=True)
        result = subprocess.run(
            ["go", "test", "-count=1", "-timeout=" + timeout,
             "-run=^(" + "|".join(NORACE_TESTS) + ")$", COMMAND_PACKAGE],
            env=env, check=False,
        )
        failed = failed or result.returncode != 0
        packages.remove(COMMAND_PACKAGE)
    if packages:
        result = subprocess.run([*flags, *packages], env=env, check=False)
        failed = failed or result.returncode != 0
    return int(failed)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", default="30m")
    parser.add_argument("packages", nargs="*", default=["./..."])
    args = parser.parse_args()
    try:
        return execute(args.packages, args.timeout)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Go test inventory/execution failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
