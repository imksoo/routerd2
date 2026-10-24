# Stateful consumer・EventRule演算・二回起動のイベント保持

2026-10-24 UTC。最終code checkpoint: `12f9be6`。
storage実行checkpointは `f4f6140`、stateful保存部は `03cac87`。

## 実装済み

`events.Checkpoint` / `Transition` で、集計状態・consumer cursor・outbox jobを
同じSQLite transactionへ保存する。timerはcursorを進めなくてもversion一致を必要とする。
stateful consumerをstateless APIへ切り替えて状態保存を迂回することはできない。
状態は処理済みcursorにも結び付け、cursorだけを戻して新しい集計状態へ古い入力を数え直すのを拒否する。
完全なDB rollbackや外部作用の再送には、引き続き作用先のdurable idempotencyが必要である。

stateはconsumerごと64 KiB、合計既定64 MiB。容量不足・SQL失敗で状態、cursor、jobを部分確定しない。
DB schemaは3。accepted D-09に従い、未公開prototypeの1/2はread-only probeで拒否する。
製品版間のmigration/fallback互換を保証したものではない。

`internal/eventrule` に、all_of / any_of / sequence / window / absence / throttle /
debounce / countのpure evaluatorを追加した。
入力・source digest・fire counterに結び付けた出力ID、永続化可能なtimer期限、
64件の出力batch、容量制限、source変更時のtimer破棄を実装した。
templateは一度だけ展開し、入力値を別の式として再評価しない。

これは**演算componentまで**。ResourceProgramのEventRule resource、daemon consumer、
出力publication、readiness/status、作用実行へは未接続。
観測時刻に基づくtimer、one-pass template、64 KiB state予算はnativeの明示した方針であり、
全legacy fieldや大規模correlationのparity合格ではない。
[componentの契約・残作業](../../internal/eventrule/README.md) を参照。

## 検証済み

| 検証 | 結果 |
| --- | --- |
| state/cursor/job transaction、並行timerのversion CAS | PASS。16競合から1件だけ確定 |
| 実SQLite FULL、状態保存trigger失敗、再開後のusage・writer | PASS。SQLite error 13と全体rollbackを確認 |
| 実child SIGKILL・WAL復元 | stateful状態・version・jobも復元。PASS |
| cursorだけの巻き戻し | 修正前は新しい集計状態を再利用するFAIL、修正後は拒否。PASS |
| 固定routerdとの直接比較 | 即時演算の出力内容を直接比較。空のresource名とcorrelate path前後空白の差を検出・修正。PASS |
| EventRule各演算・timer | 境界、再送ID、spec変更、100 timerの64+36件batch、容量不足時の部分結果なし。PASS |
| 演算＋実保存部 | countの閾値到達後、outbox挿入後のcheckpoint書込みを実SQLite triggerで失敗。再送ID/内容不変、再計数なし、reopen後job保持。PASS |
| 最終check/build | clean `12f9be6` のunit/Python/Race Detector/vet/static amd64・arm64。PASS |
| schema 3の実SSH/control再起動 | 固定 `03cac87` QEMUで37.97秒。PASS |
| 設定イベントを含むext4二回起動・署名fallback | 修正後の固定 `f4f6140` で124.67秒。設定/SSH/sentinelとevent ID/cursor一致。PASS |

[全体check/build](2026-10-24-event-transitions/make-check-build.log)、
[stateful Race log](2026-10-24-event-transitions/stateful-race.log)、
[rule演算・reference比較](2026-10-24-event-transitions/rule-race.log)、
[演算と実checkpoint](2026-10-24-event-transitions/rule-checkpoint.log)、
[storage manifest](2026-10-24-event-transitions/storage-manifest.json)、
[実行log](2026-10-24-event-transitions/storage.log)、
[provenance](2026-10-24-event-transitions/provenance.json)、
[SHA256SUMS](2026-10-24-event-transitions/SHA256SUMS) に保存した。

## Storage gateで検出したACKとの競合

`03cac87` のstorage再実行はconfig.json全体のhash不一致でFAILした。
設定はrevision 2のままで、初回witnessがproducer outboxのACK前にhashを取得していた。
その後のACK保存は同じファイルのmetadataを変更する。
失敗diskをdebugfsでread-only抽出し、journalのeventから未ACKレコードを復元すると
初回hashと完全一致した。[実diskでの原因確認](2026-10-24-event-transitions/ack-cause-proof.json) を参照。
設定意図が変わらずmetadataだけが変わることも独立したローカル試験で固定した。

probeはpublicationとproducer ACKの両方を待ってからhashを採るよう修正した。
**元のファイル全体のhash比較は保持**し、二回のevent ID/cursor一致も必須にした。
修正前runを合格に含めず、上表は修正後の別runである。

終了時もfilesystemをmountしたまま電源断する試験なので、保存diskのオフライン検査では
private copyだけに `e2fsck -E journal_only` を実行してからdebugfs/read-only SQLiteで調べた。
原diskとhost mountには触れていない。schema 3・event 1件・両起動と同じID/cursorを確認した。
[copy上の検査](2026-10-24-event-transitions/disk-copy-proof.json) を参照。
これは設定eventの二kernel起動保持であり、daemonへ未接続のrule checkpointをQEMU電源断で
検証したことにはしない。rule checkpointはhost上の実SIGKILL/WAL試験までである。

CORE-04全体、DerivedEvent、protocol event producer、federation、retention/archive/容量alarm、
privileged consumer、全実装計画、対象実機/NIC/性能受入は引き続き未完了。
