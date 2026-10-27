#!/usr/bin/env python3
"""Exercise the scratch runtime, its supervisor, and its control API."""

import argparse
import json
import shlex
import subprocess
import time
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--engine", default="docker")
    parser.add_argument("--image", default="routerd2:dev")
    parser.add_argument("--timeout", type=float, default=30)
    args = parser.parse_args()
    name = "routerd2-smoke-" + uuid.uuid4().hex[:12]

    def run(*command, check=True):
        return subprocess.run([*shlex.split(args.engine), *command], check=check, text=True, capture_output=True, timeout=30)

    try:
        run("run", "--detach", "--name", name, "--network", "none",
            "--cap-drop", "ALL", "--cap-add", "NET_ADMIN", "--cap-add", "NET_RAW",
            "--read-only", "--tmpfs", "/run:mode=0755", "--tmpfs", "/var/lib/routerd:mode=0700", args.image)
        deadline = time.monotonic() + args.timeout
        while time.monotonic() < deadline:
            state = run("inspect", "--format", "{{.State.Running}}", name).stdout.strip()
            if state != "true":
                raise RuntimeError("container exited before readiness")
            logs = run("logs", name)
            if "ROUTERD_READY" in logs.stdout + logs.stderr:
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("container did not report readiness before timeout")
        status = json.loads(run("exec", name, "/usr/sbin/routerd", "status", "--runtime-dir", "/run/routerd").stdout)
        if not status.get("ready") or status.get("pid") != 1 or status.get("platform") != "container":
            raise RuntimeError(f"unexpected container supervisor state: {status}")
        for module in status["modules"]:
            if module["required"] and (not module["ready"] or module["release_id"] != status["release_id"]):
                raise RuntimeError(f"required worker is not ready for this release: {module}")
        network = json.loads(run("exec", name, "/usr/sbin/routerd", "network", "show", "--runtime-dir", "/run/routerd").stdout)
        if network.get("status") != "applied" or network.get("observation_errors"):
            raise RuntimeError(f"network did not reconcile cleanly: {network}")
        run("stop", "--time", "5", name)
        if run("inspect", "--format", "{{.State.ExitCode}}", name).stdout.strip() != "0":
            raise RuntimeError("container did not stop cleanly")
        print("Scratch container supervisor/control/shutdown smoke: OK")
    except (RuntimeError, subprocess.CalledProcessError, subprocess.TimeoutExpired, json.JSONDecodeError) as error:
        logs = run("logs", name, check=False)
        print(logs.stdout + logs.stderr)
        raise SystemExit(str(error)) from error
    finally:
        run("rm", "--force", name, check=False)


if __name__ == "__main__":
    main()
