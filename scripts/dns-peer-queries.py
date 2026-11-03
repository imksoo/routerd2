#!/usr/bin/env python3
"""Query a live native DNS profile using operator-supplied peer results.

Run inside the uplink owner's mount namespace. Case data stays private and
contains source, name, rcode, and whether an SOA answer was observed at the peer.
"""
import argparse
import ipaddress
import json
from pathlib import Path
import re
import subprocess
import time
from urllib.parse import urlencode, urlsplit


def run(command):
    result = subprocess.run(command, capture_output=True, text=True, timeout=12)
    if result.returncode:
        raise RuntimeError(command[0] + ": " + result.stderr[-1000:])
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--stage", type=Path, required=True)
    parser.add_argument("--client", required=True)
    parser.add_argument("--cases", type=Path, required=True)
    args = parser.parse_args()

    def api(path):
        return json.loads(run(["curl", "--fail-with-body", "--max-time", "5", "-sS", "--unix-socket", str(args.stage / "run/control.sock"), "http://localhost" + path]))

    listeners = api("/v1/services/service.dns")["snapshot"]["listeners"]
    addresses = [urlsplit("//" + value).hostname for value in listeners]
    assert {ipaddress.ip_address(value).version for value in addresses} == {4, 6}
    cases = json.loads(args.cases.read_text())
    exchanges = 0
    for index, case in enumerate(cases):
        for address in addresses:
            for transport in ("+notcp", "+tcp"):
                reply = run(["ip", "netns", "exec", args.client, "dig", "@" + address, case["name"], "SOA", "+time=5", "+tries=1", "+noall", "+comments", "+answer", transport])
                status = re.search(r"status: (\w+)", reply)
                assert status and status[1] == case["rcode"], (index, transport, status[1] if status else "no DNS response")
                if case["soa"]:
                    assert re.search(r"\sIN\s+SOA\s", reply), (index, "peer SOA answer missing")
                exchanges += 1
        path = "/v1/services/service.dns-core/dns-history?" + urlencode({"name": case["name"], "limit": 100})
        until = time.monotonic() + 5
        while True:
            records = api(path)["queries"] or []
            if any(row["upstream"] == case["source"] and row["responseCode"] == case["rcode"] for row in records):
                break
            if time.monotonic() >= until:
                raise RuntimeError("DNS history did not identify the configured source for case " + str(index))
            time.sleep(.2)
        print("PASS DNS case", index + 1, "rcode", case["rcode"], "IPv4/IPv6 UDP/TCP and configured source history", flush=True)
    print("PASS actual peer DNS results:", len(cases), "sources,", exchanges, "client exchanges", flush=True)


if __name__ == "__main__":
    main()
