"""An orderly shutdown or partial event comparison cannot certify a power cut."""

import copy
import importlib.util
import json
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location("qemu_health_power", Path(__file__).with_name("qemu-health-power.py"))
POWER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(POWER)


def phases():
    origins = [
        {"id": "health/wan/" + "a" * 32, "sha256": "1" * 64, "cursor": 0},
        {"id": "health/wan/" + "b" * 32, "sha256": "2" * 64, "cursor": 0},
    ]
    first = {
        "phase": "produce", "boot_id": "50695aa4-d7d3-48c1-abab-14e150fae427",
        "origin_boot_id": "50695aa4-d7d3-48c1-abab-14e150fae427",
        "pid": 1, "exec_verified": True, "filesystem": "ext4", "revision": 1,
        "pending_count": 2, "registered": 1, "retiring": 0, "health_worker": True,
        "health_error": True, "original_supervisor": "c" * 32, "original_epoch": "d" * 32,
        "original_generation": 1, "original_digest": "e" * 64, "observations": origins,
    }
    second = dict(copy.deepcopy(first), phase="registry", boot_id="7bbe264e-57c0-4ee6-aa19-67c3a1d244fb",
                  revision=2, pending_count=0, retiring=1, health_worker=False)
    for cursor, event in enumerate(second["observations"], 11):
        event["cursor"] = cursor
    third = dict(copy.deepcopy(second), phase="recovered", boot_id="1f7b18d0-0fbb-4c40-b281-ddac308491ce",
                 registered=0, retiring=0, health_error=False)
    return [
        {"phase": "produce", "termination": "SIGKILL", "qemu_exit_status": -9, "witness": first},
        {"phase": "registry", "termination": "SIGKILL", "qemu_exit_status": -9, "witness": second},
        {"phase": "recovered", "termination": "poweroff", "qemu_exit_status": 0, "witness": third},
    ]


class PowerWitnessTests(unittest.TestCase):
    def test_complete_three_boot_chain(self):
        POWER.check_phases(phases())
        # Probes accepted by the second actual worker must also be verified
        # after it is withdrawn; they are not silently excluded from the ACK.
        data = phases()
        extra = {"id": "health/wan/" + "f" * 32, "sha256": "3" * 64, "cursor": 13}
        data[1]["witness"]["observations"].append(extra)
        data[2]["witness"]["observations"].append(copy.deepcopy(extra))
        POWER.check_phases(data)

    def test_orderly_exit_missing_boot_or_wrong_cut_is_rejected(self):
        for index in (0, 1):
            for field, value in (("termination", "poweroff"), ("qemu_exit_status", 0), ("phase", "recovered")):
                with self.subTest(index=index, field=field):
                    data = phases()
                    data[index][field] = value
                    with self.assertRaises(RuntimeError):
                        POWER.check_phases(data)
        for data in (phases()[:2], phases()[1:], phases()[::-1]):
            with self.assertRaises(RuntimeError):
                POWER.check_phases(data)

    def test_current_authority_boot_or_pending_work_cannot_be_forged(self):
        changes = {
            "boot_id": phases()[0]["witness"]["boot_id"], "origin_boot_id": "unrelated",
            "pid": 2, "exec_verified": False, "filesystem": "tmpfs", "revision": 1,
            "pending_count": 1, "registered": 1, "retiring": 1, "health_worker": True,
            "health_error": True, "original_supervisor": "f" * 32, "original_epoch": "a" * 32,
            "original_generation": 2, "original_digest": "b" * 64,
        }
        for field, value in changes.items():
            with self.subTest(field=field):
                data = phases()
                data[2]["witness"][field] = value
                with self.assertRaises(RuntimeError):
                    POWER.check_phases(data)

    def test_missing_changed_duplicate_or_recursored_history_is_rejected(self):
        for index in (1, 2):
            for mutate in (
                lambda e: e.pop(),
                lambda e: e.append(copy.deepcopy(e[0])),
                lambda e: e[0].update(sha256="0" * 64),
                lambda e: e[0].update(cursor=0),
                lambda e: e[0].update(cursor=True),
                lambda e: e[0].update(id="not-a-health-event"),
            ):
                with self.subTest(index=index, mutate=mutate):
                    data = phases()
                    mutate(data[index]["witness"]["observations"])
                    with self.assertRaises(RuntimeError):
                        POWER.check_phases(data)
        data = phases()
        data[2]["witness"]["observations"][0]["cursor"] += 10
        with self.assertRaises(RuntimeError):
            POWER.check_phases(data)

    def test_exact_single_witness_and_no_shutdown_before_cut(self):
        witness = phases()[0]["witness"]
        line = POWER.MARKER + "_CUT " + json.dumps(witness)
        self.assertEqual(POWER.parse_witness(line + "\n", "produce", True), witness)
        for log in ("", line + "\n" + line, line + "\nKernel panic", line + "\nreboot: Power down",
                    line + "\n" + POWER.MARKER + "_FAIL: injected", line.replace('"produce"', '"registry"')):
            with self.subTest(log=log), self.assertRaises(RuntimeError):
                POWER.parse_witness(log, "produce", True)


if __name__ == "__main__":
    unittest.main()
