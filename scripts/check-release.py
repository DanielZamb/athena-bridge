"""Check a release tag against the CLI version and changelog, then print its release notes."""

import argparse
import hashlib
from pathlib import Path
import re


ROOT = Path(__file__).resolve().parents[1]
TARGETS = [("darwin", "amd64"), ("darwin", "arm64"), ("linux", "amd64"), ("linux", "arm64"),
    ("windows", "amd64"), ("windows", "arm64")]


def source_version():
    match = re.search(r'^const version = "([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)"$',
        (ROOT / "cmd/athena-bridge/usage.go").read_text(encoding="utf-8"), re.MULTILINE)
    if match is None:
        raise SystemExit("Cannot find the CLI version in cmd/athena-bridge/usage.go")
    return match.group(1)


def release_notes(version):
    changelog = (ROOT / "CHANGELOG.md").read_text(encoding="utf-8")
    match = re.search(rf"^## {re.escape(version)}(?: .*)?\n(.*?)(?=^## |\Z)", changelog, re.MULTILINE | re.DOTALL)
    if match is None or not match.group(1).strip():
        raise SystemExit(f"Add a '## {version}' section with release notes to CHANGELOG.md")
    return match.group(1).strip() + "\n"


def check_assets(directory, version):
    expected = set()
    for target_os, target_arch in TARGETS:
        archive = f"athena-bridge_{version}_{target_os}_{target_arch}.{'zip' if target_os == 'windows' else 'tar.gz'}"
        expected.update((archive, f"{archive}.sha256"))
    found = {path.name for path in directory.iterdir() if path.is_file()}
    if found != expected:
        raise SystemExit(f"Release assets differ. Missing: {sorted(expected - found)}; unexpected: {sorted(found - expected)}")
    for name in sorted(expected):
        if name.endswith(".sha256"):
            archive = name.removesuffix(".sha256")
            digest = hashlib.sha256((directory / archive).read_bytes()).hexdigest()
            if (directory / name).read_text(encoding="utf-8") != f"{digest}  {archive}\n":
                raise SystemExit(f"Release checksum mismatch: {archive}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("tag", help="Release tag, for example v0.1.0")
    parser.add_argument("--assets", type=Path, help="Directory that must contain exactly the six archives and checksums")
    args = parser.parse_args()
    version = source_version()
    if args.tag != f"v{version}":
        raise SystemExit(f"Tag {args.tag} does not match CLI version {version}; expected v{version}")
    notes = release_notes(version)
    if args.assets is not None:
        check_assets(args.assets.resolve(), version)
    print(notes, end="")


if __name__ == "__main__":
    main()
