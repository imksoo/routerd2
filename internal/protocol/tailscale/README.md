# Native Tailscale router

The product supervises a fixed Tailscale 1.104.2 daemon through its typed
LocalAPI. It does not embed the engine into boot/control, invoke a host CLI, or
expose a tsnet listener as subnet routing. The source reference is routerd
1329ef5: `pkg/tailscale`, `pkg/render/tailscale_systemd.go`,
`pkg/controller/chain/system.go`, and `docs/how-to/tailscale.md`.

`make tailscale-engine` builds Linux with the project's Go toolchain and
`CGO_ENABLED=0`. It produces `dist/tailscale-engine.tar` and
`dist/tailscale-sources.tar`. The latter includes the pinned upstream source,
exact imported module zips, build recipe and inventory; the engine includes
full module and Go license notices. `TAILSCALE_ARCH=arm64 TAILSCALE_DIR=...`
selects a separate cross-build output. Bind both artifacts to the `tailscale`
role in the signed runtime catalog. A host factory boot can explicitly bind
`workers.tailscale` and `engines.tailscale`; the default engine-free boot does
not enable this role.

One node is supported per router network namespace. For example, the service
payload in a schema-2 resource is:

```json
{
  "id": "tailnet",
  "service": {
    "name": "tailnet",
    "tailscale": {
      "interface": "tailscale0",
      "server_url": "https://controlplane.tailscale.com",
      "bootstrap_dns": ["192.0.2.53"],
      "control_ca_file": "/etc/ssl/certs/ca-certificates.crt",
      "hostname": "site-router",
      "auth_key_file": "/var/lib/routerd/secrets/tailnet.key",
      "advertise_routes": ["10.42.0.0/24"],
      "advertise_exit_node": true,
      "accept_routes": false,
      "shields_up": false
    }
  }
}
```

The addresses above are documentation setup values. Supply reachable bootstrap
DNS, the intended control server and a PEM trust bundle. The private key file
contains the raw authentication key, in an owned 0400/0600 regular file. Omit
`auth_key_file` after enrollment when using the saved identity. Environment-file
parsing and inline configuration secrets from the old systemd unit are not a
native interface. Headscale route approval or tailnet administrator approval
is still needed for advertised subnet and exit routes.

The upstream daemon owns the TUN, its addresses, routing table52 and rule
priorities5210..5270. Conflicting native intent and foreign existing scope are
rejected. A namespace socket excludes concurrent owners even with different
state directories; an atomic boot/network-namespace marker permits owned
cleanup after SIGKILL. Identity lives
below the persistent `services/tailscale` directory, independently of resource
name and current preferences. It must survive worker and supervisor restarts.
Startup is disabled before full current preferences are applied with
`LocalBackend.Start(UpdatePrefs)`, which retains upstream identity. No vendor
state-file parser or prototype migration is used.

The network worker owns forwarding and its `inet routerd2` firewall. Configure
IPv4/IPv6 forwarding and directed TUN-to-LAN/WAN allow rules explicitly; accept
UDP41641 and the required ICMPv6/input traffic when enabling an input policy.
The daemon's netfilter is off. Exit service requires explicit IPv4/IPv6 NAT on
the intended WAN (or real return routing). A WAN policy before priority5270 can
bypass Tailscale's lookup; choose rules for the intended forwarding path.
WireGuard cannot explicitly bind the same UDP41641 port.

The engine has a private filesystem, resolver and `/run`. Its normal DNS
setup does not access the host's resolved/DBus or rewrite its resolver. `acceptDNS` and Tailscale SSH
are not exposed yet. Neither is equivalent to native DNS or routerd management
SSH. `BackendState=Running`, the daemon's self `Online` flag and `.active` describe
the observed engine state. They do not prove a live control-server connection,
a usable subnet or an exit path; Health can check the required communication.
Status includes original finite boot observation times and actual peer traffic counters.

Run `make check-tailscale` with Linux root/sudo, TUN, iproute2, nft, curl,
OpenSSL, tcpdump, dnsmasq and Python 3. `make tailscale-peers` downloads checksum-pinned
Headscale 0.29.6 and official Tailscale 1.104.2 Linux/amd64 peers into
`dist/peer-env/tailscale`; `make check-tailscale` includes this prerequisite.
The fixture isolates network/mount/PID namespaces,
uses the product supervisor/control/network/worker, and destroys raw peer keys,
logs and captures afterward. The handoff and implementation plan record which
communication gates have actually passed and which remain unfinished.

Legacy `TailscaleNode` import requires explicit enrollment and kernel/bootstrap
ownership policy; see [the importer](../../legacyconfig/README.md). The actual
CLI-to-PID1 fixture also checks stopped external-daemon state provisioning,
identity reuse after reauthentication, and accepted IPv4/IPv6 subnet routes
with LAN TCP/UDP forwarding without NAT. These checks use version 1.104.2 on
both sides; they do not qualify older or hardware-sealed identity migration.
