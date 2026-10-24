# HealthCheck probe履歴のdurable接続

2026-10-24 UTC。product checkpointは `205f02fc9abd5689d1a9543af5104f16c1e3d2ea`。
CORE-03 / CORE-04 / NET-01の部分checkpoint。全work packageや実機利用の合格ではない。

これまではHealthCheckの有限snapshotを送っていたが、固定routerdの `ApplyResult` が生成する
probeごとのtyped eventは破棄していた。今回はその元eventをworker側のprivateなWAL/FULL queueへ
保存し、実controlの限定受付からjournalへ公開した。EventRuleもこの履歴を実際に集計する。

## 実装した契約

- probeごとのpassed/failed/timeoutと元の `LastCheckedAt`、daemon/resource detail、network evidenceを保持する。
  phaseが同じprobeも記録する。停止時に架空のprobeを作らない。snapshotの `ProbeEvent` は
  process内callback専用の `json:"-"` で、有限factのRPC bodyへ履歴を混ぜない。
- immutable instanceがstate root下のper-service `health-events` を選ぶ。operatorのHealthCheck設定は
  このpathを指定できない。config digestやworker generationが変わっても同じpending queueを使う。
  SQLiteのexclusive lock・private path・WAL/FULL・version CASは既存storeの契約を使う。
- pending queueをproducer stateに保存する。最大64件かつencoded state 64KiB。ACK済みprefixは
  local queueから除き、新しいenqueueの領域を戻す。既存output-job tombstoneを削除するAPIは追加しない。
  target journalはstable IDを保持してdedupする。容量不足では保存済み分を捨てずに拒否する。
- 200ms周期、最大16件、request deadline 1秒。全IDが順序通り明示ACKされた後にだけprefixを除く。
  ACK待ち中のenqueueは残す。remote失敗、ACK消失、local ACK保存失敗、worker再起動でも同じID/内容を再送する。
- `POST /v1/protocol-events` は現在のsupervisor、config digest、generation、登録済みproducer epochと
  supervisor-ready workerを確認する。そのHealthCheckのprobe topicとtyped detailだけを受け付ける。
  全pageを先に検証する。partial target保存はACKせず、page再送で保存済みprefixをdedupする。
  OSのUIDを共有するworker間の完全な権限分離が完成したという主張ではない。
- 履歴には元のproducer metadataを保存する。過去のboot/configの履歴を送っても、現在のproducer登録や
  fact更新、kernel権限の復元は行わない。登録済みcurrent factとsupervisor観測の状態を変更しない。
- producerごとの保存失敗をboundedな `health.protocol_events.last_error` / `protocol_event_error` へ集約する。
  一方のproducer成功で他方の失敗を隠さない。不健全時はtrial確定を拒否するがcommit済みsourceは保持する。
  sourceから削除したproducerのerrorは除く。worker側はdelivery errorの変化と回復をlogへ出す。

local保存に失敗したprobe結果は現在factとして公開せず、workerを停止する。既に保存したpendingは保持する。
probe実行とdurable acceptance自体は一つのhardware transactionではない。enqueue commit前のcrashで
未保存probeを復元できるという保証はしない。historical outputから現在のUpを再構成しない。

## 試験結果

| 経路 | 証明した範囲 |
| --- | --- |
| full check/build | clean `205f02f` から `make check build-linux` 110.66秒、exit 0。unit・Python14・Race・vet・静的amd64/arm64。PASS |
| worker/control/runtime Race | producer fencing、current fact非更新、partial storage retry、foreign resource拒否、明示ID ACK、per-producer不健全/回復。PASS |
| confirmation/rollback unit Race | protocol保存失敗中の確定拒否、期限切れrollbackによるproducer除去とhealth回復。追加checkpoint `3082946` でPASS |
| relayの実SQLite | ACK消失/reopen、target保存後local ACK trigger失敗/reopen/dedup、ACK中のenqueue、64件/64KiB容量と全保存済み分の保持。PASS |
| rolling local queue | `MaxJobs=1` のstoreで80件をenqueue/ACKしても完走。local ACK済み分が永久jobを増やさない。PASS |
| PVE実daemon | actual TCP probe→raw履歴とEventRule count、既存Derived経路、logging/health/control再起動、旧publisher409、DB失敗時も別channelのfact sequence増加。9.61秒PASS |
| 実daemon backlog | target DB拒否中の4件をqueueから読み、health/control SIGKILL後に解除。全4件の元ID・内容がtargetに一度ずつ存在することを確認。PASS |
| QEMU raw/Derived/EventRule | 外部NICなしのfresh guestで同じ拡張試験、元queue6件を保持・回復。67.82秒PASS |
| QEMU関連回帰 | 二WAN Health/PBR88.85秒、管理/実SSH36.55秒、EventRule62.06秒、BFD/BGP53.44秒。すべてPASS |
| 実socket | TCP・HTTP・DNS・raw ICMPをroot-owned stageのRace harnessで実行。PASS |

