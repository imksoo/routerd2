"""The VM gate must distinguish executed tests from empty or skipped runs."""

import importlib.util
from pathlib import Path
import unittest
import tempfile
import re


SPEC = importlib.util.spec_from_file_location("routerd_qemu_network", Path(__file__).with_name("qemu-network.py"))
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)


class GuestResultTest(unittest.TestCase):
    def test_resource_program_requires_its_real_daemon_test(self):
        suite = GATE.SUITES["daemon-resource-program"]
        self.assertEqual(suite["test"], "TestDaemonResourceProgramLifecycleAndRecovery")
        self.assertEqual(suite["marker"], "ROUTERD_RESOURCE_PROGRAM_DAEMON_TEST")
        self.assertEqual(suite["package"], "./internal/integration")
        self.assertTrue(suite["commands"])
        marker = suite["marker"] + "_PASS\n"
        for output in (
            marker,
            "PASS\n" + marker,
            "--- SKIP: " + suite["test"] + " (0.00s)\n" + marker,
            "--- PASS: TestDaemonConfigurationAndRecovery (1.00s)\n" + marker,
        ):
            with self.subTest(output=output), self.assertRaises(RuntimeError):
                GATE.require_guest_pass(output, suite, 0)

    def test_only_fixed_clock_suite_gets_matching_boot_marker(self):
        for name in GATE.SUITES:
            with self.subTest(suite=name), tempfile.TemporaryDirectory() as d:
                root = Path(d)
                marker = GATE.clock_marker(root, name)
                nonce_file = root / "test-clock-nonce"
                if name == "daemon-dhcp4-clock":
                    nonce = nonce_file.read_text().strip()
                    self.assertRegex(nonce, r"^[0-9a-f]{32}$")
                    self.assertEqual(marker, "routerd2.clock-test=" + nonce)
                    self.assertEqual(GATE.SUITES[name]["test"], "TestDaemonDHCPv4ClockCorrections")
                else:
                    self.assertEqual(marker, "")
                    self.assertFalse(nonce_file.exists())

    def test_each_exact_suite_pass(self):
        for name, suite in GATE.SUITES.items():
            with self.subTest(suite=name):
                output = f"=== RUN   {suite['test']}\r\n--- PASS: {suite['test']} (1.25s)\r\nPASS\r\n{suite['marker']}_PASS\r\n"
                GATE.require_guest_pass(output, suite, 0)

    def test_missing_renamed_skipped_and_failed_tests_do_not_pass(self):
        suite = GATE.SUITES["daemon-management"]
        exact = f"--- PASS: {suite['test']} (1.25s)\n{suite['marker']}_PASS\n"
        invalid = {
            "empty run": "testing: warning: no tests to run\nPASS\n" + suite["marker"] + "_PASS\n",
            "wrong test": exact.replace(suite["test"], "TestRenamed"),
            "marker prefix": exact.replace(suite["marker"] + "_PASS", suite["marker"] + "_PASS_EXTRA"),
            "missing marker": exact.splitlines()[0],
            "skipped subtest": exact + "    --- SKIP: child (0.00s)\n",
            "failed subtest": exact + "    --- FAIL: child (0.00s)\n",
            "kernel panic": exact + "Kernel panic - not syncing\n",
            "initializer failure": exact + "ROUTERD_KERNEL_TEST_FAIL\n",
            "suite failure": exact + suite["marker"] + "_FAIL: shutdown\n",
        }
        for name, output in invalid.items():
            with self.subTest(reason=name), self.assertRaises(RuntimeError):
                GATE.require_guest_pass(output, suite, 0)
        with self.assertRaises(RuntimeError):
            GATE.require_guest_pass(exact, suite, 1)


if __name__ == "__main__":
    unittest.main()
