# OTLP/gRPC logs and protocol measurements

Configure a native service with `otlp_logs`. The independent worker uses the
official OpenTelemetry Go SDKs and an explicit gRPC connection. It owns its
providers; it does not install a global provider or use `OTEL_*` variables to
select its endpoint, headers, resource, export interval, or sampling.

```json
{"id":"collector","service":{"name":"collector","otlp_logs":{
  "endpoint":"https://collector.example:4317",
  "service_name":"routerd2","service_namespace":"edge","min_level":"info",
  "signals":["logs","metrics","traces"]
}}}
```

`endpoint` accepts host:port or an HTTP/HTTPS URL with an explicit port.
Host:port and HTTPS use system-trusted TLS unless `insecure` is set. HTTP selects
plaintext. `headers` supplies explicit gRPC metadata, and `attributes` supplies
string resource attributes. `min_level` is debug, info, warning, or error; it
filters exported logs without suppressing selected measurements.

Native `signals` defaults to logs only. The former `Telemetry` resource defaults
to all three signals, matching its previous selection behavior. Its importer
reports the remaining source gap: metrics and traces currently cover actual
Health probes, DHCPv6 client exchanges, observed firewall denies and configuration
application operations, plus the official Tailscale online peer count. Other controller loops, PPPoE and other former
instrumentation are not migrated by selecting these signals.

Configuration application records the actual entry and return of
`control.Engine.apply` through the same nonblocking local log handler. It emits:

- `routerd.config.apply.operations`, a cumulative operation counter grouped by
  `routerd.config.apply.result` (`success` or `error`);
- `routerd.config.apply.duration`, a seconds histogram from the original
  monotonic elapsed time, with the same result grouping;
- `config.apply`, a span with the original start/end timestamps, result,
  configuration revision and local log identity. Errors carry a generic status,
  not configuration contents or potentially sensitive error strings.

An operation includes configuration preparation and its synchronous application
path. It does not measure later protocol acquisition, every coordinator pass,
configuration persistence or confirmed-commit completion. A failed operation and
its recovery application are separate measurements. No polling interval is
invented for this event-driven operation. Revisions are span/log attributes only,
not unbounded metric dimensions. Collector acknowledgments never gate application.
Startup and exporter replacement keep the existing no-replay behavior; these
counters describe consumed measurements, not a durable lifetime total.

The existing local log owner remains authoritative. This worker reads its Unix
HTTP API and retains the original timestamp, severity, message, producer, local
cursor and scalar attributes. Readiness means the local cursor has been acquired,
not that a collector has acknowledged delivery. Worker startup, destination
replacement and a logger epoch change begin at the current cursor, without
replaying retained history. Temporary local API failure resumes the same cursor.

Health records its actual probe start and finish through the existing nonblocking
log handler. Export failure cannot delay the probe or renew its observation
lifetime. The exporter consumes that measurement once per local cursor and emits:

- `routerd.healthcheck.probes`, a cumulative counter grouped by resource, result,
  target and protocol, exported every 30 seconds;
- `routerd.healthcheck.phase`, the existing Healthy=2, Passing=1, Failing=-1,
  Unhealthy=-2 values grouped by resource and phase;
- `healthcheck.probe`, a span with the original start/end times, resource,
  interface, target, protocol, result and source log epoch/sequence. Every
  observed probe is sampled. Optional route evidence lookup is not part of the
  measured communication interval.

DHCPv6 uses the same local log owner and exporter providers. Each transmitted
transaction records its first send and completion. Elapsed monotonic time is
anchored to that send's UTC timestamp, so a clock correction cannot make its
duration negative. Retransmissions and partial Replies remain in the original
transaction; its acquisition, renewal and expiry deadlines are unchanged.

- `routerd.dhcpv6.client.exchanges`, a cumulative counter grouped by resource,
  interface, operation and result;
- `routerd.dhcpv6.client.exchange.duration`, a histogram in seconds with the same
  attributes and the existing 30-second collection interval;
- `dhcpv6.exchange`, a span with the original interval, successful transmission
  count, operation/result and source log epoch/sequence. `timeout`, `expired`,
  `no_binding` and `error` results carry error status.

