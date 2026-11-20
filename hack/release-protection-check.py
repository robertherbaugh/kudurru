"""Verify release protections with GET requests or check a proposed change locally."""

import argparse
import copy
import json
from pathlib import Path
import re
import subprocess
import sys


REPO = "robertherbaugh/kudurru"
RULESET = 24898698
BRANCHES = ("release-candidate", "release")
ACTIONS = 15368
CHECKS = {"ci-ok", "security-ok", "promotion"}
ADMIN_BYPASS = {"actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "pull_request"}
EDITABLE = ("name", "target", "enforcement", "conditions", "rules", "bypass_actors")
PROPOSAL = Path(__file__).resolve().parents[1] / ".github/rulesets/protect-releases.json"


class GitHubError(ValueError):
    def __init__(self, endpoint, stderr):
        super().__init__(f"GET {endpoint}: {stderr.strip()}")
        status = re.search(r"\(HTTP (\d{3})\)", stderr)
        self.status = int(status[1]) if status else None


def get(path=""):
    endpoint = f"repos/{REPO}{path}"
    result = subprocess.run(
        ["gh", "api", "--method", "GET", endpoint], capture_output=True, text=True
    )
    if result.returncode:
        raise GitHubError(endpoint, result.stderr)
    return json.loads(result.stdout)


def load(path):
    return json.loads(path.read_text(encoding="utf-8"))


def scoped_rules(rules):
    return [
        {key: rule[key] for key in ("type", "parameters") if key in rule}
        for rule in rules
        if rule.get("ruleset_id") == RULESET
    ]


def parameters_by_type(rules):
    return {rule["type"]: rule.get("parameters", {}) for rule in rules}


def check_policy(rules, all_effective):
    parameters = parameters_by_type(rules)
    errors = []
    for kind in ("deletion", "non_fast_forward", "pull_request", "required_status_checks"):
        if kind not in parameters:
            errors.append(f"ruleset {RULESET}: missing {kind} rule")

    review = parameters.get("pull_request", {})
    if review.get("allowed_merge_methods") != ["merge"]:
        errors.append(f"expected merge only; found {review.get('allowed_merge_methods')}")
    if review.get("required_approving_review_count", 0) < 1:
        errors.append("at least one approving review is required")
    for key in ("require_code_owner_review", "require_last_push_approval",
                "dismiss_stale_reviews_on_push", "required_review_thread_resolution"):
        if review.get(key) is not True:
            errors.append(f"{key} must be enabled")

    checks = parameters.get("required_status_checks", {}).get("required_status_checks", [])
    actions_checks = {check["context"] for check in checks if check.get("integration_id") == ACTIONS}
    if missing := CHECKS - actions_checks:
        errors.append(f"missing required GitHub Actions checks: {', '.join(sorted(missing))}")

    for rule in all_effective:
        methods = rule.get("parameters", {}).get("allowed_merge_methods")
        if rule["type"] == "required_linear_history" or (
            rule["type"] == "pull_request" and methods is not None and "merge" not in methods
        ):
            errors.append(f"{rule['type']} in ruleset {rule['ruleset_id']} conflicts with merge commits")
    return errors


def check_classic(branch):
    path = f"/branches/{branch}"
    summary = get(path)
    classic = summary.get("protection") or {}
    enabled = classic.get("enabled")
    level = classic.get("required_status_checks", {}).get("enforcement_level")
    print(f"{branch}: protected={summary.get('protected')}, classic summary "
          f"enabled={enabled}, status-check enforcement={level}")
    try:
        get(f"{path}/protection")
    except GitHubError as error:
        if error.status not in (403, 404):
            raise
        print(f"{branch}: detailed classic protection unavailable (HTTP {error.status}); "
              "not independently verified by this credential")
        if enabled is not False or level not in (None, "off"):
            return ["branch summary does not report classic protection disabled"]
        return []
    return ["detailed classic protection exists (HTTP 200); review it before promotion"]


def expected_proposal(live):
    expected = copy.deepcopy({field: live[field] for field in EDITABLE})
    parameters = parameters_by_type(expected["rules"])
    parameters["pull_request"]["allowed_merge_methods"] = ["merge"]
    checks = parameters["required_status_checks"]["required_status_checks"]
    if not any(check["context"] == "promotion" for check in checks):
        checks.append({"context": "promotion", "integration_id": ACTIONS})
    return expected


def check_github(dry_run):
    live = get(f"/rulesets/{RULESET}")
    print(f"Ruleset {live['id']} ({live['name']}): {live['enforcement']}; "
          f"bypass actors: {json.dumps(live.get('bypass_actors'))}")
    errors = []
    if live.get("id") != RULESET or live.get("target") != "branch" or live.get("enforcement") != "active":
        errors.append("expected the active release branch ruleset")
    if live.get("bypass_actors") != [ADMIN_BYPASS]:
        errors.append("expected only the repository admin role's PR-only bypass")
    if get().get("allow_merge_commit") is not True:
        errors.append("repository settings do not allow merge commits")

    if dry_run:
        proposal = load(PROPOSAL)
        if proposal != expected_proposal(live):
            errors.append("proposal differs from live ruleset beyond promotion and merge-only; review drift")

    for branch in BRANCHES:
        effective = get(f"/rules/branches/{branch}")
        rules = scoped_rules(effective)
        branch_errors = check_classic(branch)
        if parameters_by_type(rules) != parameters_by_type(live["rules"]):
            branch_errors.append(f"effective rules do not match ruleset {RULESET} detail")
        if dry_run:
            rules = proposal["rules"]
            effective = [rule for rule in effective if rule.get("ruleset_id") != RULESET]
        branch_errors += check_policy(rules, effective)
        errors.extend(f"{branch}: {error}" for error in branch_errors)
        print(f"{branch}: {'FAIL' if branch_errors else 'PASS'} "
              f"({'proposed' if dry_run else 'live'} policy)")
    return errors


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--dry-run", action="store_true", help="compare the local proposal to live settings; GET only")
    mode.add_argument("--snapshot", type=Path, help="check an effective branch-rules JSON snapshot; offline, policy only")
    args = parser.parse_args(argv)
    try:
        if args.snapshot:
            print("Snapshot mode: checks effective policy only, not bypass or classic protection.")
            effective = load(args.snapshot)
            errors = check_policy(scoped_rules(effective), effective)
        else:
            errors = check_github(args.dry_run)
    except (OSError, ValueError, KeyError, TypeError, AttributeError) as error:
        errors = [str(error)]

    if errors:
        for error in errors:
            print(f"FAIL: {error}", file=sys.stderr)
        return 1
    if args.dry_run:
        print("PASS: proposal dry run only; no settings changed and applied rules remain unverified.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
