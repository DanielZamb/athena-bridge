"""Build a compact CLI distribution with agent instructions and the example worker."""

import argparse
import gzip
import hashlib
import io
import os
from pathlib import Path
import re
import stat
import subprocess
import tarfile
import tempfile
import zipfile


ROOT = Path(__file__).resolve().parents[1]


def go(*arguments, env):
    return subprocess.check_output(["go", *arguments], cwd=ROOT, env=env, encoding="utf-8").strip()


def notices(env):
    goroot = Path(go("env", "GOROOT", env=env))
    sections = ["Third-party notices for Athena Bridge\n",
        f"Go standard library {go('env', 'GOVERSION', env=env)}\n\n{(goroot / 'LICENSE').read_text(encoding='utf-8')}"]
    dependencies = go("list", "-deps", "-f", "{{with .Module}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}",
        "./cmd/athena-bridge", env=env)
    for dependency in sorted(set(dependencies.splitlines())):
        if not dependency:
            continue
        name, version, directory = dependency.split("\t")
        if not version:
            continue
        files = sorted(path for path in Path(directory).iterdir() if path.is_file()
            and path.name.upper().startswith(("LICENSE", "COPYING", "COPYRIGHT", "NOTICE", "AUTHORS")))
        if not files:
            raise SystemExit(f"No license found for {name} {version}")
        for path in files:
            sections.append(f"{name} {version} / {path.name}\n\n{path.read_text(encoding='utf-8')}")
    return "\n\n".join(sections).encode()


def write_archive(path, files, windows):
    if windows:
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
            for name, data, mode in files:
                entry = zipfile.ZipInfo(f"athena-bridge/{name}", date_time=(1980, 1, 1, 0, 0, 0))
                entry.create_system = 3
                entry.external_attr = (stat.S_IFREG | mode) << 16
                entry.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(entry, data, compresslevel=9)
    else:
        with path.open("wb") as output:
            with gzip.GzipFile(filename="", fileobj=output, mode="wb", compresslevel=9, mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
                    for name, data, mode in files:
                        entry = tarfile.TarInfo(f"athena-bridge/{name}")
                        entry.size = len(data)
                        entry.mode = mode
                        archive.addfile(entry, io.BytesIO(data))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--os", choices=("darwin", "linux", "windows"), help="Target OS, default: go env GOOS")
    parser.add_argument("--arch", choices=("amd64", "arm64"), help="Target architecture, default: go env GOARCH")
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    args = parser.parse_args()
    env = dict(os.environ, CGO_ENABLED="0")
    target_os = args.os or go("env", "GOOS", env=env)
    target_arch = args.arch or go("env", "GOARCH", env=env)
    if target_os not in ("darwin", "linux", "windows") or target_arch not in ("amd64", "arm64"):
        parser.error("Supported targets are darwin, linux, or windows on amd64 or arm64")
    env.update(GOOS=target_os, GOARCH=target_arch)
    version = re.search(r'^const version = "([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)"$',
        (ROOT / "cmd/athena-bridge/usage.go").read_text(encoding="utf-8"), re.MULTILINE).group(1)
    windows = target_os == "windows"
    binary_name = "athena-bridge.exe" if windows else "athena-bridge"
    extension = "zip" if windows else "tar.gz"
    archive_name = f"athena-bridge_{version}_{target_os}_{target_arch}.{extension}"
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".athena-release-", dir=output) as directory:
        temporary = Path(directory)
        binary = temporary / binary_name
        subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w",
            "-o", str(binary), "./cmd/athena-bridge"], cwd=ROOT, env=env, check=True)
        files = [(binary_name, binary.read_bytes(), 0o755)]
        for source, destination in (
            ("docs/distribution.md", "README.md"),
            ("skills/athena-bridge/SKILL.md", "SKILL.md"),
            ("LICENSE", "LICENSE"),
            ("docs/lambda-contract.md", "docs/lambda-contract.md"),
            ("docs/troubleshooting.md", "docs/troubleshooting.md"),
            ("sandbox/worker.py", "worker/worker.py"),
            ("examples/worker.md", "worker/README.md"),
        ):
            files.append((destination, (ROOT / source).read_bytes(), 0o644))
        files.append(("THIRD_PARTY_NOTICES.txt", notices(env), 0o644))
        archive = temporary / archive_name
        write_archive(archive, sorted(files), windows)
        checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
        receipt = temporary / f"{archive_name}.sha256"
        receipt.write_bytes(f"{checksum}  {archive_name}\n".encode())
        archive.replace(output / archive.name)
        receipt.replace(output / receipt.name)
    print(output / archive_name)
    print(output / f"{archive_name}.sha256")


if __name__ == "__main__":
    main()