Operations distinguish Solicit, Request, Renew, Rebind, Information-request,
Confirm and Decline. A `reply` means the accepted response ended that transaction;
for Solicit it means an offer was selected, not that a lease was acquired. The
interval includes retransmissions and offer selection, not a per-packet round
trip. A protocol timeout, lease expiry, NoBinding response, superseding state,
cancellation or local error is recorded separately. No DUID, assigned address,
authentication data or server identity is exported as a measurement attribute.

The existing `firewall packet` log producer also supplies
`routerd.firewall.deny.total`, in packets, grouped by rule, action, transport and
address family. It counts only actual `drop` and `reject` NFLOG observations;
`accept` and unknown verdicts are not denies. This retains the old firewall
logger's observed-deny counting path. It does not estimate unsampled packets or
reconstruct records lost by the kernel, local queue, retention or exporter.
The homert configurations use deny sampling one and accepted-packet sampling
zero. A one-in-N logging rule therefore yields sampled observation counts,
not the firewall's total packet counter. The old DPI application, expired-flow correlation attributes and conntrack
tuning measurements are not produced by this path. Packet source/destination,
ports and payload do not become metric attributes. This path adds no packet span
and leaves firewall verdicts and packet-history storage unchanged.

Counters reset when this exporter restarts, and phase series can coexist because
phase is an attribute. They describe observed records, not a durable audit of
every exchange or probe: local queue loss or history retention can omit measurements.

The SDK owns each bounded export queue. Logs use at most 1024 records, batches
of up to 200 and a 250 ms flush interval. Traces use at most 1024 spans, batches
of up to 256 and the same interval. Each export has a one-second deadline with
retries disabled. Failed exports and queue overflow can lose remote records;
there is no durable retransmission or exactly-once guarantee. Diagnostics from
the worker's own PID are not fed back into its exporter. All providers share one
two-second shutdown budget before the owned gRPC connection closes. No external
collector daemon is bundled into the product.

`make check-otlp OTELCOL=/path/to/otelcol-contrib` builds the product and runs the
independent official collector in disposable mount, process and network
namespaces. It checks actual supervisor records, metadata and severity filtering,
then actual TCP probe success, endpoint failure and recovery against received
logs, metrics and spans. It also checks destination replacement, collector
outage/recovery, bounded replacement/removal while offline, continuing Health
observations and local disk records, and independent UDP forwarding. ISC DHCPv6
provides actual prefix acquisition, natural renewal and T2 timeout/rebind recovery.
The received logs, metric count/duration and spans are matched to their original
local records. Collector outage must leave the DHCPv6 generation unchanged and
allow its prefix lifetime to advance.
It requires Linux, root or passwordless sudo, iproute2, curl, ISC dhcpd and an
installed collector. `scripts/otlp-peer.py --dhcp6-server /path/to/dhcpd` selects
an explicit independent DHCPv6 server executable.
It never changes host interfaces or the host's collector service.

On 2026-10-30, build `health-telemetry-90f9d96` passed this acceptance with
Collector 0.151.0. The three probe phases retained their source timestamps and
log identities, and 948 concurrent UDP exchanges succeeded without loss. Evidence
is in `dist/telemetry-signals-peer/peer.log`, receiver JSONL files and the saved
binary hashes.

On 2026-10-30 21:44:07-21:45:47 JST, build
`wsl-dhcp6-telemetry-e3f3bd4` passed the extended acceptance in WSL Ubuntu in
99.29 seconds. It was built from `e3f3bd4` plus the then-uncommitted measurement
changes, now product commit `2baeefa`; all six product source hashes match that
commit, and no product source changed after the build. Official Collector
0.151.0 and ISC dhcpd 4.4.3-P1 were independent peers. The fixture used an
18-second valid lifetime, 5-second T1 and 9-second T2, not an ISP's production
lease period. It observed acquisition, natural renewal, a 4.011671380-second
Renew timeout with three transmissions, and successful Rebind with two
transmissions. Logs, spans, error status, cumulative counts and duration sums
matched the original records. While the Collector was stopped, the same DHCPv6
worker generation renewed its prefix lifetime. All 970 concurrent UDP exchanges
succeeded. Health recovery and the existing exporter lifecycle checks also passed.

