"""Verify complete selection and failure propagation without running Go."""

import importlib.util
from collections import Counter
from contextlib import redirect_stdout
import io
import json
from pathlib import Path
import re
import subprocess
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "go_test_runner", Path(__file__).with_name("go-test.py"))
runner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runner)


def discovery(names, passed=True):
    package = runner.COMMAND_PACKAGE
    events = [{"Action": "output", "Package": package, "Output": name + "\n"}
              for name in names]
    events.append({"Action": "output", "Package": package,
                   "Output": "ok  \t" + package + "\t0.01s\n"})
    events.append({"Action": "pass" if passed else "fail", "Package": package})
    return "\n".join(json.dumps(event) for event in events)


class GoTestBatches(unittest.TestCase):
    def test_six_ci_jobs_cover_every_compiled_target_and_package_once(self):
        names = [f"TestCase{index}" for index in range(933)]
        names += ["Example", "ExampleClient_read", "FuzzParse", "TestÜnicode"]
        names += list(runner.NORACE_TESTS)
        packages = [runner.COMMAND_PACKAGE, runner.SECURITY_PACKAGE,
                    "example.org/one", "example.org/two"]
        commands = []

        def run(argv, **kwargs):
            self.assertEqual(kwargs["env"]["CGO_ENABLED"], "1")
            if argv[1] == "list":
                return subprocess.CompletedProcess(argv, 0, "\n".join(packages) + "\n")
            if "-json" in argv:
                return subprocess.CompletedProcess(argv, 0, discovery(names))
            commands.append(argv)
            self.assertIn("-count=1", argv)
            self.assertIn("-timeout=30m", argv)
            return subprocess.CompletedProcess(argv, 0)

        jobs = [("commands", (i, 4)) for i in range(1, 5)]
        jobs += [("security", None), ("packages", None)]
        with patch.object(runner.subprocess, "run", side_effect=run), redirect_stdout(io.StringIO()):
            for suite, shard in jobs:
                self.assertEqual(runner.execute(["./..."], "30m", suite=suite, shard=shard), 0)
        targets = Counter()
        package_counts = Counter()
        timing = []
        for argv in commands:
            if "-race" not in argv:
                timing.append(argv)
            elif runner.COMMAND_PACKAGE in argv:
                pattern = next(arg.removeprefix("-run=") for arg in argv if arg.startswith("-run="))
                selected = [name for name in names if re.fullmatch(pattern, name)]
                self.assertTrue(0 < len(selected) <= runner.MAX_BATCH_NAMES)
                targets.update(selected)
            else:
                self.assertFalse(any(arg.startswith("-run") for arg in argv))
                package_counts.update(argv[5:])
        self.assertEqual(targets, Counter({name: 1 for name in names}))
        self.assertEqual(package_counts, Counter({package: 1 for package in packages[1:]}))
        self.assertEqual(timing, [["go", "test", "-count=1", "-timeout=30m",
                                   "-run=^(" + "|".join(runner.NORACE_TESTS) + ")$",
                                   runner.COMMAND_PACKAGE]])

    def test_sharded_failure_runs_remaining_assigned_batches_and_counts(self):
        names = [f"TestCase{index}" for index in range(933)] + list(runner.NORACE_TESTS)
        commands = []

        def run(argv, **kwargs):
            if argv[1] == "list":
                return subprocess.CompletedProcess(argv, 0, runner.COMMAND_PACKAGE + "\n")
            if "-json" in argv:
                return subprocess.CompletedProcess(argv, 0, discovery(names))
            commands.append(argv)
            return subprocess.CompletedProcess(argv, 1 if len(commands) == 1 else 0)

        with patch.object(runner.subprocess, "run", side_effect=run), redirect_stdout(io.StringIO()):
            self.assertEqual(runner.execute(["./..."], "30m", suite="commands", shard=(1, 4)), 1)
        self.assertEqual(sum("-race" in argv for argv in commands), 3)
        self.assertEqual(sum("-race" not in argv for argv in commands), 1)

    def test_invalid_shard_or_scope_refuses_before_launch(self):
        for value in ("", "0/4", "5/4", "1/0", "1", "1/4/2", "-1/4"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                runner.command_shard(value)
        self.assertEqual(runner.command_shard("2/4"), (2, 4))
        for options in ({"suite": "all", "shard": (1, 4)},
                        {"suite": "unknown"},
                        {"suite": "commands", "shard": (0, 4)},
                        {"suite": "commands", "shard": (5, 4)}):
            with self.subTest(options=options), patch.object(runner.subprocess, "run") as run:
                with self.assertRaises(ValueError):
                    runner.execute(["./..."], "30m", **options)
                run.assert_not_called()

    def test_missing_ci_package_inventory_refuses(self):
        for suite in ("commands", "security", "packages"):
            response = subprocess.CompletedProcess([], 0, "example.org/one\n")
            with self.subTest(suite=suite), patch.object(runner.subprocess, "run", return_value=response) as run:
                with self.assertRaises(ValueError):
                    runner.execute(["./..."], "30m", suite=suite)
                self.assertEqual(run.call_count, 1)

    def test_every_compiled_name_selected_once_including_examples_and_fuzz(self):
        names = [f"TestCase{index}" for index in range(237)]
        names += ["Example", "ExampleClient_read", "FuzzParse", "TestÜnicode"]
        actual = runner.inventory(discovery(names), runner.COMMAND_PACKAGE)
        groups = runner.batches(actual)
        self.assertEqual(sorted(name for group in groups for name in group),
                         sorted(names))
        self.assertTrue(all(0 < len(group) <= 100 for group in groups))
        for name in names:
            self.assertEqual(sum(bool(re.fullmatch("(" + "|".join(group) + ")", name))
                                 for group in groups), 1)
        self.assertEqual(groups, runner.batches(sorted(reversed(names))))

    def test_incomplete_or_ambiguous_inventory_refuses(self):
        for output in [discovery([]), discovery(["TestOne", "TestOne"]),
                       discovery(["TestOne"], passed=False), "not JSON",
                       discovery(["TestOne", "unexpected output"])]:
            with self.subTest(output=output), self.assertRaises(ValueError):
                runner.inventory(output, runner.COMMAND_PACKAGE)

    def test_failed_batch_does_not_replay_or_omit_remaining_tests(self):
        names = [f"TestCase{index}" for index in range(201)] + ["FuzzParse", "Example"]
        names += list(runner.NORACE_TESTS)
        commands = []
        timing = []

        def run(argv, **kwargs):
            self.assertEqual(kwargs["env"]["CGO_ENABLED"], "1")
            if argv[1] == "list":
                commands.append(argv)
                return subprocess.CompletedProcess(argv, 0,
                    runner.COMMAND_PACKAGE + "\nexample.org/other\n")
            if "-race" not in argv:
                timing.append(argv)
                return subprocess.CompletedProcess(argv, 0)
            commands.append(argv)
            self.assertIn("-race", argv)
            self.assertIn("-count=1", argv)
            self.assertIn("-timeout=30m", argv)
            if "-json" in argv:
                return subprocess.CompletedProcess(argv, 0, discovery(names))
            return subprocess.CompletedProcess(argv, 1 if len(commands) == 3 else 0)

        with patch.object(runner.subprocess, "run", side_effect=run), redirect_stdout(io.StringIO()):
            self.assertEqual(runner.execute(["./..."], "30m"), 1)
        selections = [next(arg.removeprefix("-run=") for arg in command
                           if arg.startswith("-run="))
                      for command in commands if any(arg.startswith("-run=") for arg in command)]
        for name in names:
            self.assertEqual(sum(bool(re.fullmatch(pattern, name)) for pattern in selections), 1)
        self.assertEqual(commands[-1][-1], "example.org/other")
        self.assertEqual(timing, [["go", "test", "-count=1", "-timeout=30m",
                                   "-run=^(" + "|".join(runner.NORACE_TESTS) + ")$",
                                   runner.COMMAND_PACKAGE]])

    def test_timing_tests_run_without_race_and_their_failure_counts(self):
        names = ["TestOther"] + list(runner.NORACE_TESTS)
        commands = []

        def run(argv, **kwargs):
            commands.append(argv)
            if argv[1] == "list":
                return subprocess.CompletedProcess(argv, 0, runner.COMMAND_PACKAGE + "\n")
            if "-json" in argv:
                return subprocess.CompletedProcess(argv, 0, discovery(names))
            return subprocess.CompletedProcess(argv, 0 if "-race" in argv else 1)

        with patch.object(runner.subprocess, "run", side_effect=run), redirect_stdout(io.StringIO()):
            self.assertEqual(runner.execute([runner.COMMAND_PACKAGE], "30m"), 1)
        self.assertNotIn("-race", commands[-1])
        self.assertEqual(sum("-race" not in command for command in commands[1:]), 1)

    def test_renamed_timing_test_refuses_before_running(self):
        commands = []

        def run(argv, **kwargs):
            commands.append(argv)
            if argv[1] == "list":
                return subprocess.CompletedProcess(argv, 0, runner.COMMAND_PACKAGE + "\n")
            return subprocess.CompletedProcess(argv, 0, discovery(["TestOther"]))

        with patch.object(runner.subprocess, "run", side_effect=run), \
                redirect_stdout(io.StringIO()), self.assertRaises(ValueError):
            runner.execute([runner.COMMAND_PACKAGE], "30m")
        self.assertEqual(len(commands), 2)

    def test_shard_without_command_package_uses_original_race_invocation(self):
        responses = [subprocess.CompletedProcess([], 0, "example.org/one\n"),
                     subprocess.CompletedProcess([], 0)]
        with patch.object(runner.subprocess, "run", side_effect=responses) as run:
            self.assertEqual(runner.execute(["example.org/one"], "30m"), 0)
        self.assertEqual(run.call_args.args[0],
                         ["go", "test", "-race", "-count=1", "-timeout=30m", "example.org/one"])

    def test_security_failure_retains_all_packages_once_with_same_race_bound(self):
        commands = []

        def run(argv, **kwargs):
            commands.append(argv)
            self.assertEqual(kwargs["env"]["CGO_ENABLED"], "1")
            if argv[1] == "list":
                return subprocess.CompletedProcess(argv, 0,
                    "example.org/one\n" + runner.SECURITY_PACKAGE + "\nexample.org/two\n")
            self.assertEqual(argv[:5],
                ["go", "test", "-race", "-count=1", "-timeout=30m"])
            self.assertFalse(any(arg.startswith("-run") for arg in argv))
            return subprocess.CompletedProcess(argv,
                1 if runner.SECURITY_PACKAGE in argv else 0)

        with patch.object(runner.subprocess, "run", side_effect=run), redirect_stdout(io.StringIO()):
            self.assertEqual(runner.execute(["./..."], "30m"), 1)
        self.assertEqual(commands[1][5:], [runner.SECURITY_PACKAGE])
        self.assertEqual(commands[2][5:], ["example.org/one", "example.org/two"])
        self.assertEqual(len(commands), 3)


if __name__ == "__main__":
    unittest.main()
