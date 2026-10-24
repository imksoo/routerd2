# 設定producer outboxと実daemonの永続イベント

2026-10-24 UTC。コードcheckpoint: `7bdf27f`。

## 実装済み

commit・confirm・confirmed rollbackの観測を、設定本体と同じ `config.json` の
atomic fsync/renameレコードへ保存する。pending candidateや失敗した適用は履歴へ流さない。
イベントはrandom ID・操作・revision・観測時刻のみで、設定本文・鍵・request文字列を含まない。

control workerはprivate WAL/FULL journalを開き、設定lock外でoutboxを転送する。
ACK済みprefixだけを現在の設定レコードから除去する。並行commitは未ACK prefixを保持する。
転送後のACK保存に失敗した場合やcrash後は同じIDで再送し、journal側で重複排除する。
producer outboxは64件まで。confirmed trialにはconfirm/rollbackの1枠も先に予約し、
容量不足の新commitはstage・kernel変更前に503で拒否する。

`GET /v1/observations?after_cursor=N&limit=L` はcontrol epochに依存しない永続履歴を返す。
limitは1..16、unknown/duplicate queryは400、tipを越すcursorは409。
`health.event_journal` へpending件数・delivery errorを公開し、失敗をunhealthyとして示す。
既存 `/v1/events` の揮発hintや、有限なprotocol factとは別契約である。

## 検証済み

| 検証 | 結果 |
| --- | --- |
| 設定とoutboxの同時復元・同一ID再送 | PASS |
| 実journal容量不足、pending保持、予約枠でrollback、回復後の全履歴転送 | PASS |
| journal ACK後の設定保存失敗 | config.json位置をdirectoryにして実rename失敗。pendingとhealth error保持、復元後は重複なし。PASS |
| 候補の適用失敗 | committed eventを生成しない。PASS |
| strict cursor/queryとbounded page | PASS |
| 全体checkとstatic amd64/arm64 build | clean checkpointでPASS。unit/Python/Race Detector/vetを含む |
| 実SSH commit/confirm/rollback・control SIGKILL | rootful隔離namespaceで22.48秒、QEMUで36.18秒。履歴ID/cursor不変、重複なし |
| 新コードの二WAN Health/PBR | 実namespaceで30.51秒、PCAPを保存。QEMUで73.24秒。全worker/supervisor復旧、旧通知・期限・confirmed rollbackを含む |
| 新コードのBFD→BGP | QEMUで55.37秒。control journal追加後も同じBGP workerで復旧 |

[回帰試験Race log](2026-10-24-config-events/regression-race.log)、
[全体check/build](2026-10-24-config-events/make-check-build.log)、
[実daemon SIGKILL](2026-10-24-config-events/daemon-sigkill.log)、
[二WAN log](2026-10-24-config-events/health-pbr.log)、
[wan0 PCAP](2026-10-24-config-events/wan0.pcap)・[wan1 PCAP](2026-10-24-config-events/wan1.pcap)、
[provenance](2026-10-24-config-events/provenance.json)、
[SHA256SUMS](2026-10-24-config-events/SHA256SUMS) を保存した。
QEMUの各manifest/logも同directoryにある。

非rootユーザーをnamespace UID 0へ写した最初の実行は、ホストroot所有のancestorが
未割当UIDになるためprivate path検査で拒否された。検査を緩めていない。
rootful試験でもuser所有homeを通るELFはnamespace内から実行できないため、
root所有private `/tmp` へELFをコピーして試験した。試験用stageは終了時に削除した。
PCAP等の出力directoryも開始前に作成する。

## 残作業

これは設定lifecycleの最初のdurable producer接続である。
protocol状態のevent producer、EventRule/DerivedEvent、consumer作用のdaemon接続、
federation、retention/archive/capacity alarm、eventを含むQEMU電源断受入は未完了。
対象機種/NIC/性能・実機受入と全実装計画の継続ゴールも未完了。
CORE-04やparityの全体判定は上げない。

先行 `c2f3612` の全20 QEMU系統は [同一commit一括証跡](2026-10-24-qemu-aggregate.md) にある。
新コードの関連3 QEMU gateを、その旧一括runへ混ぜて全20再合格とは表現しない。
