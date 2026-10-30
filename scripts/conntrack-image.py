#!/usr/bin/env python3
"""Build conntrackd's regular-file runtime and exact corresponding sources."""
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
    pending = ["usr/sbin/conntrackd"]
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
                raise ValueError("missing engine library " + library)
            pending.append(location)
        headers = subprocess.check_output(["readelf", "-lW", str(file)], text=True)
        pending.extend(name.lstrip("/") for name in re.findall(r"Requesting program interpreter: (.*?)\]", headers))

    packages = []
    for paragraph in (args.root / "lib/apk/db/installed").read_text().split("\n\n"):
        fields = dict(line.split(":", 1) for line in paragraph.splitlines() if len(line) > 1 and line[1] == ":")
        if fields.get("P") in ("conntrack-tools", "musl", "libmnl", "libnfnetlink", "libnetfilter_conntrack", "libnetfilter_cthelper", "libnetfilter_cttimeout", "libnetfilter_queue"):
            packages.append({"name": fields["P"], "version": fields["V"], "origin": fields["o"], "aports_commit": fields["c"], "license": fields["L"]})
    if len(packages) != 8:
        raise ValueError("missing conntrackd package provenance")
    materials = {"routerd/" + name: Path(__file__).parent / name for name in ("conntrack-image.py", "engine_sources.py", "conntrack-engine.Dockerfile", "conntrack-commit-result.patch", "conntrack-copy-event.patch")}
    for package in packages:
        origin = package["origin"]
        version = package["version"].rsplit("-r", 1)[0]
        base = "https://raw.githubusercontent.com/alpinelinux/aports/" + package["aports_commit"] + "/main/" + origin + "/"
        cache = args.cache / origin / package["aports_commit"]
        recipe = cache / "APKBUILD"
        fetch(base + "APKBUILD", recipe)
        text = recipe.read_text()
        if not re.search(r"^pkgver=" + re.escape(version) + r"$", text, re.M):
            raise ValueError("source version mismatch: " + origin)
        materials[origin + "/APKBUILD"] = recipe
        archive = None
        for checksum, name in re.findall(r'([0-9a-f]{128})  ([^"\s]+)', text):
            if Path(name).name != name:
                raise ValueError("unexpected source path")
            file = cache / name
            upstream = name.startswith(origin + "-" + version + ".tar.")
            if upstream:
                url = "https://musl.libc.org/releases/" + name if origin == "musl" else "https://www.netfilter.org/projects/" + origin + "/files/" + name
                archive = file
            else:
                url = base + name
            fetch(url, file)
            with file.open("rb") as source:
                if hashlib.file_digest(source, "sha512").hexdigest() != checksum:
                    raise ValueError("source checksum mismatch: " + name)
            materials[origin + "/" + name] = file
        if archive is None:
            raise ValueError("missing upstream source for " + origin)
        notice = "COPYRIGHT" if origin == "musl" else "COPYING"
        with tarfile.open(archive) as source:
            runtime["usr/share/licenses/conntrack-engine/" + origin + "/" + notice] = source.extractfile(origin + "-" + version + "/" + notice).read()
    inventory = (json.dumps({"format": 1, "architecture": args.arch, "packages": packages, "conntrackd_control_contract": "kernel-commit-result-v1", "local_build": "routerd/conntrack-engine.Dockerfile", "local_patches": ["routerd/conntrack-commit-result.patch", "routerd/conntrack-copy-event.patch"]}, sort_keys=True, indent=2) + "\n").encode()
    runtime["usr/share/routerd/conntrack-image.json"] = inventory
    runtime["etc/protocols"] = b"icmp 1 ICMP\ntcp 6 TCP\nudp 17 UDP\nipv6-icmp 58 IPv6-ICMP\n"
    args.output.mkdir(parents=True, exist_ok=True)
    with tarfile.open(args.output / "conntrack-engine.tar", "w", format=tarfile.USTAR_FORMAT) as out:
        for name, data in sorted(runtime.items()):
            add(out, name, data, 0o555 if data.startswith(b"\x7fELF") else 0o444)
    with tarfile.open(args.output / "conntrack-sources.tar", "w", format=tarfile.USTAR_FORMAT) as out:
        add(out, "packages.json", inventory)
        for name, file in sorted(materials.items()):
            header = tarfile.TarInfo(name)
            header.size, header.mode = file.stat().st_size, 0o444
            with file.open("rb") as source:
                out.addfile(header, source)
    print("Built conntrackd runtime and corresponding sources", flush=True)


if __name__ == "__main__":
    main()
