"""Offline regressions for the release ruleset proposal and GET-only checker."""

import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location(
    "release_protection_check", Path(__file__).with_name("release-protection-check.py")
)
checker = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(checker)
EVIDENCE = checker.PROPOSAL.parents[2] / "dev-docs/milestones/v0.2/evidence/2026-11-16-final-source"


class ReleaseProtectionTest(unittest.TestCase):
    def setUp(self):
        self.current = checker.load(EVIDENCE / "release-ruleset.json")
        self.effective = checker.load(EVIDENCE / "effective-release-rules.json")
        self.proposal = checker.load(checker.PROPOSAL)
        self.corrected = [dict(rule, ruleset_id=checker.RULESET) for rule in self.proposal["rules"]]

    def call(self, *args):
        with contextlib.redirect_stdout(io.StringIO()) as out, contextlib.redirect_stderr(io.StringIO()) as err:
            status = checker.main(list(args))
        return status, out.getvalue() + err.getvalue()

    def test_snapshot_cli_fails_current_and_passes_corrected_without_network(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(checker, "get") as get:
            corrected_path = Path(directory) / "corrected.json"
            corrected_path.write_text(json.dumps(self.corrected), encoding="utf-8")
            status, output = self.call("--snapshot", str(EVIDENCE / "effective-release-rules.json"))
            self.assertEqual(status, 1)
            self.assertIn("missing required GitHub Actions checks: promotion", output)
            self.assertIn("expected merge only", output)
            self.assertEqual(self.call("--snapshot", str(corrected_path))[0], 0)
        get.assert_not_called()

    def test_proposal_preserves_every_other_live_setting(self):
        original = copy.deepcopy(self.current)
        self.assertEqual(checker.expected_proposal(self.current), self.proposal)
        self.assertEqual(self.current, original)
        self.assertEqual(checker.expected_proposal(self.proposal), self.proposal)

    def test_policy_rejects_wrong_app_and_weakened_reviews(self):
        rules = checker.scoped_rules(self.corrected)
        parameters = checker.parameters_by_type(rules)
        parameters["required_status_checks"]["required_status_checks"][-1]["integration_id"] = 0
        parameters["pull_request"]["require_last_push_approval"] = False
        errors = checker.check_policy(rules, self.corrected)
        self.assertIn("missing required GitHub Actions checks: promotion", errors)
        self.assertIn("require_last_push_approval must be enabled", errors)

    def fake_github(self, *, applied=False, classic_status=403):
        detail = dict(self.current, **self.proposal) if applied else self.current
        effective = self.corrected if applied else self.effective
        endpoints = {"": {"allow_merge_commit": True}, f"/rulesets/{checker.RULESET}": detail}
        for branch in checker.BRANCHES:
            endpoints[f"/rules/branches/{branch}"] = effective
            endpoints[f"/branches/{branch}"] = {"protected": True, "protection": {"enabled": False}}
            endpoints[f"/branches/{branch}/protection"] = {}

        def run(command, **kwargs):
            self.assertEqual(command[:4], ["gh", "api", "--method", "GET"])
            prefix = f"repos/{checker.REPO}"
            self.assertTrue(command[4].startswith(prefix))
            path = command[4][len(prefix):]
            if path.endswith("/protection") and classic_status != 200:
                return subprocess.CompletedProcess(command, 1, "", f"gh: unavailable (HTTP {classic_status})")
            return subprocess.CompletedProcess(command, 0, json.dumps(endpoints[path]), "")

        return mock.patch.object(checker.subprocess, "run", side_effect=run)

    def test_dry_run_and_live_policy_are_distinct(self):
        with self.fake_github() as run:
            status, output = self.call("--dry-run")
            self.assertEqual(status, 0, output)
            self.assertIn("applied rules remain unverified", output)
            self.assertIn("detailed classic protection unavailable (HTTP 403)", output)
            self.assertEqual(run.call_count, 8)
            for branch in checker.BRANCHES:
                self.assertIn(f"{branch}: PASS (proposed policy)", output)
            self.assertEqual(self.call()[0], 1)
        with self.fake_github(applied=True):
            self.assertEqual(self.call()[0], 0)

    def test_dry_run_fails_when_proposal_would_remove_live_protection(self):
        self.current["conditions"]["ref_name"]["include"].append("refs/heads/stable")
        with self.fake_github():
            status, output = self.call("--dry-run")
        self.assertEqual(status, 1)
        self.assertIn("review drift", output)

    def test_live_rejects_unreviewed_scope_even_when_both_branches_pass(self):
        self.proposal["conditions"]["ref_name"]["exclude"].append("refs/heads/release-branch")
        with self.fake_github(applied=True):
            status, output = self.call()
        for branch in checker.BRANCHES:
            self.assertIn(f"{branch}: PASS (live policy)", output)
        self.assertEqual(status, 1, output)
        self.assertIn("live ruleset differs from the reviewed proposal", output)

    def test_live_fails_if_classic_protection_exists_or_has_an_unexpected_error(self):
        for classic_status in (200, 500):
            with self.subTest(classic_status=classic_status), self.fake_github(
                applied=True, classic_status=classic_status
            ):
                status, output = self.call()
                self.assertEqual(status, 1)
                self.assertIn(f"HTTP {classic_status}", output)
