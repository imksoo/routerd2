# Imported Kubernetes ingress acceptance

## Implemented path

The deployed k8s router uses `IngressService/kubernetes-api`: HTTPS `/readyz`
with Host/SNI `k8s-api.lain.local`, status 200, a 5-second interval, a 1-second
timeout, and two consecutive observations for each backend transition.
The first healthy backend in declaration order receives TCP port 6443.
Same-LAN source translation applies only to connections originally addressed to
the ingress VIP and port; direct connections to a backend retain their source.
All-unhealthy ingress explicitly rejects new connections.

The implementation reuses Health workers, conditional NAT projection, and the
existing VRRP tracker. It adds no monitoring process or generic expression engine.
The old controller marks partially available ingress as `Degraded`, and its VRRP
tracker treats that state as unhealthy. Accordingly, one failed backend retains
forwarding through the next healthy backend but lowers priority from 150 to 90.
Full recovery restores priority 150. The configured tracking confirmations remain
three unhealthy / two healthy observations.

Each tracking confirmation now consumes a distinct round of backend observations
at the configured probe interval. Repeated controller polls do not add confirmations.
The old controller counted reconciliations, so this timing is intentionally different.
A conditional ingress used as a VIP track is explicitly rejected: its old neutral
state is not reproduced by missing Health observations. The deployed ingress has
no such condition. The importer also explicitly rejects unimplemented weighted
selection, response-body matching, and dynamic address references.

Legacy source: routerd `a0b1b3ba`,
`pkg/controller/ingressservice/controller.go` (readiness, thresholds, ordered
selection and degraded state), `pkg/render/nftables.go` (original VIP/port hairpin
match), and `pkg/controller/vrrp/controller.go` (degraded priority penalty).
The current k8s configuration was captured on 2026-10-29.

## Executed acceptance

Base: `f2f3bfc`; release: `k8s-ingress-f2f3bfc`. Contract numbers were not changed
in this branch; integration owns the native/network contract update.

```sh
umask 022
make build RELEASE_ID=k8s-ingress-f2f3bfc
python3 scripts/daemon-tests.py ingress --output dist/ingress-acceptance-converged
```

`TestDaemonImportedIngressHTTPS` runs the actual product processes and kernel
networking with independent Python HTTPS servers and a client in isolated network
namespaces. It checks HTTPS Host/SNI/path, rejection of reachable status 204,
per-backend thresholds, ordered forwarding and recovery, scoped hairpin NAT,
all-unhealthy ICMP rejection, and actual VRRP packet priorities 150/90/150.
A partial backend failure must both forward through the second backend and
advertise priority 90.

The 16:04 JST run stopped during all-unhealthy withdrawal with a TLS connection
reset. The test had waited only for accepted Health observations; publication
precedes asynchronous network application. The reset-time applied rules were not
retained, so their exact state is unknown. The corrected acceptance retains the
8-second application observation bound, reports intermediate client results and
applied DNAT destinations, and requires explicit ICMP rejection quoting the same
client port. A reset is not a passing rejection result.

Final result: PASS, exit 0, 2026-10-30 16:11:58 JST, 68.95 seconds overall.
The final all-unhealthy check observed applied DNAT empty and a matching ICMP
refusal after 146.154726 ms; transient resets=0 and accepted connections=0 in that
check. The initial unavailable state was rejected after 91.741914 ms.
Evidence: `dist/ingress-acceptance-converged/TestDaemonImportedIngressHTTPS.log`.
The product binaries were unchanged after the preceding failure; only the
acceptance synchronization and diagnostics changed.

Product SHA256:

- routerd: `9914a5e3cd9fef238b43e35fe518f8e363a382448e3dc2a733e3b8a491ed59d1`
- routerd-modules: `991e842a93f883bf55b7831ec27a311765fb3cf7a7eccb28d42aad7d1192fc96`
- routerd-logging: `78280205f2493410724a111a9743d4b2761b7dbdb64d9d626f05633d80a76913`

Relevant existing Go checks passed for legacyconfig, health, vrrp, resourceprogram,
runtimeplan and network. The final group-tracking changes also passed the scoped
VRRP/Ingress checks. These checks support, but do not replace, the communication
acceptance above.

## Scope

The fixture declares static LAN addresses instead of exercising the deployed
DHCP client. It intentionally omits DNS, BGP, SAM, a second VRRP router, and the
remaining k8s configuration. It establishes neither physical-device acceptance
nor a two-router handoff, full k8s migration, or performance qualification.
Independent HTTPS peers, the real firewall, and VRRP advertisements establish
only the ingress path described above.
