#!/usr/bin/env python3
"""Synchronize release references from VERSION, or stage tag-specific artifacts.

Requires Python 3.9+, Go, Git and Helm. Artifact mode also requires kustomize.
All rendering and validation happen in a temporary directory before writes.
"""

import argparse
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parent.parent
BASE = r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)"
RELEASE = BASE + r"(?:-rc\.[1-9][0-9]*)?"
IMAGE = re.compile(
    r"ghcr\.io/robertherbaugh/kudurru(?P<program>-dnsdist|-keepalived)?:"
    r"(?P<tag>[A-Za-z0-9_.-]+)(?![A-Za-z0-9_.-]|@)"
)
CHARTS = ("deploy/helm/kudurru/Chart.yaml", "deploy/helm/kudurru-crds/Chart.yaml")
TREES = ("deploy/helm", "deploy/kustomize", "deploy/compose")


def run(args, cwd, **kwargs):
    return subprocess.run(args, cwd=cwd, check=True, **kwargs)


def read(root, name):
    return (root / name).read_text()


def replace_one(text, pattern, replacement):
    result, count = re.subn(pattern, replacement, text, flags=re.MULTILINE)
    if count != 1:
        raise ValueError(f"expected one occurrence of {pattern!r}, found {count}")
    return result


def validate_version(value):
    if not re.fullmatch(BASE, value):
        raise ValueError("VERSION must be MAJOR.MINOR.PATCH, without v or a suffix")
    return value


def artifact_version(value, version):
    value = value.removeprefix("v")
    if re.fullmatch(re.escape(version) + r"(?:-rc\.[1-9][0-9]*)?", value):
        return value
    # GoReleaser's local --snapshot version is not a published release tag.
    # Its base comes from the nearest tag, which may predate VERSION.
    if re.fullmatch(RELEASE + r"-SNAPSHOT-[0-9a-f]+", value):
        return value
    raise ValueError("artifact version must match VERSION (optionally -rc.N), or a GoReleaser snapshot")


def programs(root):
    dns = re.search(r"^FROM powerdns/dnsdist-\d+:([^@\s]+)@sha256:", read(root, "images/dnsdist/Dockerfile"), re.M)
    keep = re.search(r"^ARG KEEPALIVED_VERSION=(\S+)$", read(root, "images/keepalived/Dockerfile"), re.M)
    if not dns or not keep:
        raise ValueError("cannot read pinned supervisor versions from their Dockerfiles")
    return {"-dnsdist": dns[1], "-keepalived": keep[1]}


def references(text, version, supervisors):
    def image(match):
        program, tag = match["program"], match["tag"]
        # Keep dev/test tags and digest-pinned examples as written.
        expected = (r"[0-9][A-Za-z0-9_.-]*-k" + RELEASE) if program else RELEASE + r"(?:-debug|-hostnetwork)?"
        if not re.fullmatch(expected, tag):
            return match[0]
        suffix = ""
        if program:
            suffix = supervisors[program] + "-k"
        else:
            suffix = "-debug" if tag.endswith("-debug") else "-hostnetwork" if tag.endswith("-hostnetwork") else ""
        target = suffix + version if program else version + suffix
        return f"ghcr.io/robertherbaugh/kudurru{program or ''}:{target}"

    text = IMAGE.sub(image, text)
    text = re.sub(
        r"(oci://ghcr\.io/robertherbaugh/charts/kudurru(?:-crds)? --version )" + RELEASE + r"(?![A-Za-z0-9_.-])",
        lambda m: m[1] + version, text,
    )
    text = re.sub(
        r"(https://github\.com/robertherbaugh/kudurru/releases/download/v)" + RELEASE + r"(?=/)",
        lambda m: m[1] + version, text,
    )
    text = re.sub(
        r"(github\.com/robertherbaugh/kudurru/deploy/kustomize/[A-Za-z0-9_/-]+\?ref=v)" + RELEASE + r"(?![A-Za-z0-9_.-])",
        lambda m: m[1] + version, text,
    )
    return text


def tracked_files(root):
    output = run(["git", "ls-files", "-z"], root, stdout=subprocess.PIPE).stdout
    return [Path(p.decode()) for p in output.split(b"\0") if p]


def documentation(path):
    name = path.as_posix()
    return path.suffix == ".md" and (
        name == "README.md" or name.startswith(("docs/deploy/", "docs/guide/", "docs-site/content/start/", "deploy/"))
    )


