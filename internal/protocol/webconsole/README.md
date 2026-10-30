# Read-only operator console

A configured `web_console` service runs in the existing Go module executable.
It owns its TCP listener and HTTP lifetime; there is no external web server,
JavaScript build, second resource store, or systemd service.

```yaml
schema_version: 2
resources:
  - id: service/console
    service:
      name: console
      web_console:
        listen: 198.18.0.1:8080
        base_path: /ops/
        title: routerd2
```

Declare the local address and its firewall access separately. The listener uses
HTTP, as the original read-only WebConsole does. `ManagementAccess` import checks
its declared trusted interface/zone and binding; it does not invent a source ACL.

The initial pages display live control health, scoped kernel interfaces/routes,
resource-condition pages, current DHCPv4 leases, and confirmed-commit metadata. Health failure remains
HTTP 503 with the actual observation. Missing observations and truncated kernel
views remain explicit. Resource condition readiness is not a kernel readback.
The resource continuation uses the control API's existing observation identity;
a concurrent change returns 409 and the operator can start a fresh page.

The DHCP page lists configured DHCP services from `/v1/services` and reads the
selected service's complete typed observation from `/v1/services/{name}`. It
shows hostname, MAC/IP, lease deadline, serving/standby and synchronization state.
The observation's original boot-clock deadline determines whether the view is
current. A lease is not evidence of present connectivity; released leases are
removed by the DHCP owner, and this page does not store or invent a history.

The old console's title, base path, links, escaped HTML and read-only method
handling are retained. Its resource-store adapter and React status model are
replaced by the current typed control APIs. This is an initial management path:
correlated client inventory, further protocol detail, WireGuard/SAM pages,
and the old complete UI surface are still outstanding. Configuration documents,
mutations, and private worker APIs are not exposed by this listener.

## DHCP console acceptance

On 2026-10-30 at 20:38:32–20:38:46 JST, the management communication check ran
against base `eb66441a4f29` plus this console change. The embedded release identity
is the base ID, not a claim that the unmodified base has the new page. Build the
standard artifacts and bundled engine with `make build dnsmasq-engine`, then run
`python3 scripts/daemon-tests.py management --output dist/web-history-management-final`
(or use `make check-management` without retaining artifacts).

A separate BusyBox client acquired its address from the configured dynamic pool.
The independent HTTP client read the actual hostname, MAC, address and deadline;
a requested renewal changed the displayed deadline, and a real DHCPRELEASE
removed the row. The management peer retained its explicitly configured address;
the DHCP client used a separate initially unaddressed interface. The same run
passed existing SSH commit/confirmation/rollback and control-process recovery
checks in 16.34 seconds with the race-enabled integration harness. The original
75-second child and 100-second harness limits were unchanged.

The management fixture now uses the existing network-namespace isolation helper:
root checks use a private network namespace with the engine's normal privilege
setup. Its former user namespace with denied setgroups could not start dnsmasq.
The product privilege behavior was not changed. The acceptance compares decoded
HTML text, preserving normal escaping of the timezone's plus sign.

SHA-256 of the tested standard build artifacts:

| Artifact | SHA-256 |
| --- | --- |
| `routerd` | `9269af7a4b7f3cec3e14aec1e50716ad70fd967d08db9e88ac0c2fd3680d0ea7` |
| `routerd-modules` | `b9a7890c35dcec2e31d96416b708fb4cdbabf2195f0d65c77fd48ba2735b88b6` |
| `routerd-logging` | `31f0349d7486829c95e31ad2a31301b487614615375fc0b0b2a42e1613df673e` |

The captured HTTP page was rendered at 1200×800 and 500×844; all five columns
wrapped without overlap. Historical connection search, correlated client history,
HA synchronization display under failure, and physical homert acceptance remain
separate work. This short client-requested renewal does not establish natural
lease renewal over the normal operating interval.

## Saved communication search

The communication page reads the collector-owned SQLite history through
`GET /v1/services/{name}/traffic-history`. The runtime resolves the configured
service path and releases its mutex before opening the database read-only.
It reuses routerd's `OpenTrafficFlowLogReadOnly` and `TrafficFlowLog.List`;
there is no second writer, copied history, or query process.

