"""Regression gate for the permission boundary of the initial kernel rootfs."""

import gzip
import importlib.util
from pathlib import Path
import stat
import tempfile
import unittest


PROJECT = Path(__file__).resolve().parent.parent
SPEC = importlib.util.spec_from_file_location("routerd_mkinitramfs", PROJECT / "images/mkinitramfs.py")
PACKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PACKER)


class InitramfsRootTest(unittest.TestCase):
    def test_explicit_private_owned_root_and_unique_inodes(self):
        with tempfile.TemporaryDirectory() as directory:
            root, output = Path(directory) / "root", Path(directory) / "initramfs.cpio.gz"
            root.mkdir(mode=0o755)
            (root / "dev").mkdir()
            (root / "init").write_bytes(b"fixture initializer\n")
            PACKER.write_archive(root, output, 1234)
            names, inodes = [], set()
            with gzip.open(output, "rb") as archive:
                while True:
                    header = archive.read(110)
                    self.assertEqual(header[:6], b"070701")
                    self.assertEqual(len(header), 110)
                    fields = [int(header[offset:offset + 8], 16) for offset in range(6, 110, 8)]
                    inode, mode, uid, gid, _, epoch, size = fields[:7]
                    name_size = fields[11]
                    name = archive.read(name_size)
                    self.assertTrue(name.endswith(b"\0"))
                    name = name[:-1].decode()
                    archive.read((-(110 + name_size)) % 4)
                    self.assertNotIn(inode, inodes)
                    inodes.add(inode)
                    self.assertEqual((uid, gid, epoch), (0, 0, 1234))
                    names.append(name)
                    if len(names) == 1:
                        self.assertEqual(name, ".")
                        self.assertEqual(mode, stat.S_IFDIR | 0o755)
                        self.assertEqual(size, 0)
                    self.assertEqual(len(archive.read(size)), size)
                    archive.read((-size) % 4)
                    if name == "TRAILER!!!":
                        self.assertEqual(archive.read(), b"")
                        break
            self.assertEqual(names, [".", "dev", "dev/console", "dev/null", "init", "TRAILER!!!"])


if __name__ == "__main__":
    unittest.main()
