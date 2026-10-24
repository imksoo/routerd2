# EventRule resourceの実control worker接続

2026-10-24 UTC。最終product checkpoint: `6185ba8`。
関連3 QEMU回帰は先行 `259380d`。CORE-03/04全体は未完了。

## 実装済み

ResourceProgramの `event_rule` payloadをpure compile・ownership・条件付きprojectionへ追加した。
inactiveな不正specも副作用前に拒否する。一つのsourceは最大128 rule。
control workerが、activation registry、consumer state/cursor/outbox、timer、履歴出力の公開を実行する。
新activationはjournal tipから開始し、過去の入力を初期集計へ再生しない。

無条件で定義不変のruleは、無関係なsource revisionやcontrol再起動でも集計を保持する。
loggingのReadyはその開始条件ではなく、明示したwhen/依存条件を使う。
削除・定義変更・再activationでは別consumerを使い、旧timer/jobを公開しない。
条件付きruleはcontrol incarnation、producer/期限と条件の連続性をfenceする。
全受理通知で変化を観測し、controllerの確認間隔内でfalse→trueが完結しても旧timerを継承しない。
registryに未観測のrevision gapがあれば、安全側に新activationを作る。

元の観測時刻からtimer期限を復元し、期限より後の未処理入力より先にtimerを評価する。
期限と同時の入力は優先する。入力/出力/timer batchは有限で、rule順をrotateする。
state/cursor/job保存と、journalへの安定ID付き出力・ACKを分け、ACK前のcrashでも公開recordを重複させない。
これは履歴publicationであり、protocol leaseや外部作用のexactly-once権限ではない。

`/v1/health` の `event_rules` にrevision、ruleのid/lifetime/cursor/version、
出力outboxのpending有無、bounded errorを公開した。失敗時はhealthを不健全にし、
正本の設定と未処理状態を保持して再試行する。
設定例と操作契約は [ResourceProgram](../resource-program.md#eventrule)、
[control API](../../internal/control/README.md#durable-eventrule-resources)、
[演算component](../../internal/eventrule/README.md) を参照。

## 検証済み

| 検証 | 結果 |
| --- | --- |
| compiler・projection | specの所有copy、inactive時の除外、不正specの事前拒否。PASS |
| 実SQLite controller | 新consumerのtip開始、集計・timer復元、定義変更、停止/再開、出力ACKの実SQL失敗後の同一ID再試行。PASS |
| timer/input順序 | 修正前は期限後expectedが成立済みabsenceを消すFAIL。修正後は期限前/一致/期限後で所定の出力。RaceでPASS |
| 条件flap | 修正前は投影間のfalse→trueで旧identityを保持するFAIL。全受理通知のcontinuityを反映後は別activation。RaceでPASS |
| 無関係workerのdegraded | 修正前は無条件ruleまで除外するFAIL。明示した条件によるgateへ修正後PASS |
| 全体check/build | 最終 `6185ba8` のproduct/test sourceと一致し、文書だけが未commitの状態で `make check build-linux` exit 0。unit/Python14件/Race/vet/static amd64・arm64。PASS |
| 文書のYAML例 | 実static CLIの `config plan` で検証。PASS |
| 実daemon namespace | 最終6185ba8のELFでsupervisor/control/network/logging実process、count/timer・SIGKILL・削除・confirmed restart rollback・履歴ID/cursorを検証。さらに実control DBのlifecycle保存失敗、commit済み不健全ACK/health、解除後の回復まで29.09秒PASS |
| 新QEMU EventRule gate | 最終6185ba8のfresh ELF。上記のcount/timer/再起動・rollbackと実SQLiteの保存失敗/不健全ACK/回復を実kernelで実行。59.42秒PASS |
| 関連QEMU回帰 | 先行259380dのmanagement 34.13秒、二WAN Health/PBR TCP/UDP/PCAP・復旧75.02秒、BFD/BGP 52.17秒。すべてPASS。最終変更は下記の不健全ACKの表示修正であり、これら3件を6185ba8で再実行したとはしない |
| 未観測revision gap | 集計と未処理入力を持つ状態でrevisionを飛ばし、新activationがtipから新しい入力だけを集計することを実SQLite/Raceで確認。PASS |

[全体check/build](2026-10-24-live-eventrule/check-build.log)・[ELF hash](2026-10-24-live-eventrule/build-manifest.json)、
[controller Race](2026-10-24-live-eventrule/controller-race.log)、
[条件等のRace](2026-10-24-live-eventrule/readiness-fix-race.log)、
[実daemon log](2026-10-24-live-eventrule/native.log)、
[新QEMU manifest](2026-10-24-live-eventrule/qemu-eventrule-manifest.json)・[log](2026-10-24-live-eventrule/qemu-eventrule.log)、
[回帰run manifest](2026-10-24-live-eventrule/regression-manifest.json)、
[provenance](2026-10-24-live-eventrule/provenance.json)、
[SHA256SUMS](2026-10-24-live-eventrule/SHA256SUMS) に保存した。

## 検出した失敗と扱い

最初の `f85f3c7` QEMUは、fixtureがsupervisor Readyだけを待ち、初回の公開healthが
503だった時点で失敗した。[失敗log](2026-10-24-live-eventrule/qemu-failed-before-fix.log) と
[manifest](2026-10-24-live-eventrule/qemu-failed-before-fix-manifest.json) を保持し、合格に含めない。
fixtureは503のhealth本文を読めるようにし、commit前に公開healthの収束も待つ。
この失敗logにはhealth本文がないため、503の具体的なreasonまで証明したとはしない。

独立したコード確認で、runtime全体のdegradedが無条件ruleを停止させる不具合も見つけた。
[ローカルの修正前FAIL](2026-10-24-live-eventrule/readiness-before-fix.log) で固定し、
現在sourceと明示的条件だけでactivationを判断する形へ修正した。
timerの試験は期限を8秒とし、kill前に未発火で入力が処理済みであることも必須にした。
confirmed trialはtimeoutではなくcontrol-restart rollbackを通ったことを確認する。

その後、lifecycle保存失敗を注入すると、healthは不健全だが小さいcommit応答だけは
`healthy:true` になる不整合を検出した。[unitの修正前FAIL](2026-10-24-live-eventrule/ack-before-fix.log) と
[259380dの実daemon負例](2026-10-24-live-eventrule/native-ack-before-fix.log) で再現した。
最終6185ba8では成功応答のrevisionを維持しつつhealthyをfalseにし、
[Race](2026-10-24-live-eventrule/ack-fixed-race.log)・上記native/QEMUで検証した。
QEMUでも実DBのUPDATE triggerを失敗させ、解除後にhealthが回復し、count出力が欠落・重複なく
3件に達することを必須にした。失敗したcommitをrollback扱いにして再送させる修正ではない。

## 未実装・未検証

live input producerは設定commit/confirm/rollbackまで。Health等のprotocolイベントproducer、
DerivedEvent resource/daemon/live typed status、外部作用の認証/idempotency、federationは未接続。
条件flapの受入はlocal runtimeの実通知処理までで、新たな実packet EventRule条件試験ではない。

既存の全20系統aggregateへ今回の別runを混ぜて全件再合格とはしない。
`make qemu-eventrule` を追加し、今後の `qemu-protocols` に含めた。
128 ruleや大規模correlationの性能・遅延、retention/archive/容量alarm、
各work package全体、実機/NIC/性能profileの最終受入は残る。parity判定は上げない。
