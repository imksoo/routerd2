# Native DHCPv6 client

One worker owns UDP 546 on one interface. `request_address` requests IA_NA,
`request_prefix` requests IA_PD, and both can be enabled in the same exchange.
With neither enabled, the client performs Information-request only. DUID and
IAID belong to this client; additional workers cannot own separate IA types on
the same interface. The network writer installs learned IA_NA addresses as /128
and derives configured LAN addresses and advertisements from IA_PD prefixes.
The client does not infer a default router from DHCPv6.

The native parser and exchange implement [RFC 8415](https://www.rfc-editor.org/rfc/rfc8415.html):
Solicit/Advertise/Request/Reply, server-bound Renew, multicast Rebind, individual
assignment validity, zero-lifetime withdrawal, retained deprecated old prefixes,
and a separate information transaction. Server-selected timers and lifetimes
are not echoed as client-selected values. Invalid identities, transaction IDs,
IAIDs, duplicate/truncated options and unusable assignments are discarded.
Multiple addresses and prefixes are retained, up to 64 of each per client.
A completed or replaced transaction cannot refresh an assignment.

Only the DUID identity is persisted under `state/identities`; restart requests
fresh server confirmation through a new acquisition. No saved lease is
published as current, and no discarded PD-wrapper journal is imported. Kernel
DAD failure removes the proposed address and starts Decline; failure-path
interoperability still needs acceptance. Worker readiness currently means the
socket is open, not that an address has completed DAD.

`make check-dhcp6` uses the real supervisor/control/network/client processes,
independent dnsmasq, source-bound TCP/UDP, worker replacement, actual lease
renewal and server silence through Rebind/expiry/recovery. `make check-pd-ra`
uses an independent wire server and captures generated LAN advertisements;
IA_NA and IA_PD must share each stateful transaction and IAID.

These are isolated amd64 Linux VM checks. DHCPv6 assignment and information
snapshots still carry UTC deadlines; wall-clock correction, suspend and reboot
qualification remain unfinished. Automatic RA-triggered DHCPv6, required-address
readiness, complete legacy resource selection, VRF transport, graceful Release,
and physical NIC qualification remain separate work. The former pinned PD
state-machine wrapper, timer corrections and prototype journal reader were
removed; there is no compatibility path.
