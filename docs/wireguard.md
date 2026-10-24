# Kernel WireGuard

WireGuard encryption and packet transport run in the Linux kernel. The network
worker creates a journal-owned `wireguard` link and configures it through
generic netlink. No `wg`, `wg-quick`, `ip`, shell or additional daemon is required.
An appliance's boot settings must load the `wireguard` module; a container uses
its host's kernel and module provisioning.

The [site-to-site example](../examples/wireguard-site.yaml) declares the link,
addresses, complete peer set, routes and firewall policy. `allowed_ips` controls
WireGuard's peer selection and authenticated source-address checking. It does
not add routes or authorize forwarding. Those remain separate explicit parts
of the same transactional configuration.

## Keys and configuration changes

`private_key_file` and optional peer `preshared_key_file` name regular files
containing a base64-encoded 32-byte key. The file must belong to the network
worker's UID, have no group/other permissions, and be reachable through trusted
directories without following symlinks. Native generated keys use mode 0600.
The parent directory must already exist. The example places the key directly
in the state directory that routerd creates and verifies during boot.

With `generate_private_key: true`, the first apply generates a key only when the
file does not exist. A pre-existing file is validated and reused. No private
key or preshared key enters the configuration document, API snapshot, ownership
journal or command-line arguments. `show network` exposes the device's public
key, peer endpoints, latest handshakes and byte counters.

On first import, a file path is bound to an immutable private copy beside the
network ownership journal. Rotate a key by preparing a **new, versioned file
path** and committing that path. Modifying the contents of an already bound
path does not rotate a key. This lets rollback restore the previous key even
when its original input file has been removed. Store both the input files and
routerd's state on persistent storage; do not delete immutable key snapshots
while a retained configuration may refer to them.

For Kubernetes, projected Secret symlinks are not valid key files for this
reader. Provision versioned regular files with mode 0600 into the writable
state volume before the configuration first references them. The native reader
does not follow an atomically retargeted Secret path or silently replace a
previous key binding.

## Applying a peer set

WireGuard's kernel API can require several messages for a large peer set. Those
messages are not one transaction. Routerd records intent before mutation and
keeps its owned link down while replacing and verifying the peer set. Only a
complete readback permits the desired UP state. Recovery uses the ownership
journal and immutable keys; an unrelated pre-existing WireGuard interface or
foreign peer set is not adopted or cleared.

Removing a peer replaces the full desired set. Routes and addresses associated
with the link are handled by the same network transaction and reconciliation
path. Endpoint roaming learned from authenticated traffic is retained while
the declared endpoint is unchanged. A changed endpoint declaration takes
precedence. An empty endpoint permits a passive peer; an explicit endpoint must
be a numeric unicast IP and port. IPv6 link-local endpoints require a zone.
Hostname resolution and SAM enrollment are separate, currently unimplemented
parts of the original resource contract.

`listen_port: 0` selects an available port when a new device becomes active and
retains the observed selection while that device exists. Declare a fixed port
when a firewall or remote peer depends on it. The implementation currently
bounds each configuration at 64 interfaces, 1024 peers per interface and 16384
allowed prefixes per interface. A prefix cannot belong to two peers on the
same interface; distinct nested prefixes retain the kernel's longest-match
behavior.

Use `commit confirmed` when changing a remote router's keys, peers, routes or
management firewall. Confirmation rolls back the entire candidate through the
same owned network writer. The kernel dataplane test is available as
`make qemu-wireguard`; it uses an independent peer in a NIC-less QEMU guest.