Searches accept exact client/peer IPs, protocol, and inclusive RFC3339 start-time
bounds. Equivalent IPv6 spellings are normalized. The default is the newest
100 rows, with a requested maximum of 1000; one extra row determines whether
the result was truncated. A visible notice asks the operator to narrow the
search when necessary. This is a bounded search, not a stable paged export.
The query has the original console's 1500 ms database budget, independently of
network application. Missing or unreadable history returns HTTP 503 instead
of pretending that no communication occurred.

The page shows original endpoints, translated address, classification engine,
DNS/TLS/HTTP names, accounting availability and packet/byte counters. A missing
end observation is not labelled as current connectivity. The existing
`make check-traffic-flow` fixture includes independent HTTP requests against
the actual product while real TLS/UDP traffic populates the existing database.

## Tailscale observation view

The VPN page uses the revision returned by `/v1/services` to read the existing
typed configuration document. Only interface, hostname, advertisement intent,
route acceptance and inbound-blocking policy enter its template. Control server
URLs, authentication files and persisted engine state are not display data.
The selected canonical service name reads `/v1/services/{name}` so a truncated
list observation does not hide peer details. No CLI polling or second store is
introduced.

Membership, addresses, peers, allowed peer prefixes and byte counters come from
the existing Tailscale observation. Its original boot-clock deadline applies to
the whole observation, including peers. Expired data is labelled as the last
observation, with current connectivity unconfirmed. A configured but inactive
service remains unobserved. Known authentication states are shown without an
authentication URL. Advertised local routes are explicitly configuration intent,
not a claim that the control server approved them or that forwarding works.

`go test -race ./internal/protocol/webconsole` checks actual Unix HTTP reads and
escaped HTTP rendering for current, expired, authentication-required and missing
observations. These focused checks use recorded observation inputs and do not
establish real Tailscale forwarding; the existing `check-tailscale` and
`check-tailscale-homert` communication checks own that acceptance.

### Tailscale console communication acceptance

On 2026-10-30, the integrated product at `b32a43c` with release identity
`wsl-console-b32a43c` passed the existing Tailscale communication checks on a
separate Windows machine running Ubuntu under WSL. The normal run completed at
21:11:18–21:13:30 JST (132.4 seconds); the homert advertisement run completed at
21:13:57–21:14:43 JST (45.9 seconds). Both exited successfully. The acceptance
script change was integrated as `305fbc2`; removing an unused helper return in
`735682a` did not change the product or acceptance conditions.

The standard signed product bundle ran as PID 1 in its own mount, network and
process namespaces. The independently checked peers were Tailscale 1.104.2 and
Headscale 0.29.6. An independent LAN client read the actual product console and
matched its observed membership, both address families, peer and configured
advertisements. The console did not expose enrollment keys, their file paths,
control URLs or registration URLs. Whole-product restart retained both the
actual communication and displayed membership. Revocation displayed the need
for reauthentication; reauthentication restored traffic and the observed view.

The homert case advertised all three configured RFC1918 prefixes. The peer's
kernel routes and both-direction LAN TCP/UDP worked without subnet NAT or
manual return routes. Both IP families also passed exit traffic through the
declared WAN NAT. Removing the Tailscale configuration withdrew routes and
connectivity and displayed no configured Tailscale service. Reapplying it
restored routes, communication and the console view. Packet captures backed the
existing communication checks. Physical homert02/03 acceptance, physical WAN
switching and long-duration operation remain separate work.

Use the existing `make check-tailscale` and `make check-tailscale-homert` targets.
With the same standard build, engine and peer prerequisites already present,
run `python3 scripts/tailscale-peer.py` and the same command with `--homert`.
The scripts create and remove their own isolated network, process and mount
namespaces. The recorded outputs were `dist/vpn-accept.log` and
`dist/vpn-homert-accept.log`; one actual HTTP response was retained temporarily
as `dist/vpn-live.html` for layout review.

