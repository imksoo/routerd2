"""Negative checks for the independent two-boot witness acceptance gate."""

import copy
import importlib.util
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location("qemu_storage", Path(__file__).with_name("qemu-storage.py"))
STORAGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(STORAGE)


def witnesses():
    first_id = "50695aa4-d7d3-48c1-abab-14e150fae427"
    first = {
        "boot_id": first_id, "pid": 1, "revision": 2, "pinned_exec_verified": True,
        "release_id": STORAGE.IDENTITIES["trial"], "release_operation": "begin-boot", "pending": True,
        "config_sha256": "config", "host_key_sha256": "host", "host_key_fingerprint": "fingerprint",
        "sentinel_sha256": "sentinel",
    }
    second = dict(first, boot_id="7bbe264e-57c0-4ee6-aa19-67c3a1d244fb",
                  release_id=STORAGE.IDENTITIES["baseline"], release_operation="boot-rollback",
                  pending=False, failed_boot_id=first_id)
    return [{"witness": first}, {"witness": second}]


class TwoBootWitnessTests(unittest.TestCase):
    def test_both_complete_witnesses_are_required(self):
        STORAGE.check_witnesses(witnesses())
        for phase in (0, 1):
            for value in (None, "", "not-a-boot-id", "50695AA4-D7D3-48C1-ABAB-14E150FAE427", 1):
                with self.subTest(phase=phase, boot_id=value):
                    data = witnesses()
                    data[phase]["witness"]["boot_id"] = value
                    with self.assertRaises(RuntimeError):
                        STORAGE.check_witnesses(data)
            data = witnesses()
            del data[phase]["witness"]["boot_id"]
            with self.assertRaises(RuntimeError):
                STORAGE.check_witnesses(data)

    def test_same_boot_wrong_fallback_or_changed_data_cannot_pass(self):
        changes = {
            "boot_id": witnesses()[0]["witness"]["boot_id"],
            "release_id": STORAGE.IDENTITIES["trial"], "release_operation": "confirm",
            "failed_boot_id": "unrelated", "pending": True, "pid": 2, "revision": 1,
            "pinned_exec_verified": False, "config_sha256": "changed",
            "host_key_sha256": "changed", "host_key_fingerprint": "changed", "sentinel_sha256": "changed",
        }
        for field, value in changes.items():
            with self.subTest(field=field):
                data = copy.deepcopy(witnesses())
                data[1]["witness"][field] = value
                with self.assertRaises(RuntimeError):
                    STORAGE.check_witnesses(data)


if __name__ == "__main__":
    unittest.main()
