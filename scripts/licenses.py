#!/usr/bin/env python3
"""Collect source license notices for the complete pinned Go module graph.

This is an inventory, not an SPDX classifier or a legal compatibility verdict.
The full build graph is deliberately included; a separate package inventory
records which modules the two static Linux command builds actually import.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile


NOTICE_NAME = re.compile(
    r"^(?:licen[cs]e|copying|copyright|notice|patents)(?:$|[-_. ])", re.I
)
SOURCE_SUFFIXES = {".go", ".c", ".h", ".cc", ".cpp", ".py", ".js", ".ts", ".sh"}
SKIP_DIRECTORIES = {".git", ".hg", ".svn", "node_modules", "dist"}


def json_stream(data):
    decoder = json.JSONDecoder()
    position = 0
    while position < len(data):
        while position < len(data) and data[position].isspace():
            position += 1
        if position == len(data):
            return
        item, position = decoder.raw_decode(data, position)
        yield item


def run_go(go, *arguments, environment=None):
    result = subprocess.run(
        [go, *arguments], env=environment, check=False, capture_output=True, text=True
    )
    if result.returncode:
        raise RuntimeError(
            f"go {' '.join(arguments)} failed:\n{result.stderr.strip()}"
        )
    return result.stdout


def notice_files(root):
    """Keep nested notices too, including files inside LICENSES directories."""
    root = root.resolve(strict=True)
    found = []
    for directory, names, files in os.walk(root, followlinks=False):
        names[:] = sorted(name for name in names if name not in SKIP_DIRECTORIES)
        relative_dir = Path(directory).relative_to(root)
        license_directory = any(part.lower() == "licenses" for part in relative_dir.parts)
        for name in sorted(files):
            path = Path(directory) / name
            if not (NOTICE_NAME.match(name) or license_directory):
                continue
            if path.suffix.lower() in SOURCE_SUFFIXES:
                continue
            # A module's notice must be part of its own source archive.
            resolved = path.resolve(strict=True)
            if not resolved.is_relative_to(root) or not resolved.is_file():
                raise RuntimeError(f"notice leaves its source directory: {path}")
            found.append(path.relative_to(root))
    return sorted(found, key=str)


def copy_notices(source, destination, relative_paths):
    notices = []
    for relative in relative_paths:
        contents = (source / relative).read_bytes()
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(contents)
        target.chmod(0o644)
        notices.append({
            "source_path": relative.as_posix(),
            "sha256": hashlib.sha256(contents).hexdigest(),
            "bytes": len(contents),
        })
    return notices


def module_directory_name(path, version):
    # Human-readable prefix plus a collision-resistant identity. No module path
    # is interpreted as an output directory, even for a local replacement.
    identity = path + "@" + (version or "local")
    prefix = re.sub(r"[^A-Za-z0-9._-]", "_", identity)[:150]
    return prefix + "-" + hashlib.sha256(identity.encode()).hexdigest()[:12]


def source_tree_digest(root):
    """Hash a local replacement independently of the upstream module archive."""
    root = root.resolve(strict=True)
    files = []
    for directory, names, filenames in os.walk(root, followlinks=False):
        names[:] = sorted(name for name in names if name not in SKIP_DIRECTORIES)
        for name in names:
            if (Path(directory) / name).is_symlink():
                raise RuntimeError(f"local replacement contains a symlink directory: {name}")
        for name in sorted(filenames):
            path = Path(directory) / name
            if path.is_symlink() or not path.is_file():
                raise RuntimeError(f"local replacement contains a non-regular file: {path}")
            files.append(path.relative_to(root))
    digest = hashlib.sha256()
    for relative in sorted(files, key=str):
        digest.update(relative.as_posix().encode("utf-8") + b"\0")
        digest.update(hashlib.sha256((root / relative).read_bytes()).digest())
    return {"sha256": digest.hexdigest(), "files": len(files),
            "method": "SHA256 of sorted UTF-8 relative paths, NUL, and each file's raw SHA256 digest"}


def upstream_reference(module, source, sums):
    """Keep declared upstream identity separate from the modified source hash."""
    path, version = module["Path"], module.get("Version")
    reference = {"path": path, "version": version,
                 "sum": sums.get((path, version)),
                 "go_mod_sum": sums.get((path, (version or "") + "/go.mod"))}
    provenance = source / "UPSTREAM.json"
    if provenance.exists():
        if provenance.is_symlink():
            raise RuntimeError(f"local replacement provenance cannot be a symlink: {provenance}")
        declared = json.loads(provenance.read_text())
        if declared.get("module") != path or declared.get("version") != version:
            raise RuntimeError(f"local replacement provenance disagrees with go.mod: {path}")
        if reference["sum"] and declared.get("module_sum") != reference["sum"]:
            raise RuntimeError(f"local replacement provenance disagrees with go.sum: {path}")
        reference["sum"] = reference["sum"] or declared.get("module_sum")
        reference["declared_provenance"] = declared
        reference["provenance_sha256"] = hashlib.sha256(provenance.read_bytes()).hexdigest()
    return reference


def collect(args, staging):
    modules = list(json_stream(run_go(args.go, "list", "-m", "-json", "all")))
    if not modules or not modules[0].get("Main"):
        raise RuntimeError("run licenses.py from inside the routerd2 Go module")
    project = modules[0]
    sums = {}
    sum_file = Path(project["GoMod"]).with_name("go.sum")
    if sum_file.exists():
        for line in sum_file.read_text().splitlines():
            fields = line.split()
            if len(fields) == 3:
                sums[(fields[0], fields[1])] = fields[2]
    # The build graph includes dependencies whose packages are not linked. Go
    # therefore might not have downloaded their source archives during build.
    downloaded = list(json_stream(run_go(args.go, "mod", "download", "-json", "all")))
    downloads = {(item.get("Path"), item.get("Version")): item for item in downloaded}
    for item in downloaded:
        if item.get("Error"):
            raise RuntimeError(f"module download failed: {item['Path']}: {item['Error']}")

    imported = {}
    for target in args.target or ["linux/amd64", "linux/arm64"]:
        parts = target.split("/")
        if len(parts) != 2 or not all(re.fullmatch(r"[a-z0-9_]+", part) for part in parts):
            raise RuntimeError(f"invalid target {target!r}; use GOOS/GOARCH")
        environment = dict(os.environ, CGO_ENABLED="0", GOOS=parts[0], GOARCH=parts[1])
        packages = list(json_stream(run_go(
            args.go, "list", "-deps", "-json", "./cmd/routerd", "./cmd/routerd-modules",
            environment=environment,
        )))
        by_module = {}
        standard = []
        for package in packages:
            if package.get("Error") or package.get("DepsErrors"):
                raise RuntimeError(f"package inventory is incomplete: {package.get('ImportPath')}")
            if package.get("Standard"):
                standard.append(package["ImportPath"])
            elif package.get("Module"):
                by_module.setdefault(package["Module"]["Path"], []).append(package["ImportPath"])
        imported[target] = {
            "cgo_enabled": False,
            "commands": ["./cmd/routerd", "./cmd/routerd-modules"],
            "modules": {name: sorted(values) for name, values in sorted(by_module.items())},
            "standard_packages": sorted(standard),
        }

    entries = []
    missing = []
    for module in sorted(modules, key=lambda item: item["Path"]):
        effective = module.get("Replace", module)
        downloaded_module = downloads.get((effective["Path"], effective.get("Version")), {})
        directory = effective.get("Dir") or downloaded_module.get("Dir")
        if not directory:
            raise RuntimeError(f"module source directory unavailable: {module['Path']}")
        source = Path(directory).resolve(strict=True)
        files = notice_files(source)
        # A nested third-party notice is not evidence of the module's own
        # license. Require an explicit license/copying file at the root.
        root_license = [path for path in files if len(path.parts) == 1 and
                        re.match(r"^(?:licen[cs]e|copying)(?:$|[-_. ])", path.name, re.I)]
        if not root_license:
            missing.append(module["Path"] + "@" + module.get("Version", "project"))
        directory_name = module_directory_name(module["Path"], module.get("Version"))
        destination = staging / "modules" / directory_name
        entry = {
            "path": module["Path"],
            "version": module.get("Version"),
            "main": bool(module.get("Main")),
            "sum": module.get("Sum") or downloaded_module.get("Sum"),
            "go_mod_sum": module.get("GoModSum") or downloaded_module.get("GoModSum"),
            "notice_directory": "modules/" + directory_name,
            "notices": copy_notices(source, destination, files),
            "imported_by_targets": sorted(target for target, inventory in imported.items()
                                           if module["Path"] in inventory["modules"]),
        }
        if module.get("Replace"):
            replacement = module["Replace"]
            entry["replacement"] = {
                "path": replacement["Path"], "version": replacement.get("Version"),
                "local": not bool(replacement.get("Version")),
            }
            entry["upstream_reference"] = upstream_reference(module, source, sums)
            if not replacement.get("Version"):
                # A module's recorded h1 checksum belongs to upstream. It must
                # never be presented as validation of a locally patched tree.
                entry["sum"] = None
                entry["go_mod_sum"] = None
                entry["replacement"]["source_tree_digest"] = source_tree_digest(source)
        entries.append(entry)
    if missing:
        raise RuntimeError("root license notice missing for: " + ", ".join(missing))

    go_environment = json.loads(run_go(args.go, "env", "-json", "GOROOT", "GOVERSION"))
    go_root = Path(go_environment["GOROOT"]).resolve(strict=True)
    go_notices = [Path(name) for name in ("LICENSE", "PATENTS") if (go_root / name).is_file()]
    if Path("LICENSE") not in go_notices:
        raise RuntimeError("Go toolchain LICENSE is missing")
    # The Go standard library itself vendors small supporting packages. Keep
    # their distinct upstream notices when present, not just Go's root license.
    vendor = go_root / "src/vendor"
    if vendor.is_dir():
        go_notices += [Path("src/vendor") / path for path in notice_files(vendor)]
    toolchain = {
        "version": go_environment["GOVERSION"], "notice_directory": "go",
        "notices": copy_notices(go_root, staging / "go", sorted(set(go_notices), key=str)),
    }
    manifest = {
        "schema_version": 1,
        "scope": "build-module-graph",
        "description": "Source notice inventory; module presence does not imply runtime linkage or license compatibility.",
        "project": project["Path"],
        "go_mod_sha256": hashlib.sha256(Path(project["GoMod"]).read_bytes()).hexdigest(),
        "toolchain": toolchain,
        "modules": entries,
        "runtime_package_inventory": imported,
    }
    (staging / "manifest.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    lines = [
        "routerd2 dependency source notices", "=================================", "",
        "Scope: complete selected Go build module graph, plus Go toolchain notices.",
        "This collection preserves upstream notice text; it does not classify licenses",
        "or assert that every listed module is linked into a runtime binary.",
        "manifest.json records module versions, checksums, notice hashes, and the",
        "packages imported by static command builds for: " + ", ".join(imported) + ".",
        "Local replacements record their source tree digest separately from the",
        "declared upstream version/checksum; an upstream checksum does not verify a local patch.",
        "Linux kernel and Alpine package notices are separate from this Go inventory.", "",
        f"Go toolchain: {toolchain['version']} (go/)", "",
    ]
    for entry in entries:
        lines.append(f"{entry['path']} {entry['version'] or '(project source)'}")
        lines.append(f"  Notices: {entry['notice_directory']}/")
        if entry.get("replacement"):
            replacement = entry["replacement"]
            lines.append(f"  Replacement: {replacement['path']} {replacement['version'] or '(local source)'}")
            reference = entry["upstream_reference"]
            lines.append(f"  Upstream reference: {reference['path']} {reference['version']} {reference['sum'] or '(checksum unavailable)'}")
            if replacement.get("source_tree_digest"):
                lines.append(f"  Local source SHA256: {replacement['source_tree_digest']['sha256']}")
        lines.append(f"  Imported targets: {', '.join(entry['imported_by_targets']) or '(not imported by these command builds)'}")
    (staging / "NOTICE.txt").write_text("\n".join(lines) + "\n")
    return len(entries)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default=os.environ.get("GO", "go"))
    parser.add_argument("--output", type=Path, default=Path("dist/licenses"))
    parser.add_argument("--target", action="append", help="GOOS/GOARCH; repeatable (default: linux/amd64 and linux/arm64)")
    args = parser.parse_args()
    output = args.output.absolute()
    current = Path.cwd().resolve()
    if output.resolve() == current or output.resolve() in current.parents:
        parser.error("output cannot replace the project directory or one of its parents")
    if output.is_symlink():
        parser.error("output cannot be a symbolic link")
    output.parent.mkdir(parents=True, exist_ok=True)
    staging = Path(tempfile.mkdtemp(prefix=".routerd2-licenses-", dir=output.parent))
    try:
        count = collect(args, staging)
        # Never publish a partial inventory or destroy the previous good bundle
        # when collection fails. A successful collection replaces it as a unit.
        previous = None
        if output.exists():
            previous = Path(tempfile.mkdtemp(prefix=".routerd2-licenses-old-", dir=output.parent))
            previous.rmdir()
            output.rename(previous)
        try:
            staging.rename(output)
        except OSError:
            if previous is not None:
                previous.rename(output)
            raise
        if previous is not None:
            shutil.rmtree(previous)
        print(f"Collected source notices for {count} modules and the Go toolchain in {output}")
    except (OSError, RuntimeError, ValueError) as error:
        raise SystemExit(f"license collection failed: {error}") from error
    finally:
        if staging.exists():
            shutil.rmtree(staging)


if __name__ == "__main__":
    main()
