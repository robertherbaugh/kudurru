"""Verify complete selection and failure propagation without running Go."""

import importlib.util
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


if __name__ == "__main__":
    unittest.main()
