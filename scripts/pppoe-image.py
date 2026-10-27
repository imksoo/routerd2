#!/usr/bin/env python3
"""Build the pinned PPP runtime and its corresponding source bundle from APKs.

The input is an Alpine package directory, never an executable run by
this tool. Sources come from each installed package's exact aports commit and
are checked against that APKBUILD's SHA512 values. Runtime lookup uses only the
small regular-file image; source/build materials travel as a separate artifact.
"""
import argparse
import hashlib
import io
import json
from pathlib import Path
import re
import shutil
import tarfile
import tempfile
import urllib.request


def fetch(url, dest):
    if dest.is_file():
        return
    dest.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=dest.parent, delete=False) as tmp:
        temporary = Path(tmp.name)
        try:
            with urllib.request.urlopen(url, timeout=30) as response:
                shutil.copyfileobj(response, tmp)
            tmp.close()
            temporary.replace(dest)
        finally:
            temporary.unlink(missing_ok=True)


def add(tar, name, data, mode=0o444):
    h = tarfile.TarInfo(name)
    h.size, h.mode = len(data), mode
    tar.addfile(h, io.BytesIO(data))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--root", type=Path, required=True)
    p.add_argument("--arch", choices=("amd64", "arm64"), required=True)
    p.add_argument("--cache", type=Path, default=Path("dist/ppp-image-sources"))
    p.add_argument("--output", type=Path, default=Path("dist/pppoe-engine.tar"))
    p.add_argument("--sources-output", type=Path, default=Path("dist/pppoe-sources.tar"))
    args = p.parse_args()
    machine = {"amd64": "x86_64", "arm64": "aarch64"}[args.arch]
    runtime = {}
    for name in ["usr/sbin/pppd", "usr/lib/pppd/2.5.3/pppoe.so", "lib/ld-musl-" + machine + ".so.1", "lib/libc.musl-" + machine + ".so.1", "usr/lib/libpcap.so.1", "usr/lib/libssl.so.3", "usr/lib/libcrypto.so.3", "usr/lib/ossl-modules/legacy.so"]:
        runtime[name] = (args.root / name).read_bytes()
    packages = []
    for paragraph in (args.root / "lib/apk/db/installed").read_text().split("\n\n"):
        fields = dict(line.split(":", 1) for line in paragraph.splitlines() if len(line) > 1 and line[1] == ":")
        if fields.get("P") in ["ppp-daemon", "ppp-pppoe", "musl", "libpcap", "libssl3", "libcrypto3"]:
            packages.append({"name": fields["P"], "version": fields["V"], "origin": fields["o"], "aports_commit": fields["c"], "license": fields["L"]})
    if len(packages) != 6:
        raise ValueError("missing PPP package provenance")
    archives = {}
    materials = {}
    urls = {"ppp": "https://download.samba.org/pub/ppp/ppp-{v}.tar.gz", "musl": "https://musl.libc.org/releases/musl-{v}.tar.gz", "libpcap": "https://www.tcpdump.org/release/libpcap-{v}.tar.gz", "openssl": "https://github.com/openssl/openssl/releases/download/openssl-{v}/openssl-{v}.tar.gz"}
    for package in packages:
        origin = package["origin"]
        if origin in archives:
            continue
        version = package["version"].rsplit("-r", 1)[0]
        base = "https://raw.githubusercontent.com/alpinelinux/aports/" + package["aports_commit"] + "/main/" + origin + "/"
        cache = args.cache / origin / package["aports_commit"]
        apkbuild = cache / "APKBUILD"
        fetch(base + "APKBUILD", apkbuild)
        recipe = apkbuild.read_text()
        if not re.search(r"^pkgver=" + re.escape(version) + r"$", recipe, re.M):
            raise ValueError("package source version mismatch: " + origin)
        materials[origin + "/APKBUILD"] = apkbuild
        upstream = origin + "-" + version + ".tar.gz"
        for checksum, name in re.findall(r"^([0-9a-f]{128})  (\S+)$", recipe, re.M):
            if Path(name).name != name:
                raise ValueError("unexpected package source path")
            file = cache / name
            fetch(urls[origin].format(v=version) if name == upstream else base + name, file)
            with file.open("rb") as f:
                if hashlib.file_digest(f, "sha512").hexdigest() != checksum:
                    raise ValueError("package source checksum mismatch: " + name)
            materials[origin + "/" + name] = file
        archives[origin] = (cache / upstream, version)
    licenses = {"ppp": ["COPYING", "LICENSE.BSD", "LICENSE.GPL-2"], "musl": ["COPYRIGHT"], "libpcap": ["LICENSE", "CREDITS"], "openssl": ["LICENSE.txt"]}
    for origin, names in licenses.items():
        archive, version = archives[origin]
        with tarfile.open(archive) as source:
            for name in names:
                runtime["usr/share/licenses/ppp-engine/" + origin + "/" + name] = source.extractfile(origin + "-" + version + "/" + name).read()
    runtime["etc/passwd"] = b"root:x:0:0:root:/root:/bin/false\n"
    inventory = (json.dumps({"format": 1, "architecture": args.arch, "packages": packages}, sort_keys=True, indent=2) + "\n").encode()
    runtime["usr/share/routerd/ppp-image.json"] = inventory
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(args.output, "w", format=tarfile.USTAR_FORMAT) as out:
        for name, data in sorted(runtime.items()):
            add(out, name, data, 0o555 if data.startswith(b"\x7fELF") else 0o444)
    args.sources_output.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(args.sources_output, "w", format=tarfile.USTAR_FORMAT) as out:
        add(out, "packages.json", inventory)
        for name, file in sorted(materials.items()):
            h = tarfile.TarInfo(name)
            h.size, h.mode = file.stat().st_size, 0o444
            with file.open("rb") as f:
                out.addfile(h, f)
    print("Built", args.output, "and corresponding sources", args.sources_output, flush=True)


if __name__ == "__main__":
    main()
