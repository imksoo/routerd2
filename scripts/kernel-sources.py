#!/usr/bin/env python3
"""Collect the installed Alpine kernel's pinned recipe and checked sources."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import tarfile
import tempfile

from engine_sources import add, fetch


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    database = args.root / "lib/apk/db/installed"
    for block in database.read_text().split("\n\n"):
        fields = dict(line.split(":", 1) for line in block.splitlines() if len(line) > 1 and line[1] == ":")
        if fields.get("P") == "linux-lts":
            break
    else:
        raise ValueError("linux-lts package provenance is absent from the kernel root")
    commit, origin = fields["c"], fields["o"]
    if not re.fullmatch(r"[0-9a-f]{40}", commit) or not re.fullmatch(r"[a-z0-9_-]+", origin):
        raise ValueError("kernel APK metadata has no valid pinned source origin")
    package = {
        "name": fields["P"], "version": fields["V"], "architecture": fields["A"],
        "license_expression_from_apk": fields["L"], "origin": origin, "aports_commit": commit,
    }
    kernel_release = (args.root / "usr/share/kernel/lts/kernel.release").read_text().strip()
    kernel_config = (args.root / "boot" / ("config-" + kernel_release)).read_bytes()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".kernel-sources-", dir=args.output.parent) as temporary:
        stage = Path(temporary)
        recipe_archive = stage / "recipe.tar.gz"
        fetch(f"https://gitlab.alpinelinux.org/alpine/aports/-/archive/{commit}/aports-{commit}.tar.gz?path=main/{origin}", recipe_archive)
        recipe = {}
        with tarfile.open(recipe_archive) as archive:
            for member in archive:
                if not member.isfile():
                    continue
                path = PurePosixPath(member.name)
                if path.parts[1:3] != ("main", origin) or ".." in path.parts:
                    raise ValueError("unexpected file in the kernel recipe archive")
                recipe[str(PurePosixPath(*path.parts[3:]))] = archive.extractfile(member).read()
        checksums = re.findall(r'([0-9a-f]{128})  ([^"\s]+)', recipe["APKBUILD"].decode())
        if not checksums:
            raise ValueError("kernel recipe has no SHA512 source checksums")
        sources = {}
        for checksum, name in checksums:
            if name in recipe:
                digest = hashlib.sha512(recipe[name]).hexdigest()
            else:
                if PurePosixPath(name).name != name:
                    raise ValueError("kernel source filename must be a basename")
                file = stage / name
                fetch("https://distfiles.alpinelinux.org/distfiles/v3.24/" + name, file)
                with file.open("rb") as source:
                    digest = hashlib.file_digest(source, "sha512").hexdigest()
                sources[name] = file
            if digest != checksum:
                raise ValueError("kernel source checksum mismatch: " + name)
        output = stage / "kernel-sources.tar"
        with tarfile.open(output, "w", format=tarfile.USTAR_FORMAT) as archive:
            add(archive, "package.json", (json.dumps(package, sort_keys=True, indent=2) + "\n").encode())
            add(archive, origin + "/installed.config", kernel_config)
            for name, data in sorted(recipe.items()):
                add(archive, origin + "/" + name, data)
            for name, file in sorted(sources.items()):
                entry = tarfile.TarInfo(origin + "/" + name)
                entry.size, entry.mode = file.stat().st_size, 0o444
                with file.open("rb") as source:
                    archive.addfile(entry, source)
        output.replace(args.output)
    print("Collected kernel sources", package["version"], commit, flush=True)


if __name__ == "__main__":
    main()
