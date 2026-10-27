#!/usr/bin/env python3
"""Build the pinned static Tailscale engine and its matching source archive.

Only cmd/tailscaled enters the runtime image. Node identity and trust material
are operator state; neither peer credentials nor build-host resolver settings
are bundled. Downloaded modules use the ordinary Go cache.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

from engine_sources import add, fetch
from licenses import json_stream, notice_files

VERSION = "1.104.2"
SOURCE_SHA256 = "4c1c806e16507414a1459afcdc399f6a1323c21a1ac90f07bea0465fd8fd7e1d"
SOURCE_URL = "https://codeload.github.com/tailscale/tailscale/tar.gz/refs/tags/v" + VERSION


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default="go")
    parser.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    parser.add_argument("--cache", type=Path, default=Path("dist/peer-env/tailscale"))
    parser.add_argument("--output", type=Path, default=Path("dist"))
    args = parser.parse_args()
    # Resolve the project's selected toolchain before leaving its go.mod.
    goroot = Path(subprocess.check_output([args.go, "env", "GOROOT"], text=True).strip())
    args.go = str(goroot / "bin" / "go")
    cache, output = args.cache.resolve(), args.output.resolve()
    archive = cache / ("tailscale-" + VERSION + ".tar.gz")
    fetch(SOURCE_URL, archive)
    with archive.open("rb") as file:
        if hashlib.file_digest(file, "sha256").hexdigest() != SOURCE_SHA256:
            raise ValueError("Tailscale source checksum mismatch")
    output.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=args.arch, GOTOOLCHAIN="local")
    with tempfile.TemporaryDirectory(prefix="routerd-tailscale-build-") as directory:
        stage = Path(directory)
        with tarfile.open(archive) as source:
            source.extractall(stage, filter="data")
        source = stage / ("tailscale-" + VERSION)

        def go(*arguments):
            return subprocess.check_output([args.go, *arguments], cwd=source, env=env, text=True)

        daemon = stage / "tailscaled"
        flags = "-s -w -X tailscale.com/version.shortStamp=" + VERSION + " -X tailscale.com/version.longStamp=" + VERSION + "-routerd"
        subprocess.run([args.go, "build", "-trimpath", "-buildvcs=false", "-ldflags", flags, "-o", str(daemon), "./cmd/tailscaled"], cwd=source, env=env, check=True)
        packages = list(json_stream(go("list", "-buildvcs=false", "-deps", "-json", "./cmd/tailscaled")))
        modules = {}
        for package in packages:
            module = package.get("Module")
            if module and not module.get("Main"):
                module = module.get("Replace", module)
                modules[(module["Path"], module["Version"])] = module
        notices = ["Tailscale " + VERSION + "\n" + (source / "LICENSE").read_text()]
        materials = {"tailscale/source.tar.gz": archive, "routerd/tailscale-image.py": Path(__file__).resolve(), "routerd/engine_sources.py": Path(__file__).with_name("engine_sources.py").resolve(), "routerd/licenses.py": Path(__file__).with_name("licenses.py").resolve()}
        inventory = []
        for (path, version), module in sorted(modules.items()):
            downloaded = json.loads(go("mod", "download", "-json", path + "@" + version))
            name = "modules/" + hashlib.sha256((path + "@" + version).encode()).hexdigest() + ".zip"
            materials[name] = Path(downloaded["Zip"])
            found = notice_files(Path(module["Dir"]))
            if not found:
                raise ValueError("missing module license notices: " + path)
            for notice in found:
                notices.append(path + "@" + version + "/" + str(notice) + "\n" + (Path(module["Dir"]) / notice).read_text(errors="replace"))
            inventory.append({"module": path, "version": version, "sum": downloaded["Sum"], "source": name})
        goroot = Path(go("env", "GOROOT").strip())
        notices.append("Go toolchain\n" + (goroot / "LICENSE").read_text() + "\n" + (goroot / "PATENTS").read_text())
        metadata = (json.dumps({"version": VERSION, "architecture": args.arch, "go_version": go("version").strip(), "source_url": SOURCE_URL, "source_sha256": SOURCE_SHA256, "modules": inventory}, sort_keys=True, indent=2) + "\n").encode()
        with tarfile.open(output / "tailscale-engine.tar", "w", format=tarfile.USTAR_FORMAT) as image:
            add(image, "usr/sbin/tailscaled", daemon.read_bytes(), 0o555)
            add(image, "usr/share/licenses/tailscale/NOTICES", "\n\n".join(notices).encode())
            add(image, "usr/share/routerd/tailscale-image.json", metadata)
        with tarfile.open(output / "tailscale-sources.tar", "w", format=tarfile.USTAR_FORMAT) as sources:
            for name, file in sorted(materials.items()):
                add(sources, name, file.read_bytes())
            add(sources, "build.json", metadata)
        print("Built static Tailscale", VERSION, args.arch, "with", len(modules), "module sources and license notices")


if __name__ == "__main__":
    main()
