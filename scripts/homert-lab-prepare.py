#!/usr/bin/env python3
"""Compile one real routerd configuration for an isolated appliance lab.

The source configuration and migration policy remain inputs.  This program only
changes physical NIC ownership, creates a fresh management DHCP identity, and
points HA and Tailscale at files and peers created for this lab run.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

import yaml


def fail(message):
    raise SystemExit(message)


def resource_map(resources):
    result = {}
    for resource in resources:
        try:
            ref = resource["kind"] + "/" + resource["metadata"]["name"]
        except (KeyError, TypeError):
            fail("source contains a resource without kind/name")
        if ref in result:
            fail("source contains duplicate resource names")
        result[ref] = resource
    return result


def write_private(path, data):
    path.write_bytes(data)
    path.chmod(0o600)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--routerd", type=Path, required=True)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--policy", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--node", choices=("02", "03"), required=True)
    parser.add_argument("--control-url", required=True)
    parser.add_argument("--bootstrap-dns", required=True)
    parser.add_argument("--management-route-metric", type=int, default=100)
    parser.add_argument("--management-client-id", help="fresh DHCP option 61 bytes, hexadecimal")
    args = parser.parse_args()

    if not args.routerd.is_file():
        fail("routerd executable is missing")
    if args.output.exists():
        fail("output must not already exist")
    if args.management_route_metric < 0:
        fail("management route metric must be non-negative")
    if not args.control_url.startswith("https://"):
        fail("control URL must be HTTPS")
    try:
        bytes.fromhex(args.management_client_id) if args.management_client_id else b""
    except ValueError:
        fail("management client ID must be hexadecimal")

    source = yaml.safe_load(args.source.read_text())
    policy = yaml.safe_load(args.policy.read_text())
    if not isinstance(source, dict) or not isinstance(source.get("spec", {}).get("resources"), list):
        fail("source must contain Router resources")
    if not isinstance(policy, dict):
        fail("policy must be a mapping")
    resources = source["spec"]["resources"]
    by_ref = resource_map(resources)
    for alias, ifname in (("wan", "eth0"), ("lan", "eth1"), ("mgmt", "eth2")):
        item = by_ref.get("Interface/" + alias)
        if item is None:
            fail("source lacks required " + alias + " interface")
        spec = item.setdefault("spec", {})
        spec.update(ifname=ifname, managed=True, adminUp=True)

    management_ref = "DHCPv4Client/appliance-management"
    if management_ref in by_ref:
        fail("source already owns appliance management DHCP")
    resources.append({
        "apiVersion": "net.routerd.net/v1alpha1",
        "kind": "DHCPv4Client",
        "metadata": {"name": "appliance-management"},
        "spec": {
            "interface": "Interface/mgmt",
            "useRoutes": True,
            "useDNS": False,
            "routeMetric": args.management_route_metric,
        },
    })

    tailscale = policy.get("tailscale", {})
    if not isinstance(tailscale, dict) or not tailscale:
        fail("policy lacks a Tailscale replacement")
    for ref, replacement in tailscale.items():
        resource = by_ref.get(ref)
        if resource is None or resource.get("kind") != "TailscaleNode":
            fail("policy Tailscale reference is absent from source")
        resource["spec"]["loginServer"] = args.control_url
        replacement.update(
            bootstrap_dns=[args.bootstrap_dns],
            control_ca_file="/etc/routerd/headscale.pem",
            enrollment="key_file",
            auth_key_file="/etc/routerd/tailnet.auth",
        )

    lease_sync = policy.get("lease_sync", {})
    if not isinstance(lease_sync, dict) or not lease_sync:
        fail("policy lacks lease synchronization replacement")
    other = "03" if args.node == "02" else "02"
    for replacement in lease_sync.values():
        replication = replacement.get("replication")
        if replication is None:
            continue
        replication.update(
            certificate_file="/etc/routerd/ha/" + args.node + ".crt",
            private_key_file="/etc/routerd/ha/" + args.node + ".key",
            peer_certificate_file="/etc/routerd/ha/" + other + ".crt",
        )
        replacement.pop("bootstrap", None)

    vrrp = policy.get("vrrp", {})
    if not isinstance(vrrp, dict) or not vrrp:
        fail("policy lacks VRRP replacement")
    for replacement in vrrp.values():
        replacement["vip_interface"] = "lan-vip"

    ra_clients = policy.get("ra_clients", {})
    if not isinstance(ra_clients, dict) or not ra_clients:
        fail("policy lacks RA client replacement")
    for replacement in ra_clients.values():
        replacement["interface"] = "eth0"

    client_id = bytes.fromhex(args.management_client_id) if args.management_client_id else b"\x01" + os.urandom(16)
    if not client_id or len(client_id) > 255:
        fail("management client ID must contain 1 through 255 bytes")

    args.output.mkdir(mode=0o700)
    source_path = args.output / "source.yaml"
    policy_path = args.output / "policy.yaml"
    native_path = args.output / "program.json"
    diagnostics_path = args.output / "migration.log"
    write_private(source_path, yaml.safe_dump(source, sort_keys=False).encode())
    write_private(policy_path, yaml.safe_dump(policy, sort_keys=False).encode())
    result = subprocess.run(
        [str(args.routerd), "config", "migrate", "--policy", str(policy_path), str(source_path)],
        capture_output=True,
    )
    write_private(diagnostics_path, result.stderr)
    if result.returncode:
        fail("migration failed; see private migration.log")
    native = yaml.safe_load(result.stdout)
    management = [
        resource["service"]["dhcp4"]
        for resource in native.get("resources", [])
        if resource.get("service", {}).get("dhcp4", {}).get("interface") == "eth2"
    ]
    if len(management) != 1:
        fail("migration did not produce exactly one management DHCP client")
    management[0]["client_id"] = base64.b64encode(client_id).decode()
    write_private(native_path, (json.dumps(native, indent=2) + "\n").encode())
    checked = subprocess.run([str(args.routerd), "config", "validate", str(native_path)], capture_output=True)
    if checked.returncode:
        (args.output / "validation.log").write_bytes(checked.stderr)
        (args.output / "validation.log").chmod(0o600)
        fail("native validation failed; see private validation.log")
    manifest = {
        "source_sha256": hashlib.sha256(args.source.read_bytes()).hexdigest(),
        "source_resources": len(resources) - 1,
        "native_resources": len(native.get("resources", [])),
        "node": args.node,
        "interfaces": {"wan": "eth0", "lan": "eth1", "management": "eth2"},
        "management_dhcp_client_id_sha256": hashlib.sha256(client_id).hexdigest(),
        "management_dhcp_client_id_bytes": len(client_id),
        "control": "fresh isolated Headscale enrollment",
        "ha": "fresh isolated TLS identities",
    }
    write_private(args.output / "manifest.json", (json.dumps(manifest, indent=2) + "\n").encode())
    print(json.dumps(manifest, sort_keys=True))


if __name__ == "__main__":
    main()
