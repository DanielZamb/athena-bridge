"""Inspect a binary release and optionally run its CLI against local fixtures."""

import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import zipfile


ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path, nargs="?", help="Archive, default: the only binary archive in dist/")
    parser.add_argument("--run", action="store_true", help="Run the packaged binary on its target OS and architecture")
    args = parser.parse_args()
    if args.archive is None:
        candidates = sorted([*(ROOT / "dist").glob("athena-bridge_*.tar.gz"),
            *(ROOT / "dist").glob("athena-bridge_*.zip")])
        if len(candidates) != 1:
            parser.error("Specify an archive when dist/ does not contain exactly one binary archive")
        args.archive = candidates[0]
    archive = args.archive.resolve()
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    expected_digest = archive.with_name(archive.name + ".sha256").read_text(encoding="utf-8")
    if expected_digest != f"{digest}  {archive.name}\n":
        raise SystemExit("Release checksum mismatch")
    if archive.suffix == ".zip":
        with zipfile.ZipFile(archive) as source:
            entries = source.infolist()
            files = {entry.filename: (source.read(entry), entry.external_attr >> 16 & 0o777) for entry in entries}
        binary = "athena-bridge.exe"
    else:
        with tarfile.open(archive) as source:
            entries = source.getmembers()
            if any(not entry.isfile() for entry in entries):
                raise SystemExit("Release must contain only regular files")
            files = {entry.name: (source.extractfile(entry).read(), entry.mode) for entry in entries}
        binary = "athena-bridge"
    expected = {f"athena-bridge/{name}" for name in (binary, "README.md", "SKILL.md", "LICENSE",
        "THIRD_PARTY_NOTICES.txt", "docs/lambda-contract.md", "docs/troubleshooting.md", "worker/README.md",
        "worker/worker.py")}
    if set(files) != expected or len(entries) != len(expected):
        raise SystemExit(f"Unexpected release contents: {sorted(files)}")
    for name, (data, mode) in files.items():
        if not data or mode != (0o755 if name == f"athena-bridge/{binary}" else 0o644):
            raise SystemExit(f"Invalid release file or permissions: {name}")
    if files["athena-bridge/worker/worker.py"][0] != (ROOT / "sandbox/worker.py").read_bytes():
        raise SystemExit("Packaged worker differs from the tested example")
    if args.run:
        with tempfile.TemporaryDirectory(prefix="athena-release-proof-") as directory:
            temporary = Path(directory)
            for name, (data, mode) in files.items():
                target = temporary / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(data)
                target.chmod(mode)
            executable = temporary / "athena-bridge" / binary
            env = dict(os.environ, ATHENA_BRIDGE_USAGE_DB="off", AWS_EC2_METADATA_DISABLED="true")
            subprocess.run([str(executable), "version"], cwd=temporary, env=env, check=True, timeout=15)
            subprocess.run([str(executable), "--help"], cwd=temporary, env=env,
                check=True, stdout=subprocess.DEVNULL, timeout=15)
            subprocess.run([sys.executable, str(ROOT / "scripts/verify-local.py"), "--binary", str(executable)],
                cwd=temporary, env=env, check=True, timeout=90)
    print(f"Release verified: {archive.name}; exactly {len(files)} files; checksum and worker match.")


if __name__ == "__main__":
    main()
