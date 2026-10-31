#!/usr/bin/env python3
"""Disk image boundaries that must fail before formatting anything."""

import contextlib
import importlib.util
import io
import json
import stat
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("build_disk_image", Path(__file__).with_name("build-disk-image.py"))
builder = importlib.util.module_from_spec(spec)
spec.loader.exec_module(builder)


class DiskImageTest(unittest.TestCase):
    def test_prepared_identity_tree_preserves_bytes_and_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, target = root / "selected", root / "state"
            source.mkdir(mode=0o700)
            (source / "services/tailscale").mkdir(parents=True, mode=0o700)
            (source / "services").chmod(0o700)
            selected = {
                "ssh_host_ed25519_key": b"disposable SSH identity fixture\n",
                "services/tailscale/tailscaled.state": b"disposable vendor identity fixture\n",
            }
            for name, content in selected.items():
                path = source / name
                path.write_bytes(content)
                path.chmod(0o600)
            (root / "old-lease.json").write_text("must not be copied")
            target.mkdir()
            (target / "lost+found").mkdir()
            builder.provision_state(source, target)
            self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o700)
            for name, content in selected.items():
                self.assertEqual((target / name).read_bytes(), content)
                self.assertEqual(stat.S_IMODE((target / name).stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE((target / "services/tailscale").stat().st_mode), 0o700)
            self.assertEqual({str(p.relative_to(target)) for p in target.rglob("*") if p.is_file()}, set(selected))
            self.assertTrue((target / "lost+found").is_dir())

    def test_identity_symlink_does_not_copy_an_unselected_secret(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, target = root / "selected", root / "state"
            source.mkdir()
            (root / "unselected").write_text("not selected")
            (source / "ssh_host_ed25519_key").symlink_to(root / "unselected")
            with self.assertRaisesRegex(ValueError, "regular files"):
                builder.provision_state(source, target)
            self.assertFalse(target.exists())

    def test_arm64_image_is_rejected_before_output_creation(self):
        with tempfile.TemporaryDirectory() as directory:
            image = Path(directory)
            (image / "manifest.json").write_text(json.dumps({
                "input_type": "apk", "kernel_package": {"architecture": "aarch64"},
            }))
            output = image / "output"
            errors = io.StringIO()
            with patch.object(sys, "argv", ["build-disk-image", "--image-dir", str(image), "--output", str(output)]), \
                    patch.object(builder.os, "geteuid", return_value=0), contextlib.redirect_stderr(errors):
                with self.assertRaises(SystemExit):
                    builder.main()
            self.assertIn("amd64/x86_64", errors.getvalue())
            self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