The reproducible path is `make build RELEASE_ID=wsl-dhcp6-telemetry-e3f3bd4`,
then `python3 scripts/otlp-peer.py --output dist/dhcp6-telemetry-peer` as root
(or with existing passwordless sudo). Evidence: `peer.log`, `binaries.json`,
`product-sources.json`, Collector JSONL and ISC log in that output directory.
The three executed SHA-256 values were:

- `routerd`: `1da234264bc90a942342d3ca21fa52faa7550f65fe07a965c914ed00f75c7244`
- `routerd-modules`: `9d5b7af154ce60fc590428e620ab785c11dc78781855eca732fd39d44b03bce0`
- `routerd-logging`: `1167ce8e5a1decbb7ff98f94bf661148dad8e5557d5ae9277cb2c5c8ad5f4a91`

On 2026-10-30 22:12:19-22:14:29 JST, the extended firewall measurement
acceptance passed in 129.97 seconds with combined product `ffffd46`, release
`wsl-firewall-ffffd46`. The two measurement files match product commit `9baee18`
(integrated as `cefdc42`); the acceptance script was uncommitted during this run.
Real IPv4 UDP packets exercised allow, silent drop and ICMP rejection. The
original NFLOG records and Collector counters agreed at one and then two
observed drops/rejections per rule. A dedicated logged allow rule proved that
accepted packets were excluded; ordinary allowed forwarding remained unlogged.
This explicit allow log is a test condition, unlike homert's accepted-packet
sampling zero. The deny logging divisor was one, matching homert.

With the Collector stopped, the same NFLOG worker retained all three verdicts
and recorded the packets. After exporter replacement and Collector recovery,
fresh drops/rejections produced new counters starting at one, without replaying
old records. All 1,269 concurrent allowed UDP exchanges succeeded. Existing
Health and DHCPv6 measurement checks also passed. Evidence is in
`dist/firewall-otlp-peer/peer.log`, `binaries.json`, Collector JSONL and
`firewall-probes.jsonl`. No product or script changed during this run.
Executed SHA-256 values:

- `routerd`: `f0e45fe4ed30c84d88abc1ac39ec47c459a9881daa5fb6c98fd6e764216fc8e3`
- `routerd-modules`: `0934174f00f28450eae0b3b162eca68b65ce6c6458f5b5f3a3dd504afcb3f11b`
- `routerd-logging`: `55209bf884f664b196a43568dfbf6683ca2e477cf833a1c0fb6b2fd20ff23c88`

The firewall metric acceptance covers IPv4 and logging divisor one. It does not
establish IPv6 measurements, sampled-rate accuracy, loss-free export, old DPI
classification/correlation or target-appliance acceptance.

This acceptance covers OTLP/gRPC plaintext and the exercised lifecycle. It does
not qualify other signal producers, archive-on-delete forwarding, automatic
LAN client address acquisition, physical-device performance, mutual TLS or
deployment-specific TLS trust.


### Combined-source verification

At 21:50:29-21:52:07 JST on 2026-10-30, clean source `3342df0` combined main
`93cc25e`, DNS history and DHCPv6 telemetry. The standard static amd64 build
`wsl-observability-3342df0` passed the same ISC/Collector acceptance in 98.42
seconds. Acquisition, natural renewal, T2 timeout, Rebind recovery and exact
original timestamps were received. During Collector outage the same DHCPv6
worker renewed its actual prefix deadline; routed UDP completed 962 round
trips with no failures. Configuration replacement, removal and fresh receiver
recovery also passed. The DNS history acceptance ran concurrently with the
same three executable SHA-256 values. Relevant Race Detector and vet checks
passed. Evidence: `dist/observability-integrated/{build.json,dhcp6/}` including
`execution.json`, `binaries.json`, actual receiver records and `peer.log`.
This remains a private-namespace functional check, not full homert acceptance.

### Configuration application acceptance (2026-10-31 JST)