SHA-256 of the product used by both checks:

| Artifact | SHA-256 |
| --- | --- |
| `routerd` | `dd5a345ced5192c09b0bfcb73e9ab5e4cb33070b340d995f916e77fa852ab678` |
| `routerd-modules` | `61230e7c61ac672f6621d3ca9ff934d0d5162ba2547a8002229624b6862e3eeb` |
| `routerd-logging` | `4be6777af1a9daf85b4a2c4017120949428ebbcdb8ad6fadc8e889591fc6c798` |

### History search communication acceptance on a separate machine

On 2026-10-30 at 21:14–21:16 JST, the standard product build `b32a43c`
(`wsl-console-b32a43c`) passed both existing traffic-flow checks on Ubuntu
26.04 under WSL 2, Linux 6.18.33.1, on a separate Windows computer. The fixture
was `735682a`; each check completed in 22.15 seconds with the race detector.
The three product SHA-256 values are identical to the Tailscale checks above.
The same build also passed the existing management check in 14.41 seconds.

Independent LAN TCP/UDP traffic crossed source NAT, and an independent HTTP
client searched the collector's real stored rows by client address, protocol
and original start time. Built-in and reused nDPI classification both retained
real TLS names and counters. The console returned unavailable, rather than an
empty success, while storage was unusable; actual forwarding continued. After
whole-product restart the original connection was still searchable. After
closure its original end time appeared, and retention removal disappeared
from the HTTP result. The existing UDP archive check also passed.

Reproduce through `make check-traffic-flow` after the standard build. The
recorded output was `dist/wsl-flow-console/`. A later nDPI run captured one real
HTTP response for local layout review using a temporary test-only write; that
write was removed and the fixture restored exactly. These results establish
the saved-history operator path, not full homert natural renewal, HA, hardware
qualification, or the remaining client-inventory and protocol UI features.

Both real HTTP responses were rendered offline in a dedicated browser profile
and inspected at a desktop width of 1280 pixels. The initial existing-browser
inspection tool stopped because it could not identify the current URL; the
offline renderer opened only the saved local responses. A subsequent wording
correction changed Japanese labels/errors only, without changing data access,
protocol handling or the acceptance conditions.

## Saved DNS query search

`<base_path>dns-queries` reads the selected DNS service through the typed control
endpoint `GET /v1/services/{name}/dns-history`. The runtime resolves the prepared
query-log path and releases its lock before the old routerd read-only
`logstore.OpenDNSQueryLogReadOnly` and `List` calls. The resolver remains the sole
writer and retention owner. No new database, background collector, listener or
command polling is introduced.

Search supports exact client IP, query-name patterns, response code and inclusive
RFC3339 time bounds. The old query-name matching is retained: case-insensitive
LIKE matching, optional final dot, `%` for any length and `_` for one character.
Results are newest first, default 100 and maximum 1000, with an explicit notice
when another matching row exists. This is bounded search rather than pagination
or complete export. The page displays original query time, client, name/type,
response code, answers, upstream, cache use and duration. Historical results do
not assert present reachability. Disabled logging, an absent database and a
failed read are reported as errors, not successful empty history. A request may
read the previously selected database while its configuration is changing.

The existing DNS retention acceptance exercises actual DNS requests and a
separate LAN HTTP client. Query recording and retention keep their original
behavior; reading history does not hold the DNS worker's write lock.


On 2026-10-30 at 21:34:33-21:34:49 JST, standard product `4f429de`
(`wsl-dns-history`) passed this check in 15.34 seconds on the separate WSL
machine. The fixture was committed as `b744037`. Actual UDP/TCP DNS answers,
NXDOMAIN, exact stored row IDs and client/name/response/time filters matched
an independent LAN HTTP client. The original two-second retention window was
unchanged. Expired rows remained searchable during archive unreachability and
a DNS worker restart, then disappeared after archive recovery. Policy removal
left query recording working. Concurrent routed UDP completed 142 round trips
with no failures. The three distribution executables were statically linked.

