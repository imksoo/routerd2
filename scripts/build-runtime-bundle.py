#!/usr/bin/env python3
# SPDX-License-Identifier: BSD-3-Clause
"""Package the normal Linux build using the existing offline signed publisher."""

import argparse
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--boot", type=Path, required=True)
    parser.add_argument("--binary-dir", type=Path, required=True)
    parser.add_argument("--engine-dir", type=Path, required=True)
    parser.add_argument("--licenses", type=Path, required=True)
    parser.add_argument("--private-key-file", type=Path, required=True)
    parser.add_argument("--sequence", type=int, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    binaries = args.binary_dir.absolute()
    engines = args.engine_dir.absolute()
    boot = json.loads(args.boot.read_text())
    version = json.loads(subprocess.check_output([str(binaries / "routerd"), "version"], text=True))
    abi = version["runtime_abi"]
    artifacts = [{"name": name, "kind": "native", "command": name, "file": str(binaries / name)}
                 for name in ("routerd", "routerd-modules", "routerd-logging")]
    layout = {
        "boot_abi": abi["boot_abi"], "worker_abi": abi["worker_abi"],
        "entrypoint": "routerd",
        "roles": [{"role": role, "artifact": Path(path).name} for role, path in boot["workers"].items()],
        "engines": [], "artifacts": artifacts,
    }
    for role, path in boot.get("engines", {}).items():
        image = Path(path).name
        source = image.removesuffix("-engine.tar") + "-sources.tar"
        layout["engines"].append({"role": role, "artifact": role + "-engine", "sources": role + "-sources"})
        for kind, name in (("engine", image), ("sources", source)):
            artifacts.append({"name": role + "-" + kind, "kind": "data", "file": str(engines / name)})
    artifacts.append({"name": "ndpi-sources", "kind": "data", "file": str(engines / "ndpi-sources.tar")})
    output = args.output.absolute()
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".runtime-bundle-", dir=output.parent) as temporary:
        stage = Path(temporary)
        notices = stage / "licenses.tar"
        files = [(Path(__file__).resolve().parents[1] / "LICENSE", "licenses/routerd2/LICENSE")]
        files.extend((path, "licenses/" + str(path.relative_to(args.licenses)))
                     for path in sorted(args.licenses.rglob("*")) if path.is_file())
        with tarfile.open(notices, "w") as archive:
            for path, name in files:
                entry = archive.gettarinfo(str(path), arcname=name)
                entry.uid = entry.gid = entry.mtime = 0
                entry.uname = entry.gname = ""
                entry.mode = 0o444
                with path.open("rb") as source:
                    archive.addfile(entry, source)
        notices.chmod(0o400)
        artifacts.append({"name": "licenses", "kind": "data", "file": str(notices)})
        # The build tree may be shared. The publisher reads only this private
        # snapshot; it never signs through mutable shared-directory paths.
        for index, artifact in enumerate(artifacts):
            frozen = stage / str(index)
            shutil.copyfile(artifact["file"], frozen)
            frozen.chmod(0o500 if artifact["kind"] == "native" else 0o400)
            artifact["file"] = str(frozen)
        declaration = stage / "layout.json"
        declaration.write_text(json.dumps(layout))
        declaration.chmod(0o600)
        subprocess.run([str(binaries / "routerd-bundle"), "pack", "--layout", str(declaration),
                        "--private-key-file", str(args.private_key_file.absolute()),
                        "--release-id", version["release_id"], "--target-arch", version["goarch"],
                        "--sequence", str(args.sequence), "--output", str(output)], check=True)


if __name__ == "__main__":
    main()
