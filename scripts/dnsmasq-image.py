#!/usr/bin/env python3
"""Package the existing dnsmasq engine with its matching sources and notices."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import tarfile

from engine_sources import add, fetch


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    parser.add_argument("--cache", type=Path, default=Path("/sources"))
    parser.add_argument("--output", type=Path, default=Path("/out"))
    args = parser.parse_args()
    machine = "x86_64" if args.arch == "amd64" else "aarch64"
    runtime = {name: (args.root / name).read_bytes() for name in ("usr/sbin/dnsmasq", "usr/sbin/routerd-dhcp-event", "lib/ld-musl-" + machine + ".so.1", "lib/libc.musl-" + machine + ".so.1")}
    packages = []
    for paragraph in (args.root / "lib/apk/db/installed").read_text().split("\n\n"):
        fields = dict(line.split(":", 1) for line in paragraph.splitlines() if len(line) > 1 and line[1] == ":")
        if fields.get("P") in ("dnsmasq", "musl"):
            packages.append({"name": fields["P"], "version": fields["V"], "origin": fields["o"], "aports_commit": fields["c"], "license": fields["L"]})
    if len(packages) != 2:
        raise ValueError("missing dnsmasq package provenance")
    materials = {"routerd/" + name: Path(__file__).parent / name for name in ("dnsmasq-image.py", "engine_sources.py", "dnsmasq-engine.Dockerfile")}
    hook = Path(__file__).parent / "hook"
    for source, target in (("main.go", "cmd/routerd-dhcp-event/main.go"), ("event.go", "internal/protocol/dhcpevent/event.go"),
                           ("go.mod", "go.mod"), ("go.sum", "go.sum"), ("LICENSE", "LICENSE"), ("toolchain.txt", "hook-toolchain.txt")):
        materials["routerd/" + target] = hook / source
    for name in ("LICENSE", "Go-LICENSE", "Go-PATENTS"):
        runtime["usr/share/licenses/routerd-dhcp-event/" + name] = (hook / name).read_bytes()
        materials["routerd/hook-notices/" + name] = hook / name

    for package in packages:
        origin = package["origin"]
        version = package["version"].rsplit("-r", 1)[0].replace("_p", "rel")
        base = "https://raw.githubusercontent.com/alpinelinux/aports/" + package["aports_commit"] + "/main/" + origin + "/"
        cache = args.cache / origin / package["aports_commit"]
        recipe = cache / "APKBUILD"
        fetch(base + "APKBUILD", recipe)
        materials[origin + "/APKBUILD"] = recipe
        archive = None
        for checksum, name in re.findall(r'([0-9a-f]{128})  ([^"\s]+)', recipe.read_text()):
            file = cache / name
            if name.startswith(origin + "-" + version + ".tar."):
                url = ("https://musl.libc.org/releases/" if origin == "musl" else "https://www.thekelleys.org.uk/dnsmasq/") + name
                archive = file
            else:
                url = base + name
            fetch(url, file)
            with file.open("rb") as source:
                if hashlib.file_digest(source, "sha512").hexdigest() != checksum:
                    raise ValueError("source checksum mismatch: " + name)
            materials[origin + "/" + name] = file
        if archive is None:
            raise ValueError("missing source archive for " + origin)
        notice = "COPYRIGHT" if origin == "musl" else "COPYING"
        with tarfile.open(archive) as source:
            runtime["usr/share/licenses/dnsmasq-engine/" + origin + "/" + notice] = source.extractfile(origin + "-" + version + "/" + notice).read()
    runtime["etc/passwd"] = b"root:x:0:0:root:/:/sbin/nologin\n"
    runtime["etc/group"] = b"root:x:0:\n"
    inventory = (json.dumps({"format": 1, "architecture": args.arch, "packages": packages, "hook_toolchain": (hook / "toolchain.txt").read_text().strip()}, sort_keys=True, indent=2) + "\n").encode()
    runtime["usr/share/routerd/dnsmasq-image.json"] = inventory
    args.output.mkdir(parents=True, exist_ok=True)
    with tarfile.open(args.output / "dnsmasq-engine.tar", "w", format=tarfile.USTAR_FORMAT) as out:
        for name, data in sorted(runtime.items()):
            add(out, name, data, 0o555 if data.startswith(b"\x7fELF") else 0o444)
    with tarfile.open(args.output / "dnsmasq-sources.tar", "w", format=tarfile.USTAR_FORMAT) as out:
        add(out, "packages.json", inventory)
        for name, file in sorted(materials.items()):
            add(out, name, file.read_bytes())
    print("Built dnsmasq runtime and corresponding sources", flush=True)


if __name__ == "__main__":
    main()