def changes(root, version, files, supervisors):
    result = {"VERSION": version + "\n"}
    for name in CHARTS:
        text = read(root, name)
        for field in ("version", "appVersion"):
            text = replace_one(text, rf"^{field}: .+$", f"{field}: {version}")
        result[name] = text
    name = "deploy/helm/kudurru/values.yaml"
    text = read(root, name)
    for field, program in (("dnsdistVersion", "-dnsdist"), ("keepalivedVersion", "-keepalived")):
        text = replace_one(text, rf"^(\s+){field}: .+$", lambda m: f"{m[1]}{field}: {supervisors[program]}")
    result[name] = text
    for path in files:
        if documentation(path) or (path.as_posix().startswith("deploy/compose/") and path.name == "docker-compose.yml"):
            result[path.as_posix()] = references(read(root, path), version, supervisors)
    name = "CHANGELOG.md"
    text = read(root, name)
    if not re.search(rf"^## \[{re.escape(version)}\](?: |$)", text, re.M):
        text = replace_one(text, r"^## \[Unreleased\]$", f"## [Unreleased]\n\n## [{version}] - unreleased")
    result[name] = text
    return result


def stage(root, destination, files, planned):
    # Only tracked files: local Compose secrets/data must never enter an archive.
    for path in files:
        if any(path.as_posix().startswith(tree + "/") for tree in TREES):
            source = root / path
            if source.is_symlink():
                raise ValueError(f"deployment input must be a regular file: {path}")
            target = destination / path
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target)
    for name, text in planned.items():
        if name.startswith("deploy/"):
            target = destination / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text)


def render(root, destination, scratch):
    if not shutil.which("helm"):
        raise ValueError("helm is required to regenerate Kustomize from the charts")
    binary = scratch / "deploy-tests"
    run(["go", "test", "-c", "-o", str(binary), "./deploy"], root)
    run([str(binary), "-test.run=^TestKustomizeBases$", "-update"], destination / "deploy")


def apply(root, planned, original):
    dirty = [name for name, text in planned.items() if original[name] != text]
    # Detect concurrent edits before changing any file.
    for name in original:
        if read(root, name) != original[name]:
            raise ValueError(f"file changed during synchronization: {name}")
    written = []
    try:
        for name in dirty:
            written.append(name)
            (root / name).write_text(planned[name])
    except OSError:
        for name in written:
            (root / name).write_text(original[name])
        raise
    return dirty


def artifacts(root, destination, scratch, version, supervisors):
    if not shutil.which("kustomize"):
        raise ValueError("kustomize is required (make deploy-tools)")
    output = scratch / "release"
    output.mkdir()
    for name, source in (("crds.yaml", "crd"), ("install.yaml", "default")):
        body = run(["kustomize", "build", str(destination / "deploy/kustomize" / source)], root, stdout=subprocess.PIPE).stdout
        (output / name).write_bytes(body)
    shutil.copytree(destination / "deploy/compose", output / "compose")
    for path in (output / "compose").rglob("*"):
        if path.is_file() and path.suffix in (".md", ".yml", ".yaml"):
            path.write_text(references(path.read_text(), version, supervisors))
    target = root / "bin/release"
    target.parent.mkdir(exist_ok=True)
    if target.is_symlink():
        raise ValueError("bin/release must not be a symlink")
    # The output is wholly generated; replace it only after both builds pass.
    if target.exists():
        shutil.rmtree(target)
    shutil.copytree(output, target)
    print(f"Staged {version} manifests and Compose bundle in bin/release")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", help="set VERSION and synchronize (default: read VERSION)")
    parser.add_argument("--check", action="store_true", help="report drift without changing files")
    parser.add_argument("--artifact-version", help="stage tag-specific release artifacts only, without editing tracked files")
    args = parser.parse_args(argv)
    if args.artifact_version and (args.version or args.check):
        parser.error("--artifact-version cannot be combined with --version or --check")
    version = validate_version(args.version if args.version is not None else read(ROOT, "VERSION").strip())
    supervisors = programs(ROOT)
    files = tracked_files(ROOT)
    if args.artifact_version:
        version = artifact_version(args.artifact_version, version)
    planned = changes(ROOT, version, files, supervisors)
    inputs = set(planned) | {p.as_posix() for p in files if any(p.as_posix().startswith(tree + "/") for tree in TREES)}
    original = {name: read(ROOT, name) for name in inputs}
    with tempfile.TemporaryDirectory(prefix="kudurru-version-") as tmp:
        scratch = Path(tmp)
        destination = scratch / "tree"
        stage(ROOT, destination, files, planned)
        render(ROOT, destination, scratch)
        if args.artifact_version:
            artifacts(ROOT, destination, scratch, version, supervisors)
            return 0
        for path in (destination / "deploy/kustomize").rglob("*.yaml"):
            name = path.relative_to(destination).as_posix()
            if name not in original:
                raise ValueError(f"unexpected generated file: {name}")
            planned[name] = path.read_text()
        dirty = [name for name, text in planned.items() if original[name] != text]
        if args.check:
            for name in dirty:
                print(f"out of date: {name}")
            return int(bool(dirty))
        for name in apply(ROOT, planned, original):
            print(f"updated {name}")
    print(f"Release references match VERSION={version}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        print(f"sync-version: {exc}", file=sys.stderr)
        sys.exit(1)
