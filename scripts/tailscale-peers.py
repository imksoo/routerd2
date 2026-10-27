#!/usr/bin/env python3
"""Fetch checksum-pinned independent Linux/amd64 peers for check-tailscale."""
import hashlib
from pathlib import Path
import shutil
import tarfile
import tempfile
import urllib.request

ROOT = Path(__file__).resolve().parents[1] / "dist/peer-env/tailscale"
BINARIES = {
    "tailscale": "790cf117e5ddb64421faed041490274e65c47ad6c90e9d4ce2cb3968ad06e535",
    "tailscaled": "2b391d03f7cf25c4e0d11d344d577deca94436efad1ae9af002e46269d951ba3",
    "headscale": "7477c374949eea6fb7cf3cbff14dbef49e4d118d9a33557d6764a77ece617e53",
}


def digest(file):
    with file.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def fetch(url, checksum):
    temporary = tempfile.TemporaryFile()
    try:
        with urllib.request.urlopen(url, timeout=30) as response:
            shutil.copyfileobj(response, temporary)
        temporary.seek(0)
        if hashlib.file_digest(temporary, "sha256").hexdigest() != checksum:
            raise ValueError("independent peer checksum mismatch")
        temporary.seek(0)
        return temporary
    except BaseException:
        temporary.close()
        raise


def install(source, name):
    with tempfile.NamedTemporaryFile(dir=ROOT, delete=False) as temporary:
        path = Path(temporary.name)
        try:
            shutil.copyfileobj(source, temporary)
            temporary.close()
            if digest(path) != BINARIES[name]:
                raise ValueError("independent peer binary mismatch: " + name)
            path.chmod(0o755)
            path.replace(ROOT / name)
        finally:
            path.unlink(missing_ok=True)


def main():
    ROOT.mkdir(parents=True, exist_ok=True)
    needed = {name for name, checksum in BINARIES.items() if not (ROOT / name).is_file() or digest(ROOT / name) != checksum}
    if needed & {"tailscale", "tailscaled"}:
        with fetch("https://pkgs.tailscale.com/stable/tailscale_1.104.2_amd64.tgz", "512f613ed7d04fc7282f7dc5bd76aa105aae28168fbb9357eb749ebcbc29f0ae") as archive:
            with tarfile.open(fileobj=archive, mode="r:gz") as source:
                for name in sorted(needed & {"tailscale", "tailscaled"}):
                    with source.extractfile("tailscale_1.104.2_amd64/" + name) as file:
                        install(file, name)
    if "headscale" in needed:
        url = "https://github.com/juanfont/headscale/releases/download/v0.29.6/headscale_0.29.6_linux_amd64"
        with fetch(url, BINARIES["headscale"]) as source:
            install(source, "headscale")
    for name, checksum in BINARIES.items():
        if digest(ROOT / name) != checksum:
            raise ValueError("independent peer binary mismatch: " + name)
        (ROOT / name).chmod(0o755)
    print("Verified independent Tailscale 1.104.2 and Headscale 0.29.6 Linux/amd64 peers")


if __name__ == "__main__":
    main()
