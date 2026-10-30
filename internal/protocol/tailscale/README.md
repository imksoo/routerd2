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
setup does not access the host's resolved/DBus or rewrite its resolver. A native
DNS source can explicitly consume MagicDNS through `TailscaleNode/NAME` and
`dnsServers`; see [DNS integration](../dns/README.md). This uses the existing
finite observation and service authority. OS-level `acceptDNS`, control-server
global/split resolvers and search-domain installation are not exposed yet.
Tailscale SSH is not enabled and is separate from routerd management SSH. `BackendState=Running`, the daemon's self `Online` flag and `.active` describe
the observed engine state. They do not prove a live control-server connection,
a usable subnet or an exit path; Health can check the required communication.
A failed observation publication is reported as a warning. It does not revoke
or extend the engine's current activation grant. The loop continues to check
that original deadline and stops the engine when it expires, including when
control is unavailable. Missing reports still expire in control's observation
view; a reporting failure is not reported as healthy connectivity.

Status includes original finite boot observation times, actual peer traffic
counters, each peer's DNS name and assigned tags. The node snapshot exposes
`dns_name`, `tags` and the current `tailnet` (`name`, `dns_domain`,
`magic_dns_enabled`). Disconnected membership is omitted. These are daemon
observations; `advertise_tags` stays in configuration as a request. Cached
metadata in a NeedsLogin or expired snapshot does not establish permission.

`show services NAME` also preserves the daemon's operational values rather than
inferring them from configured advertisements. On the local node,
`allowed_prefixes` contains routes permitted by the control plane,
`primary_routes` contains the subnet routes assigned to this node as primary,
and `exit_node_option` indicates an offered and approved exit node. These are
observations, separate from `advertise_routes` and `advertise_exit_node` intent.
For each peer, `active` reports recent traffic (the pinned daemon currently uses
recent sends), while `online` reports its observed control-plane presence.
`last_seen`, when supplied and nonzero, is the last online time known to the
control plane, normally supplied for an offline peer. It is not the last packet
or handshake time. `exit_node` identifies the exit selected by this router;
`exit_node_option` only identifies a peer offered and approved as an exit.
These fields do not prove current end-to-end reachability or identify which
remote peers are using this router as their exit. Observation expiry still uses
the original boot clock; a historical `last_seen` never extends that expiry.

Run `make check-tailscale` with Linux root/sudo, TUN, iproute2, nft, curl,
OpenSSL, tcpdump, dig, dnsmasq and Python 3. `make tailscale-peers` downloads checksum-pinned
Headscale 0.29.6 and official Tailscale 1.104.2 Linux/amd64 peers into
`dist/peer-env/tailscale`; `make check-tailscale` includes this prerequisite.
The fixture isolates network/mount/PID namespaces,
uses the product supervisor/control/network/worker, and destroys raw peer keys,
logs and captures afterward. The handoff and implementation plan record which
communication gates have actually passed and which remain unfinished.

`make check-tailscale-homert` uses the same isolated control server and official
peer with the homert node's three IPv4 advertisements: `10.0.0.0/8`,
`172.16.0.0/12`, and `192.168.0.0/16`. It checks the peer's actual table52 routes
and TCP/UDP in both directions with a client address in each site range.
There are no manual LAN return routes or subnet NAT. The product's engine
preferences and unchanged OS resolver establish `acceptRoutes:false` and
`acceptDNS:false`; a separately advertised peer route is not installed.
IPv4/IPv6 exit traffic uses declared WAN NAT, distinct from subnet routing.
Worker and whole PID1 restarts retain identity without an enrollment file.
Removing the Tailscale service withdraws the peer routes and LAN connectivity;
reapplying restores the three paths and both exit families. Captures check the
application traffic. The fixed private access network and Headscale here are
fixture setup, not the real homert WAN or production tailnet. This profile
does not advertise a delegated IPv6 LAN subnet or add LAN-to-tailnet IPv6 NAT,
and does not qualify simultaneous use with the real HGW or physical NIC.

On 2026-10-31 00:01:24–00:02:21 JST, release
`wsl-operations-f93be4c` (product `f93be4c`, fixture `5dcf034`) passed this
homert profile in 57.123 seconds. The real interactive CLI matched the running
daemon's local permitted/primary routes and exit eligibility, and each peer's
online/activity/last-seen/selected-exit/exit-eligibility values. This comparison
passed during traffic, after service removal/reapply, and after the official
peer went offline with a nonzero last-seen time. Evidence is in
`dist/operations-acceptance/tailscale-homert/`. The product provided the exit;
selection of another exit by the product was not exercised. This isolated
acceptance does not qualify the production tailnet, real HGW or physical NIC.

Legacy `TailscaleNode` import requires explicit enrollment and kernel/bootstrap
ownership policy; see [the importer](../../legacyconfig/README.md). The actual
CLI-to-PID1 fixture also checks stopped external-daemon state provisioning,
identity reuse after reauthentication, and accepted IPv4/IPv6 subnet routes
with LAN TCP/UDP forwarding without NAT. These checks use version 1.104.2 on
both sides; they do not qualify older or hardware-sealed identity migration.

The MagicDNS fixture enables Headscale's `magicdns-aaaa` node attribute for
IPv6 answers and tests LAN UDP/TCP A/AAAA queries across producer refreshes,
original authority expiry, node revocation and reauthentication. Another WAN
source stays usable during withdrawal, with no private-query fallthrough. This is an
explicit native DNS source, not an import of the old daemon's OS resolver
mutation. See the [upstream DNS behavior](https://tailscale.com/docs/reference/faq/dns-resolv-conf)
and [Headscale node attributes](https://headscale.net/stable/ref/policy/#node-attributes).

`shields_up` controls the daemon's inbound packet filter in addition to the
explicit native kernel firewall. It blocks new peer connections to the router
and its advertised subnets; LAN-initiated connections and their replies remain
possible. It does not replace the native INPUT/FORWARD policy.

Tags are granted by the control server. In Headscale 0.29.6, a personal
preauth key does not assign tags merely because a client advertises them.
Use a control-approved tagged enrollment key for that registration. Removing
the client request is not administrative tag revocation; use the control
server's identity/authorization operations. See [Headscale registration](https://headscale.net/stable/ref/registration/)
and [tag ownership](https://headscale.net/stable/ref/tags/). Native configuration
rollback does not roll back the control server's authorization decisions.
