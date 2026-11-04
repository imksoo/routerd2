#!/usr/bin/env python3
"""Install a prepared routerd2 GPT image onto an explicitly selected Linux disk.

This overwrites the selected disk. Run from a separate maintenance environment.
"""

import argparse
import array
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", type=Path, required=True)
    parser.add_argument("--device", type=Path, required=True, help="Whole block device to overwrite")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("run as root from a separate maintenance environment")
    with args.image.open("rb") as source:
        image = os.fstat(source.fileno())
        if not stat.S_ISREG(image.st_mode) or image.st_size == 0:
            parser.error("image must be a completed regular disk image")
        image_path = f"/proc/self/fd/{source.fileno()}"
        subprocess.run(["sfdisk", "--verify", image_path], pass_fds=(source.fileno(),), check=True)
        layout = json.loads(subprocess.run(["sfdisk", "--json", image_path],
            pass_fds=(source.fileno(),), check=True, capture_output=True, text=True).stdout)["partitiontable"]
        if layout["label"] != "gpt" or layout["sectorsize"] != 512:
            parser.error("image must use the builder's 512-byte-sector GPT layout")
        # Linux's exclusive block open also excludes mounted partitions and
        # claims in other mount namespaces. Hold it through write and reread.
        with os.fdopen(os.open(args.device, os.O_RDWR | os.O_EXCL), "r+b") as target:
            device = os.fstat(target.fileno())
            if not stat.S_ISBLK(device.st_mode):
                parser.error("device must be a whole block device")
            sysfs = (Path("/sys/dev/block") / f"{os.major(device.st_rdev)}:{os.minor(device.st_rdev)}").resolve(strict=True)
            if (sysfs / "partition").exists():
                parser.error("select the whole disk, not a partition")
            capacity = array.array("Q", [0])
            sector = array.array("I", [0])
            fcntl.ioctl(target.fileno(), 0x80081272, capacity)  # BLKGETSIZE64
            fcntl.ioctl(target.fileno(), 0x1268, sector)  # BLKSSZGET
            if capacity[0] < image.st_size:
                parser.error(f"device size {capacity[0]} is smaller than image size {image.st_size}")
            if sector[0] != 512:
                parser.error("this disk image requires 512-byte logical sectors")
            print(f"Installing {image.st_size} bytes from {args.image} to {args.device}", flush=True)
            block_size = 4 * 1024 * 1024
            expected = hashlib.sha256()
            for block in iter(lambda: source.read(block_size), b""):
                target.write(block)
                expected.update(block)
            target.flush()
            os.fsync(target.fileno())
            target.seek(0)
            observed = hashlib.sha256()
            for offset in range(0, image.st_size, block_size):
                size = min(block_size, image.st_size - offset)
                block = target.read(size)
                if len(block) != size:
                    raise OSError("short disk read during installation verification")
                observed.update(block)
            if observed.digest() != expected.digest():
                raise OSError("installed disk differs from the source image")
            device_path = f"/proc/self/fd/{target.fileno()}"
            if capacity[0] > image.st_size:
                subprocess.run(["sfdisk", "--no-reread", "--no-tell-kernel", "--relocate", "gpt-bak-std", device_path],
                    pass_fds=(target.fileno(),), check=True)
            subprocess.run(["sfdisk", "--verify", device_path], pass_fds=(target.fileno(),), check=True)
            os.fsync(target.fileno())
            fcntl.ioctl(target.fileno(), 0x125F)  # BLKRRPART
            print(json.dumps({"device": str(args.device), "image_bytes": image.st_size,
                "image_sha256": expected.hexdigest(), "device_bytes": capacity[0]}, indent=2))


if __name__ == "__main__":
    main()
