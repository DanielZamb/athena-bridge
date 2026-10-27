#!/usr/bin/env python3
"""Scan publishable working files and the current branch's complete Git history."""

from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[1]


def git(*arguments):
    return subprocess.check_output(["git", *arguments], cwd=ROOT)


def main():
    scanner = shutil.which("gitleaks")
    if scanner is None:
        sys.exit("Install Gitleaks 8.30.1 or later, then rerun make secrets. See CONTRIBUTING.md.")
    if git("rev-parse", "--is-shallow-repository").strip() == b"true":
        sys.exit("Fetch the complete branch history before scanning: git fetch --unshallow")
    flags = ["--config", str(ROOT / ".gitleaks.toml"), "--redact", "--no-banner"]
    with tempfile.TemporaryDirectory(prefix="athena-bridge-secrets-") as directory:
        snapshot = Path(directory)
        names = git("ls-files", "-z", "--cached", "--others", "--exclude-standard")
        for name in set(names.decode().split("\0")) - {""}:
            source = ROOT / name
            if not source.exists() and not source.is_symlink():
                continue
            destination = snapshot / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, destination, follow_symlinks=False)
        print("Scanning tracked and nonignored working files", flush=True)
        working = subprocess.run([scanner, "dir", ".", *flags], cwd=snapshot)
    print("Scanning current branch history", flush=True)
    history = subprocess.run([scanner, "git", str(ROOT), "--log-opts=HEAD", *flags], cwd=ROOT)
    return 1 if working.returncode or history.returncode else 0


if __name__ == "__main__":
    sys.exit(main())