The dedicated `a2d959f` checkout plus this change used the normal `make build`
path, release `control-telemetry-a2d959f`, and three inspected static amd64 ELF
binaries. The existing `scripts/otlp-peer.py` completed with exit 0 at
02:42:16–02:44:28 JST (about 132 seconds), using OpenTelemetry Collector Contrib
0.151.0 and an independent ISC DHCPv6 server in private namespaces.

A real configuration commit produced revision 2 with `success` and monotonic
elapsed time 191827752 ns. Its original local record and exported `config.apply`
span agreed on revision, result, identity and both timestamps. The operation
counter was 2, including the captured startup application; the duration histogram
sum 0.811852889 seconds matched those two original measurements. No fabricated
measurement or HTTP response time supplied these values. The existing independent
Health, DHCPv6 and NFLOG checks passed as well. Collector interruption, destination
replacement, recovery and removal preserved 1238 routed UDP replies with zero
failures. This does not establish loss-free telemetry under saturation.

Repeat with `make check-otlp`, or reuse normal build outputs and provide the
existing ISC executable explicitly when it is outside `/usr/sbin`:

```
python3 scripts/otlp-peer.py --binary-dir dist/bin --dhcp6-server /path/to/dhcpd --output dist/otlp-peer
```

The peer check matched successful application measurements. Actual failed
application export, wall-clock correction during application, target hardware
and every controller loop remain outside this acceptance. The selected control
and legacy-import Race Detector checks and `go vet` passed. Initial unprivileged
control checks stopped at existing private-state directory validation; the same
checks passed with administrator privileges. A first peer invocation stopped
before starting any product because the default ISC executable was absent;
using the existing peer-environment executable resolved that prerequisite.


### Tailscaleのオンライン対向数

公式デーモンの既存の状態取得が成功した際に、`Peer.Online` が真の対向を数えます。
自身は含めません。旧実装と同じ `routerd.tailscale.peer.count` を、単位 `{peer}` のゲージとして転送します。
属性 `routerd.tailscale.node` には設定されたリソース名だけを使います。
対向の名前、アドレス、認証情報は計測属性へ含めません。

取得済みの状態から10秒間隔で記録し、既存の30秒間隔の転送処理へ渡します。
新しい状態取得処理や外部デーモンは追加しません。転送先の応答を通信や状態報告の条件にしません。
取得失敗や実行権限の期限切れは、対向数ゼロとして記録しません。
この値は公式デーモンのオンライン判定です。接続数、通信成功数、鍵交換の成功数を表すものではありません。
転送担当の再起動時には履歴を再送せず、次の成功した状態取得から値を再び記録します。

受入は既存の独立した対向試験を使います。正規の製品と配布ツールを構築し、
固定版のTailscaleエンジン・ソースと試験用の対向実装を用意した環境で実行します。

```sh
python3 scripts/tailscale-peer.py --homert --collector /usr/local/bin/otelcol-contrib
```

公式CLIで対向を停止・再開し、その後に収集された計測値を公式 LocalAPI の実状態と照合します。
復帰後には三つのLAN経路とIPv4・IPv6の出口でTCP・UDPの応答を確認します。
追加した停止・復帰区間の証拠は、実ソケットの応答と LocalAPI・Collector の照合です。
対象実機の受入や、通信を継続できない時間の上限を保証する試験ではありません。

2026年10月31日10:46〜10:49 JST、専用版 `tailnet-metrics-native102` で受入が成功しました。
基点は `b28ee41` で、今回の計測処理と受入スクリプトを加えた版です。所要時間は175.556秒でした。
独立した OpenTelemetry Collector Contrib 0.151.0 で、オンライン・停止・復帰の各操作後に
新しい計測点 `1 → 0 → 1` を受信し、公式 LocalAPI の実値と照合しました。
製品全体の再起動、設定の撤去・復元、管理用SSH、三つのLAN経路、IPv4・IPv6の出口通信も通りました。
通常の製品と配布ツール、固定版エンジンとそのソース一式が必要です。
初回の準備不足では配布ツール、次の試行ではエンジンのソースがなく、いずれも製品起動前に停止しました。
依存物を用意した後は試験条件を変えていません。
