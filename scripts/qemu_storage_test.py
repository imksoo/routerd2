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
        "boot_id": first_id, "pid": 1, "revision": 2, "pinned_exec_verified": True, "host_identity_inspected": True,
        "release_id": STORAGE.IDENTITIES["trial"], "release_operation": "begin-boot", "pending": True,
        "config_sha256": "config", "host_key_sha256": "host", "host_key_fingerprint": "fingerprint",
        "sentinel_sha256": "sentinel",
        "event_id": "d7b3f753ee2a52bb24261bf44a508688", "event_cursor": 1,
        "store_id": "b" * 32, "snapshot_digest": "c" * 64, "snapshot_revision": 2,
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
            "pinned_exec_verified": False, "host_identity_inspected": False, "config_sha256": "changed",
            "host_key_sha256": "changed", "host_key_fingerprint": "changed", "sentinel_sha256": "changed",
            "event_id": "changed", "event_cursor": 2,
            "store_id": "d" * 32, "snapshot_digest": "e" * 64, "snapshot_revision": 3,
        }
        for field, value in changes.items():
            with self.subTest(field=field):
                data = copy.deepcopy(witnesses())
                data[1]["witness"][field] = value
                with self.assertRaises(RuntimeError):
                    STORAGE.check_witnesses(data)

    def test_missing_or_malformed_publication_cannot_pass(self):
        for phase in (0, 1):
            for field, values in {"event_id": [None, "", "not-an-event-id"], "event_cursor": [None, 0, -1, True]}.items():
                for value in values:
                    with self.subTest(phase=phase, field=field, value=value):
                        data = witnesses()
                        data[phase]["witness"][field] = value
                        with self.assertRaises(RuntimeError):
                            STORAGE.check_witnesses(data)

    def test_missing_or_invalid_snapshot_provenance_cannot_pass(self):
        for phase in (0, 1):
            for field, values in {"store_id": [None, "", "A" * 32],
                                  "snapshot_digest": [None, "", "../config", "C" * 64],
                                  "snapshot_revision": [None, 0, True, 1, 3]}.items():
                for value in values:
                    with self.subTest(phase=phase, field=field, value=value):
                        data = witnesses()
                        data[phase]["witness"][field] = value
                        with self.assertRaises(RuntimeError):
                            STORAGE.check_witnesses(data)


if __name__ == "__main__":
    unittest.main()