Evidence: `dist/wsl-dns-history-peer/{peer.log,execution.json,binaries.json,
forwarding.jsonl,dns-console.html}`. Reproduce with `python3
scripts/retention-peer.py --dns-queries --binary-dir dist/bin --output
 dist/wsl-dns-history-peer` in the isolated development environment. The saved
real HTML response was also rendered offline at 1280 pixels and inspected for
Japanese text, table visibility and overlapping elements. This result covers
the DNS history operator path; it does not establish homert full-configuration
natural renewal, two-node acceptance or target-hardware qualification.


The combined source `3342df0` includes main `93cc25e`, DNS history and DHCPv6
telemetry. Its standard `wsl-observability-3342df0` build passed the same DNS
history check at 21:50:29-21:50:44 JST in 15.31 seconds. The source tree was
clean. The concurrent DHCPv6/Collector check used byte-identical executables;
evidence and SHA-256 values are under `dist/observability-integrated/dns/`.
The API error wording is Japanese in this tested version.


## Saved firewall event search

`<base_path>firewall-events` reads `GET /v1/services/{name}/firewall-history`
for a configured `firewall_log` service. The runtime releases its state lock
before opening the collector-owned database through the existing routerd
`OpenFirewallLogReadOnly` and `List` functions. The NFLOG collector remains the
sole writer and retention owner; the reader starts no process or database.

Filters retain the old console's query capability: exact source IP and action,
an inclusive RFC3339 start time, and a newest-first limit of 100 by default,
maximum 1000. Equivalent IP spellings are normalized. The old store caps each
read at 1000, so `at_limit` says only that the requested limit was reached; it
does not assert that another matching event exists. End-time filtering, paging,
destination filtering, address-set enrichment and connection correlation are
not implemented by this route. The table shows the original packet timestamp,
rule, action, family/protocol/TCP flags, addresses/ports, interfaces and size.
These are stored sampled events, not current rules or total traffic counts.
An unavailable database or failed query is an HTTP error rather than empty
history. The old stored event type and query code are reused directly.

The firewall history path was accepted on 2026-10-30 22:12:44-22:13:04 JST
using the standard `wsl-firewall-ffffd46` build (source `ffffd46`).
`TestDaemonFirewallPacketLogs` completed in 14.55 seconds through the existing
`daemon-tests.py firewall` runner. An independent HTTP client matched stored
IPv4/IPv6 UDP drop/reject and TCP SYN observations by row ID and the source,
action and time filters. The same UI retained records across whole-product
restart and real archive-send failure, then reflected expiry only after archive
recovery. The original eight-second retention and one-second maintenance period
were unchanged. A six-row HTML response was saved and rendered at 1280x1400;
Japanese labels and packet fields were legible without overlap. These are
isolated Linux namespace results on WSL, not target hardware qualification or
acceptance of the complete homert configuration. Evidence is in
`dist/firewall-integrated/` (execution metadata, log and captured HTML).


## 運用ログの検索

`<base_path>logs` は、ログ担当の `logging.sock` にある `GET /v1/logs` を直接読み取ります。
旧画面のイベント検索と継続取得の用途を、既存の型付き `QueryResult` と `Status` へ接続しました。
新しい保存先、配信処理、ログ変換器は作りません。検索時にネットワークの適用処理を呼び出しません。

発生元の完全一致、最低重要度、取得件数で検索します。既定は最新100件、上限は200件です。
検索範囲はメモリー内の保持範囲で、起動時に保存から読み戻した末尾を含みます。
保存ファイル全体の検索ではありません。各記録の元の時刻、発生元、世代、連番、重要度、本文、属性を表示します。
HTMLの引用処理を維持し、機密属性の除去は既存のログ担当が所有します。

継続リンクはAPIが返した `Cursor` をそのまま使い、検索条件を保持します。
途中で条件に合わない記録があっても、表示行の連番から継続位置を作り直しません。
初回は最新の一致範囲、継続取得は返された位置以降の古い順です。
ログ担当の再起動による `Reset`、保持範囲や発生元などに由来する `Gap` とその理由を表示します。
一致する記録がない場合と、取得できなかった場合を区別します。

