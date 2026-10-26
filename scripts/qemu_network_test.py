"""The VM gate must distinguish executed tests from empty or skipped runs."""

import importlib.util
from pathlib import Path
import unittest
import tempfile
import json


SPEC = importlib.util.spec_from_file_location("routerd_qemu_network", Path(__file__).with_name("qemu-network.py"))
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)


class GuestResultTest(unittest.TestCase):
    def test_graceful_vrrp_requires_real_admission_gate(self):
        suite = GATE.SUITES["daemon-vrrp-activation"]
        self.assertEqual(suite["test"], "TestDaemonVRRPGracefulAdmission")
        self.assertTrue(suite["commands"])
        initializer = (GATE.PROJECT / "images/kernel-test-init/main.go").read_text()
        self.assertIn('case "daemon-vrrp-activation":', initializer)
        self.assertIn('"TestDaemonVRRPGracefulAdmission", "ROUTERD_VRRP_ACTIVATION_TEST"', initializer)

    def test_conditional_vrrp_requires_actual_ownership_and_shutdown_gate(self):
        suite = GATE.SUITES["daemon-vrrp-conditional"]
        self.assertEqual(suite["test"], "TestDaemonVRRPConditionalOwnership")
        self.assertTrue(suite["commands"])
        initializer = (GATE.PROJECT / "images/kernel-test-init/main.go").read_text()
        self.assertIn('case "daemon-vrrp-conditional":', initializer)
        self.assertIn('"TestDaemonVRRPConditionalOwnership", "ROUTERD_VRRP_CONDITIONAL_TEST"', initializer)

    def test_conditional_recovery_requires_its_actual_daemon_oracle(self):
        suite = GATE.SUITES["daemon-startup-recovery"]
        self.assertEqual(suite["test"], "TestDaemonConditionalStartupRecovery")
        self.assertEqual(suite["marker"], "ROUTERD_STARTUP_RECOVERY_DAEMON_TEST")
        self.assertTrue(suite["commands"])
    def test_startup_suite_requires_actual_adoption_and_recovery_test(self):
        suite = GATE.SUITES["daemon-startup"]
        self.assertEqual(suite["test"], "TestDaemonStartupSelectionAndRecovery")
        self.assertEqual(suite["marker"], "ROUTERD_STARTUP_SELECTION_DAEMON_TEST")
        self.assertTrue(suite["commands"])
        initializer = (GATE.PROJECT / "images/kernel-test-init/main.go").read_text()
        self.assertIn('case "daemon-startup":', initializer)
        self.assertIn('"TestDaemonStartupSelectionAndRecovery", "ROUTERD_STARTUP_SELECTION_DAEMON_TEST"', initializer)
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
        fixed = {"daemon-dhcp4-clock": "TestDaemonDHCPv4ClockCorrections",
                 "daemon-vrrp-clock": "TestDaemonVRRPClockCorrections",
                 "daemon-dhcp6-clock": "TestDaemonDHCP6ClockCorrections",
                 "daemon-ra-clock": "TestDaemonRAClientClockCorrections"}
        for name in fixed:
            self.assertIn(name, GATE.SUITES)
        for name in GATE.SUITES:
            with self.subTest(suite=name), tempfile.TemporaryDirectory() as d:
                root = Path(d)
                marker = GATE.clock_marker(root, name)
                nonce_file = root / "test-clock-nonce"
                if name in fixed:
                    nonce = nonce_file.read_text().strip()
                    self.assertRegex(nonce, r"^[0-9a-f]{32}$")
                    self.assertEqual(marker, "routerd2.clock-test=" + nonce)
                    self.assertEqual(GATE.SUITES[name]["test"], fixed[name])
                else:
                    self.assertEqual(marker, "")
                    self.assertFalse(nonce_file.exists())

    def test_module_selection_keeps_dependency_closure_and_builtins(self):
        dep = "kernel/foo.ko: kernel/base.ko\nkernel/base.ko:\nkernel/unneeded.ko:\n"
        self.assertEqual(GATE.module_closure(dep, "kernel/builtin.ko\n", ["foo", "builtin"]),
                         {"kernel/foo.ko", "kernel/base.ko"})
        for invalid, roots in [("kernel/a.ko: kernel/b.ko\nkernel/b.ko: kernel/a.ko\n", ["a"]),
                               (dep, ["missing"]), ("../a.ko:\n", ["a"]),
                               ("a/foo.ko:\nb/foo.ko:\n", ["foo"])]:
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                GATE.module_closure(invalid, "", roots)

    def test_pruned_image_keeps_metadata_and_only_selected_modules(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            base = root / "lib/modules/test-kernel"
            (base / "kernel").mkdir(parents=True)
            (base / "modules.dep").write_text("kernel/foo.ko: kernel/base.ko\nkernel/base.ko:\nkernel/unused.ko:\n")
            (base / "modules.builtin").write_text("")
            for name in ("foo", "base", "unused"):
                (base / ("kernel/" + name + ".ko")).write_bytes(b"module")
            result = GATE.prune_modules(root, "test-kernel", ["foo"])
            self.assertEqual(result["selected_files"], ["kernel/base.ko", "kernel/foo.ko"])
            self.assertFalse((base / "kernel/unused.ko").exists())
            self.assertTrue((base / "modules.dep").is_file())

    def test_fixed_kernel_module_roots_are_declared_once(self):
        roots = json.loads((GATE.PROJECT / "images/kernel-test-init/modules.json").read_text())
        self.assertEqual(len(roots), len(set(roots)))
        self.assertTrue({"af_packet", "wireguard", "veth", "ip6_tunnel", "nft_ct", "nft_chain_nat"}.issubset(roots))
        init = (GATE.PROJECT / "images/kernel-test-init/main.go").read_text()
        self.assertIn("go:embed modules.json", init)

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
            "truncated initramfs": exact + "Initramfs unpacking failed: write error\n",
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
