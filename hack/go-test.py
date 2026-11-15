#!/usr/bin/env python3
"""Run the race suite, bounding the large command package by test inventory.

A few timing tests of the command package assert their strict bound only
without the race detector, which slows publication about tenfold. They run a
second time, without it, after the race batches (NORACE_TESTS).

The security package includes 4,113 real IAM create/delete cycles. Run it in
its own race process so other packages do not compete for its timeout budget.
Its inventory, race instrumentation and per-process timeout stay unchanged.
"""

import argparse
import json
import math
import os
import re
import subprocess
import sys


COMMAND_PACKAGE = "github.com/robertherbaugh/kudurru/cmd/kudurru"
SECURITY_PACKAGE = "github.com/robertherbaugh/kudurru/internal/auth/security"
TEST_NAME = re.compile(r"(?:Test|Example|Fuzz)\w*\Z")
MAX_BATCH_NAMES = 100
SUITES = ("all", "commands", "security", "packages")
# Command tests whose strict timing assertion is compiled in only without the
# race detector (cmd/kudurru/norace_test.go). Each must be in the inventory.
NORACE_TESTS = ("TestRunServeSQLiteLongApplyKeepsServingDNSAndReads",
                "TestRunServeSQLiteZonesAnswerAcrossCommits",
                "TestRunServeSQLitePrimaryParentAndChildZones",
                "TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates")


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


def command_shard(value):
    if not re.fullmatch(r"[1-9][0-9]*/[1-9][0-9]*", value):
        raise ValueError("command shard must be I/N with 1 <= I <= N")
    index, count = map(int, value.split("/"))
    if index > count:
        raise ValueError("command shard must be I/N with 1 <= I <= N")
    return index, count


def execute(patterns, timeout, *, suite="all", shard=None):
    if suite not in SUITES or (shard is not None and suite != "commands"):
        raise ValueError("a command shard requires the commands suite")
    if shard is not None and (len(shard) != 2 or not 1 <= shard[0] <= shard[1]):
        raise ValueError("command shard must satisfy 1 <= I <= N")
    env = dict(os.environ, CGO_ENABLED="1")
    listed = subprocess.run(
        ["go", "list", *patterns], env=env, text=True, stdout=subprocess.PIPE,
        check=True,
    )
    packages = listed.stdout.splitlines()
    if not packages or len(set(packages)) != len(packages):
        raise ValueError("package inventory must be nonempty and unique")
    if suite != "all":
        required = [COMMAND_PACKAGE] if suite == "commands" else [SECURITY_PACKAGE]
        if suite == "packages":
            required = [COMMAND_PACKAGE, SECURITY_PACKAGE]
        if any(package not in packages for package in required):
            raise ValueError("CI suite requires the complete project package inventory")
        if suite == "commands":
            packages = [COMMAND_PACKAGE]
        elif suite == "security":
            packages = [SECURITY_PACKAGE]
        else:
            packages = [package for package in packages
                        if package not in (COMMAND_PACKAGE, SECURITY_PACKAGE)]
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
            if shard is not None and (number - 1) % shard[1] != shard[0] - 1:
                continue
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
        if shard is None or shard[0] == 1:
            print("Command timing tests without the race detector: "
                  + ", ".join(NORACE_TESTS), flush=True)
            result = subprocess.run(
                ["go", "test", "-count=1", "-timeout=" + timeout,
                 "-run=^(" + "|".join(NORACE_TESTS) + ")$", COMMAND_PACKAGE],
                env=env, check=False,
            )
            failed = failed or result.returncode != 0
        packages.remove(COMMAND_PACKAGE)
    if SECURITY_PACKAGE in packages:
        print(SECURITY_PACKAGE + ": isolated race process for the complete "
              "IAM churn regression", flush=True)
        result = subprocess.run([*flags, SECURITY_PACKAGE], env=env, check=False)
        failed = failed or result.returncode != 0
        packages.remove(SECURITY_PACKAGE)
    if packages:
        result = subprocess.run([*flags, *packages], env=env, check=False)
        failed = failed or result.returncode != 0
    return int(failed)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", default="30m")
    parser.add_argument("--suite", choices=SUITES, default="all")
    parser.add_argument("--command-shard", default="")
    parser.add_argument("packages", nargs="*", default=["./..."])
    args = parser.parse_args()
    try:
        shard = command_shard(args.command_shard) if args.command_shard else None
        return execute(args.packages, args.timeout, suite=args.suite, shard=shard)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Go test inventory/execution failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
