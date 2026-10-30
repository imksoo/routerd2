#!/usr/bin/env python3
"""Collect pinned nDPI and matching Alpine crypto-library sources and notices."""
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
    args = parser.parse_args()
    output = Path("/out")
    output.mkdir(exist_ok=True)
    notices = output / "ndpi-notices"
    notices.mkdir(exist_ok=True)
    source = Path("/sources/nDPI-4.2.tar.gz")
    if not source.exists():
        fetch("https://codeload.github.com/ntop/nDPI/tar.gz/refs/tags/4.2", source)
    digest = hashlib.sha256(source.read_bytes()).hexdigest()
    if digest != "e54ce8fe13adc5d747be7553513657fae78f796e0bd459e122c280cc06ce4daf":
        raise ValueError("nDPI source checksum mismatch")
    packages = [{"name": "nDPI", "version": "4.2", "sha256": digest}]
    materials = {"nDPI-4.2.tar.gz": source}
    with tarfile.open(source) as archive:
        (notices / "nDPI-COPYING").write_bytes(archive.extractfile("nDPI-4.2/COPYING").read())
    gpl = Path(__file__).resolve().parents[1] / "packaging/alpine/licenses/GPL-3.0.txt"
    if hashlib.sha256(gpl.read_bytes()).hexdigest() != "3972dc9744f6499f0f9b2dbf76696f2ae7ad8af9b23dde66d6af86c9dfb36986":
        raise ValueError("GPL-3.0 notice checksum mismatch")
    (notices / "GPL-3.0.txt").write_bytes(gpl.read_bytes())
    materials["GPL-3.0.txt"] = gpl
    installed = (args.root / "lib/apk/db/installed").read_text()
    for paragraph in installed.split("\n\n"):
        fields = dict(line.split(":", 1) for line in paragraph.splitlines() if len(line) > 1 and line[1] == ":")
        if fields.get("P") not in ("libgcrypt-static", "libgpg-error-static"):
            continue
        origin = fields["o"]
        package = {"name": fields["P"], "version": fields["V"], "origin": origin, "aports_commit": fields["c"], "license": fields["L"]}
        packages.append(package)
        version = fields["V"].rsplit("-r", 1)[0]
        base = "https://raw.githubusercontent.com/alpinelinux/aports/"+fields["c"]+"/main/"+origin+"/"
        cache = Path("/sources") / origin
        recipe = cache / "APKBUILD"
        fetch(base+"APKBUILD", recipe)
        materials[origin+"/APKBUILD"] = recipe
        for checksum, name in re.findall(r'([0-9a-f]{128})  ([^"\s]+)', recipe.read_text()):
            path = cache / name
            is_source = name.startswith(origin+"-"+version+".tar.")
            url = "https://www.gnupg.org/ftp/gcrypt/"+origin+"/"+name if is_source else base+name
            fetch(url, path)
            if hashlib.sha512(path.read_bytes()).hexdigest() != checksum:
                raise ValueError("source checksum mismatch: "+name)
            materials[origin+"/"+name] = path
            if is_source:
                with tarfile.open(path) as archive:
                    for entry in archive.getmembers():
                        if entry.isfile() and entry.name.count("/") == 1 and Path(entry.name).name.startswith("COPYING"):
                            (notices / (origin+"-"+Path(entry.name).name)).write_bytes(archive.extractfile(entry).read())
    if len(packages) != 3:
        raise ValueError("missing static crypto package provenance")
    # Zig supplies the statically linked musl and compiler-rt implementation.
    installed_tools = Path("/lib/apk/db/installed").read_text()
    zig = None
    for paragraph in installed_tools.split("\n\n"):
        fields = dict(line.split(":", 1) for line in paragraph.splitlines() if len(line) > 1 and line[1] == ":")
        if fields.get("P") == "zig":
            zig = fields
            break
    if zig is None:
        raise ValueError("missing Zig runtime source provenance")
    base = "https://raw.githubusercontent.com/alpinelinux/aports/"+zig["c"]+"/community/zig/"
    recipe = Path("/sources/zig/APKBUILD")
    fetch(base+"APKBUILD", recipe)
    materials["zig/APKBUILD"] = recipe
    version = zig["V"].rsplit("-r", 1)[0]
    for checksum, name in re.findall(r'([0-9a-f]{128})  ([^"\s]+)', recipe.read_text()):
        path = Path("/sources/zig") / name
        is_source = name == "zig-"+version+".tar.xz"
        url = "https://ziglang.org/download/"+version+"/"+name if is_source else base+name
        fetch(url, path)
        if hashlib.sha512(path.read_bytes()).hexdigest() != checksum:
            raise ValueError("source checksum mismatch: "+name)
        materials["zig/"+name] = path
        if is_source:
            with tarfile.open(path) as archive:
                for entry in archive.getmembers():
                    if entry.isfile() and re.fullmatch(r"(?:LICENSE|COPYRIGHT|COPYING)(?:\..*)?", Path(entry.name).name):
                        (notices / entry.name.replace("/", "-")).write_bytes(archive.extractfile(entry).read())
    packages.append({"name": "zig", "version": zig["V"], "aports_commit": zig["c"],
                     "usage": "musl and compiler-rt static runtime", "license": zig["L"]})
    inventory = (json.dumps({"architecture": args.arch, "packages": packages}, indent=2, sort_keys=True)+"\n").encode()
    (notices/"manifest.json").write_bytes(inventory)
    for name in ("ndpi-sources.py", "engine_sources.py"):
        materials["routerd/"+name] = Path(__file__).parent / name
    with tarfile.open(output/"ndpi-sources.tar", "w", format=tarfile.USTAR_FORMAT) as archive:
        add(archive, "packages.json", inventory)
        for name, path in sorted(materials.items()):
            add(archive, name, path.read_bytes())


if __name__ == "__main__":
    main()