保存状態は検索と同じ応答の `Status` を表示します。
保存の稼働・停止、保存待ち件数、欠落件数、最後の保存エラーと成功時刻を確認できます。
`DiskDegraded` は劣化が記録されたことを示すため、現在も保存できないと決めつけません。
`DiskRecoveryRequired` は修復後の再起動が必要であることを明示します。
物理ディスク障害中の画面確認は、この画面追加の受入では未確認です。


## 時刻同期の観測

`<base_path>time` は、既存の `GET /v1/services` から現在のNTPサービスを選び、
`GET /v1/services/{name}` の完全な `ntpconfig.Snapshot` を読み取ります。
一覧に含まれない条件付き停止中のサービスを、未構成や故障と決めつけません。
旧WebConsoleの資源状態確認を、現在の実chronyd観測に接続するページです。
追加の時刻取得、計測、保存、プロセスはありません。

同期状態、選択された時刻源、階層、参照時刻、時計補正の有効・無効、残り補正量を表示します。
時刻偏差の二乗平均平方根、周波数補正と不確かさ、基準時刻源までの遅延と誤差、うるう秒状態も元の値を使います。
単位は各項目に表示し、符号や精度を変えません。
同期済みの表示を、LAN端末が時刻を採用できることや配布成功の保証には使いません。

現在性は、観測元の起動識別子と `ObservedNanos`・`ExpiresNanos` で判定します。
元の期限を延長せず、期限切れでは最後の観測として表示します。
観測失敗時は `LastError` を示し、取得できなかった数値をゼロの正常値として表示しません。
プロセスが存在しても観測がなければ未観測です。API読取失敗はHTTPエラーのまま返します。


### 時刻画面の実通信確認

2026年10月30日23:12:29～23:12:51 JSTに、Windows上のWSL Ubuntuで静的時刻源の受入を確認しました。
製品は `a646df6`、配布識別子は `wsl-operations-a646df6`、試験は `a5c8357` です。
22.35秒で終了コード0でした。同じ配布物のchronyd 4.8を独立した対向として使用しました。

独立LAN端末が画面を読み、同期した実観測の時刻源・この装置の階層・偏差・品質の値を制御APIと照合しました。
隔離製品の監督と時刻担当を一時停止し、制御APIと画面を動かしたまま、元の5秒期限で期限切れ表示になることを確認しました。
再開後は新しい実観測で同期表示へ戻りました。時刻担当はPID899・世代1からPID1149・世代3へ再起動しています。
同じ世代での継続成功ではありません。独立chronydの `-Q` によるLAN時刻取得と、上流・LAN双方のNTPパケットも確認しました。

最初の試行では同期と期限切れ表示まで成功しましたが、復帰時の制御APIへの一時接続断で試験が終了しました。
監督再開時に他の担当も生存通知の期限切れで再起動していました。
試験を修正し、既存30秒の復帰待ちの範囲で接続失敗だけを再観測しました。
HTTPエラーやJSON解釈エラーは隠さず、製品・5秒の観測期限・時刻品質の条件は変更していません。

再現は標準の `make build chrony-engine` 後に、既存の
`python3 scripts/ntp-peer.py --profile static --output dist/snapshot-time-integrated/ntp` を実行します。
統合後に詳細ログと一時HTMLを削除しました。実行版、条件、結果は本節に残しています。
HTML採取だけの一時差分は復元済みです。
実HTTP応答はEdgeで描画し、日本語、数値、階層表示、配置を確認しました。

この受入はシステム時計の補正を無効にした静的時刻源の経路に限ります。
DHCP由来の時刻源切替を含む画面、実機の時計補正、homert全設定と二台構成はこの試験では未確認です。


## 署名付き更新・復旧の状態

`<base_path>updates` は、既存の `GET /v1/releases/status` と `GET /v1/version` を読み取ります。
DIST-01で追加した署名付き実行版の運用を対象にしています。
旧WebConsoleと同じ読取専用の範囲で、更新や再起動の操作、新しい保存先は追加しません。
署名付き実行版の管理が構成されていない場合は、既存APIのHTTP 501をそのまま示します。