[全体check](2026-10-24-health-protocol-events/check-build.log)・[ELF hash](2026-10-24-health-protocol-events/build-manifest.json)、
[relay Race](2026-10-24-health-protocol-events/relay-race.log)・[worker ACK Race](2026-10-24-health-protocol-events/ack-fixed-race.log)、
[confirmation/rollbackと受付Race](2026-10-24-health-protocol-events/control-race.log)、
[実daemon](2026-10-24-health-protocol-events/native.log)・[native provenance](2026-10-24-health-protocol-events/native-manifest.json)、
[実socket](2026-10-24-health-protocol-events/sockets.log)、
[QEMU manifest一覧](2026-10-24-health-protocol-events/qemu-manifest.json) に範囲とhashを保存した。
QEMUは一系統ずつfresh ELFをbuildして実行した。全22 gateの新しいaggregate合格とはしない。
product/integration sourceは `205f02f` と一致し、その後は追加relay unit test、文書とsuite説明だけを変更した。
追加unit testには関連Raceとvetを実行した。古い全体checkを追加test実行の証拠へ流用しない。

## 失敗を固定してから修正した点

test-first commits `2320335` / `665493f` / `0dd94c7` で未実装APIのFAILを保存した。
`b8bcb0a` の拡張integration fixtureを変更前 `1ba9020` のdaemon ELFで実行すると、
最初のraw failed probe履歴待ちで10秒timeoutした。[FAIL excerpt](2026-10-24-health-protocol-events/native-red-excerpt.log) と
[変更前ELF manifest](2026-10-24-health-protocol-events/before-build-manifest.json) を保持する。
これを新実装の9.61秒/67.82秒の合格と区別する。

初回wiringでは、nil snapshotがJSON `null` としてencodeされ、factを含まないproducer envelopeの受付に
合わなかった。`Snapshot` を省略する形へ修正し、matching ACK testを弱めずに通した。
[初回関連Race](2026-10-24-health-protocol-events/wiring-first-race.log)・
[修正後ACK Race](2026-10-24-health-protocol-events/ack-fixed-race.log) とfull checkに記録する。

実DB faultでは `events` INSERTをHealthCheck sourceだけ拒否し、current Health factのsequenceが増えることを
確認してからqueueを読む。保存済みqueueがjournalへまだ存在しないこと、再起動後の旧generation POSTが409、
解除後の元event JSONが完全一致で一度だけ存在することを検証した。QEMU電源断の代用とはしない。

原logはprivate `dist/goal-validation` に保持する。公開版はworkspace/private stage path、
公開kernel certificate identifierとserial末尾空白を正規化した。
[provenance](2026-10-24-health-protocol-events/provenance.json) の原SHAと
[公開SHA256SUMS](2026-10-24-health-protocol-events/SHA256SUMS) を分ける。

## 残作業・未検証

他protocolのraw event producer、producer retirement/deletion時の残queue回収、source切替をまたぐ全受入は残る。
現在のretained queueはservice再有効化・fresh current登録後に再送する。永久削除後の自動回収が
完成したとは数えない。local/target容量とfilesystem全体の限界、retention/archive/容量alarm、規模/遅延、
live queueのQEMU abrupt power-loss/reboot、released version間のmigration/rollbackも未完了。
一般status reference、他provider/構造化field/Derived連鎖、作用の権限/idempotency、federationと全計画を続ける。
対象機種・NIC・性能条件は未確定で、実機利用可能とはまだ判定しない。parityを上げない。

読んだ実装は `internal/protocol/health/`、`internal/serviceruntime/{worker,health}.go`、
`internal/serviceconfig/{config,events}.go`、`internal/runtimeplan/{runtime,resource_conditions,bfd_cache}.go`、
`internal/control/{control,server,state_views,eventrules}.go`、`internal/events/`、
`internal/integration/derived_test.go`、`scripts/qemu-network.py`、固定routerdの
`pkg/{healthcheck,api,controller/vrrp,controller/bfd}`。適用ルールは `../AGENTS.md`。
