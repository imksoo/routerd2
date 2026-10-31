#!/usr/bin/env python3
"""Disk image boundaries that must fail before formatting anything."""

import contextlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("build_disk_image", Path(__file__).with_name("build-disk-image.py"))
builder = importlib.util.module_from_spec(spec)
spec.loader.exec_module(builder)


class DiskImageTest(unittest.TestCase):
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
