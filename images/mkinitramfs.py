#!/usr/bin/env python3
"""Write a reproducible newc initramfs without requiring mknod or root."""

import argparse
import gzip
import os
from pathlib import Path
import stat


def pad(stream, count):
    stream.write(b"\0" * ((-count) % 4))


def entry(stream, inode, name, mode, epoch, data=None, size=0, rdev=(0, 0)):
    encoded_name = name.encode("utf-8") + b"\0"
    header = (
        inode, mode, 0, 0, 2 if stat.S_ISDIR(mode) else 1, epoch, size,
        0, 0, rdev[0], rdev[1], len(encoded_name), 0,
    )
    stream.write(b"070701" + "".join(f"{value:08x}" for value in header).encode())
    stream.write(encoded_name)
    pad(stream, 110 + len(encoded_name))
    if data is not None:
        if isinstance(data, bytes):
            stream.write(data)
        else:
            remaining = size
            while remaining:
                chunk = data.read(min(remaining, 1024 * 1024))
                if not chunk:
                    raise ValueError(f"file changed while archiving: {name}")
                stream.write(chunk)
                remaining -= len(chunk)
    pad(stream, size)


def write_archive(root, output, epoch, compresslevel=9):
    paths = {path.relative_to(root).as_posix(): path for path in root.rglob("*")}
    # Initial stdio exists before PID 1 mounts devtmpfs. These archive entries
    # create device nodes during kernel extraction, not on the build host.
    devices = {"dev/console": (5, 1), "dev/null": (1, 3)}
    for name in devices:
        if name in paths:
            raise ValueError(f"unexpected build-host device entry: {name}")
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("wb") as raw:
        with gzip.GzipFile(fileobj=raw, filename="", mode="wb", mtime=epoch, compresslevel=compresslevel) as archive:
            for inode, name in enumerate(sorted(set(paths) | set(devices)), 1):
                if name in devices:
                    entry(archive, inode, name, stat.S_IFCHR | 0o600, epoch, rdev=devices[name])
                    continue
                path = paths[name]
                metadata = path.lstat()
                mode = metadata.st_mode
                if stat.S_ISDIR(mode):
                    entry(archive, inode, name, mode, epoch)
                elif stat.S_ISLNK(mode):
                    target = os.readlink(path).encode("utf-8")
                    entry(archive, inode, name, mode, epoch, data=target, size=len(target))
                elif stat.S_ISREG(mode):
                    with path.open("rb") as source:
                        entry(archive, inode, name, mode, epoch, data=source, size=metadata.st_size)
                else:
                    raise ValueError(f"unsupported filesystem entry: {name}")
            entry(archive, len(paths) + len(devices) + 1, "TRAILER!!!", 0, epoch)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--epoch", type=int, default=0)
    parser.add_argument("--compression-level", type=int, choices=range(1, 10), default=9)
    args = parser.parse_args()
    if not 0 <= args.epoch <= 0xFFFFFFFF:
        parser.error("epoch must fit the newc 32-bit timestamp")
    if not args.root.is_dir():
        parser.error("root must be a directory")
    write_archive(args.root, args.output, args.epoch, args.compression_level)


if __name__ == "__main__":
    main()
