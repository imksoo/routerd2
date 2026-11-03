#!/usr/bin/env python3
"""Check archive failure/recovery using the live uplink fixture's actual histories."""
import argparse
import copy
import json
from pathlib import Path
import sqlite3
import subprocess
import time


def run(command):
    result = subprocess.run(command, capture_output=True, text=True, timeout=40)
    if result.returncode:
        raise RuntimeError(command[0] + ": " + result.stderr[-1500:] + result.stdout[-1500:])
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--stage", type=Path, required=True)
    parser.add_argument("--router", required=True)
    parser.add_argument("--client", required=True)
    parser.add_argument("--archive-log", type=Path, required=True)
    args = parser.parse_args()
    stage = args.stage
    router, client = args.router, args.client
    datasets = [
        ("dnsQueries", "dns-core/dns-queries.db", "dns_queries", "id", "ts", "1"),
        ("trafficFlows", "flows/traffic-flows.db", "flows", "id", "ts_started", "ts_ended IS NOT NULL"),
        ("firewallEvents", "packets/firewall-events.db", "firewall_logs", "id", "ts", "1"),
        ("dhcpFingerprints", "lan-leases/dhcp-fingerprints.db", "dhcp_fingerprint", "mac", "observed_at", "1"),
    ]

    def api(path, body=None):
        command = ["curl", "--fail-with-body", "--max-time", "35", "-sS", "--unix-socket", str(stage / "run/control.sock"), "http://localhost" + path]
        if body is not None:
            command += ["-H", "Content-Type: application/json", "--data-binary", json.dumps(body)]
        return json.loads(run(command))

    def records(dataset):
        _, file, table, key, timestamp, condition = dataset
        with sqlite3.connect((stage / "state/services" / file).as_uri() + "?mode=ro", uri=True) as database:
            return set(database.execute(f"SELECT {key}, {timestamp} FROM {table} WHERE {condition}"))

    def wait(test):
        until = time.monotonic() + 45
        while time.monotonic() < until:
            if test():
                return
            time.sleep(.3)
        raise RuntimeError("live history archive did not reach the expected result")

    def communication():
        for transport in ([], ["+tcp"]):
            answer = run(["ip", "netns", "exec", client, "dig", "@198.18.0.1", "router." + lan_domain, "A", "+short", "+time=3", "+tries=1", *transport])
            assert answer.strip(), "LAN DNS returned no A answer"
        for family, address in (("-4", "1.1.1.1"), ("-6", "[2606:4700:4700::1111]")):
            run(["ip", "netns", "exec", client, "curl", family, "--noproxy", "*", "--fail", "--max-time", "8", "--resolve", "one.one.one.one:443:" + address, "-sS", "-I", "https://one.one.one.one/"])

    original = api("/v1/config")["config"]
    core = next(r["service"]["dns"] for r in original["resources"] if r["id"] == "service/dns-core")
    lan_domain = core["zones"][0]["spec"]["zone"].rstrip(".")
    protected = {d[0]: set(sorted(records(d), key=lambda pair: pair[1])[:10]) for d in datasets}
    assert all(protected.values()), {name: len(rows) for name, rows in protected.items()}

    def commit(document, name):
        api("/v1/config/commit", {"expected_revision": api("/v1/config")["revision"], "request_id": name, "config": document})

    candidate = copy.deepcopy(original)
    policies = [candidate["retention"]]
    for resource in candidate["resources"]:
        service = resource.get("service", {})
        for kind, field in (("dns", "query_log_retention"), ("dhcp_server", "fingerprint_retention"), ("traffic_flow", "retention"), ("firewall_log", "retention")):
            if field in service.get(kind, {}):
                policies.append(service[kind][field])
    for policy in policies:
        assert policy["syslog"], "profile has no archive destination"
        policy.update(max_age_seconds=2, interval_seconds=1)
        policy["syslog"][0]["address"] = "192.0.2.254:5514"
    run(["ip", "-n", router, "route", "add", "unreachable", "192.0.2.254/32"])
    try:
        commit(candidate, "retention-archive-unreachable")
        wait(lambda: bool(api("/v1/event-store").get("retention_error")))
        time.sleep(4)
        for dataset in datasets:
            assert protected[dataset[0]] <= records(dataset), dataset[0] + " deleted history on archive failure"
        communication()
        print("PASS all four real history datasets retained expired records during archive failure; DNS and IPv4/IPv6 HTTPS continued", flush=True)
        for policy in policies:
            policy["syslog"][0]["address"] = "192.0.2.2:5514"
        commit(candidate, "retention-archive-restored")

        def archived(dataset):
            signal, _, _, key, timestamp, _ = dataset
            result = set()
            marker = ": info " + signal + " "
            for line in args.archive_log.read_text().splitlines(keepends=True):
                if not line.endswith("\n"):
                    continue
                message = json.loads(line).get("message", "")
                if marker in message:
                    row, _ = json.JSONDecoder().raw_decode(message.split(marker, 1)[1])
                    result.add((row[key], row[timestamp]))
            return result

        for dataset in datasets:
            selected = protected[dataset[0]]
            wait(lambda: selected <= archived(dataset) and not selected.intersection(records(dataset)))
            print("PASS", dataset[0], "archived original record identities/timestamps before natural expiry", len(selected), flush=True)
        communication()
    finally:
        try:
            commit(original, "retention-original-policy")
        finally:
            run(["ip", "-n", router, "route", "delete", "unreachable", "192.0.2.254/32"])
    wait(lambda: api("/v1/config")["healthy"])
    communication()
    print("PASS original seven-day/daily archive policies restored; current client communication works", flush=True)


if __name__ == "__main__":
    main()
