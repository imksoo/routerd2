"""Source download and regular-file tar primitives for bundled engines."""
import io
from pathlib import Path
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
