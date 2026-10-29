#!/usr/bin/env python3
"""Bundle chronyd, its ELF closure, notices and exact corresponding sources."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tarfile

from engine_sources import add, fetch


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    parser.add_argument("--cache", type=Path, default=Path("/sources"))
    parser.add_argument("--output", type=Path, default=Path("/out"))
    args = parser.parse_args()
    runtime = {}
    pending = ["usr/sbin/chronyd"]
    while pending:
        name = pending.pop()
        if name in runtime:
            continue
        file = args.root / name
        runtime[name] = file.read_bytes()
        dynamic = subprocess.check_output(["readelf", "-dW", str(file)], text=True)
        for library in re.findall(r"\(NEEDED\).*\[(.*?)\]", dynamic):
            location = next((directory + "/" + library for directory in ("lib", "usr/lib") if (args.root / directory / library).is_file()), None)
            if location is None:
                raise ValueError("missing chronyd library " + library)
            pending.append(location)
        headers = subprocess.check_output(["readelf", "-lW", str(file)], text=True)
        pending.extend(name.lstrip("/") for name in re.findall(r"Requesting program interpreter: (.*?)\]", headers))
    packages = []
    for paragraph in (args.root / "lib/apk/db/installed").read_text().split("\n\n"):
        fields = dict(line.split(":", 1) for line in paragraph.splitlines() if len(line) > 1 and line[1] == ":")
        if fields.get("P") in ("chrony", "chrony-common", "musl", "libcap2", "libseccomp"):
            packages.append({"name": fields["P"], "version": fields["V"], "origin": fields["o"], "aports_commit": fields["c"], "license": fields["L"]})
    if len(packages) != 5:
        raise ValueError("missing chronyd package provenance")
    materials = {"routerd/" + name: Path(__file__).parent / name for name in ("chrony-image.py", "engine_sources.py", "chrony-engine.Dockerfile")}
    origins = set()
    for package in packages:
        origin = package["origin"]
        if origin in origins:
            continue
        origins.add(origin)
        base = "https://raw.githubusercontent.com/alpinelinux/aports/" + package["aports_commit"] + "/main/" + origin + "/"
        cache = args.cache / origin / package["aports_commit"]
        recipe = cache / "APKBUILD"
        fetch(base + "APKBUILD", recipe)
        text = recipe.read_text()
        materials[origin + "/APKBUILD"] = recipe
        notices = 0
        for checksum, name in re.findall(r'([0-9a-f]{128})  ([^"\s]+)', text):
            file = cache / name
            archive = bool(re.search(r"\.tar\.(gz|xz|bz2)$", name))
            url = "https://distfiles.alpinelinux.org/distfiles/v3.24/" + name if archive else base + name
            fetch(url, file)
            with file.open("rb") as source:
                if hashlib.file_digest(source, "sha512").hexdigest() != checksum:
                    raise ValueError("source checksum mismatch: " + name)
            materials[origin + "/" + name] = file
            if archive:
                with tarfile.open(file) as source:
                    for member in source.getmembers():
                        if member.isfile() and Path(member.name).name in ("COPYING", "COPYING.LIB", "LICENSE", "License", "COPYRIGHT"):
                            runtime["usr/share/licenses/chrony-engine/" + origin + "/" + member.name] = source.extractfile(member).read()
                            notices += 1
        if not notices:
            raise ValueError("missing source license notice for " + origin)
    runtime["etc/passwd"] = b"root:x:0:0:root:/:/sbin/nologin\n"
    runtime["etc/group"] = b"root:x:0:\n"
    inventory = (json.dumps({"format": 1, "architecture": args.arch, "packages": packages}, sort_keys=True, indent=2) + "\n").encode()
    runtime["usr/share/routerd/chrony-image.json"] = inventory
    args.output.mkdir(parents=True, exist_ok=True)
    with tarfile.open(args.output / "chrony-engine.tar", "w", format=tarfile.USTAR_FORMAT) as out:
        for name, data in sorted(runtime.items()):
            add(out, name, data, 0o555 if data.startswith(b"\x7fELF") else 0o444)
    with tarfile.open(args.output / "chrony-sources.tar", "w", format=tarfile.USTAR_FORMAT) as out:
        add(out, "packages.json", inventory)
        for name, file in sorted(materials.items()):
            add(out, name, file.read_bytes())
    print("Built chronyd runtime and corresponding sources", flush=True)


if __name__ == "__main__":
    main()