制御担当の実行版と、起動に選択された版、予約された版、前版を区別します。
各選択では版、連番、SHA-256、組み合わせる保存設定のリビジョンとSHA-256を確認できます。
選択と実行版は別々の要求で取得するため、同時点の一括観測ではありません。
予約や復帰先の選択だけで、実行中の版が切り替わったとは判断しません。

保存された正常版は、試行中であれば前版、それ以外では選択された確定版です。
導入時に登録した基準版を含むため、現在の全サービスの正常性を表すものではありません。
試行番号、試行回数の上限、元の開始時刻を表示します。
実際の試行期限はAPIで公開されていないため、残り時間を推定せず、取得できないことを明記します。
試行を確定・失敗させる権限を持つ `Attempt.Token` は、画面用の型へ渡しません。

直近の失敗には失敗した版、時刻、理由を表示し、過去の記録であることを明示します。
失敗記録が残っていても、現在の障害や復旧完了を断定しません。
APIの取得失敗を空の正常状態に置き換えず、理由などのHTML引用処理を維持します。

### 更新・復旧画面と領域回復の受入

2026年10月31日00:03:56～00:04:01 JSTに、独立したWSL Ubuntuで受入が成功しました。
製品は `f93be4c`、配布識別子は `wsl-operations-f93be4c`、試験は `bcd58c6` です。
標準の `make build` で作成した署名対象の実行物を、既存の `make check-update-rollback` で使用しました。

実SSHと制御用Unixソケットを通した領域回復で、古い世代の要求と使用中・予約中・前版の削除を拒否しました。
指定した不要な版を削除した後は、満杯だった領域へ次の署名付き実行物を格納できました。
選択と復帰制限の値は変わりませんでした。

実HTTP応答は確定・予約・取消・試行中・失敗後の保存状態と一致しました。
失敗理由に含むHTML特殊文字も引用処理されました。初回は試験が版文字列にない空白を想定して失敗しました。
試験の比較だけを修正し、製品、試行期限、合格条件は変更していません。
同じ製品の実HTTP応答をEdgeで描画し、日本語、版と状態の区別、折返し、配置を確認しました。
HTML採取だけの一時差分は復元済みです。

初期選択と試行・失敗の遷移は、実際の保存処理を試験内で呼び出して準備しています。
この受入は実再起動、自動正常確定、転送通信の成立を示すものではありません。

## BGPの相手と学習経路

`<base_path>bgp` は、既存の `GET /v1/services` と `GET /v1/services/{name}` から
BGP担当の完全な `bgpconfig.Snapshot` を読み取ります。独自の観測・保存・経路適用は行いません。
相手ごとの接続状態・自律システム番号・接続元と、学習した選択経路の次ホップ・学習元・経路表・優先度を表示します。
相手の再起動を待って保持する経路には、その状態を表示します。
現在性は既存の `CurrentObservation` で判定し、元の起動識別子と観測期限を維持します。
停止時・期限切れの観測を現在の接続確立として表示しません。条件付き停止中の構成は一覧に含みません。
接続確立の表示と、カーネルへの適用・端末通信の成立は別々の確認です。

2026年10月31日04:28 JSTに、配布識別子 `bgp-console-native93` の標準ビルドを使い、
既存の `TestDaemonBGPTwoFRRPeers` が27.06秒で成功しました。
独立したLAN端末がHTTPで両相手と選択経路を読み、片側の再起動中には保持された経路を確認しました。
両相手とのTCP・UDP通信、片側の経路撤回と復帰、製品の再起動後の経路と画面の復帰も確認しました。
実HTTP応答はEdgeで描画し、日本語の項目・数値・表の配置に重なりがないことを確認しました。
この試験は隔離したLinuxネットワーク名前空間での受入です。対象実機やhomert全設定の受入ではありません。

再現は標準ビルド後に `python3 scripts/daemon-tests.py bgp-frr --test TestDaemonBGPTwoFRRPeers` を実行します。
独立対向には既定の `/usr/lib/frr/bgpd` を使用します。
