"""A required local dataplane gate must never accept an empty or skipped run."""

import importlib.util
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location("health_pbr", Path(__file__).with_name("health-pbr.py"))
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)


class ResultTest(unittest.TestCase):
    def test_required_test_and_marker(self):
        output = f"--- PASS: {GATE.TEST} (12.0s)\n{GATE.MARKER}\nPASS\n"
        GATE.require_pass(output, 0)
        for bad in ("", "PASS\n", GATE.MARKER, output.replace(GATE.TEST, "TestRenamed"), output.replace(GATE.MARKER, ""), output.replace(GATE.MARKER, GATE.MARKER + "_EXTRA"), output + "--- SKIP: peer\n", output + "--- FAIL: peer\n"):
            with self.subTest(output=bad), self.assertRaises(RuntimeError):
                GATE.require_pass(bad, 0)
        with self.assertRaises(RuntimeError):
            GATE.require_pass(output, 1)


if __name__ == "__main__":
    unittest.main()
