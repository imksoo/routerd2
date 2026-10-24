# 条件停止中のHealth履歴回収

2026-10-24 UTC。product checkpoint `f5e9cc5`。CORE-03 / CORE-04 / NET-01の部分checkpoint。
全計画・実機利用可能性の受入は未完了。

## 実装と所有権

従来の永久削除回収に加え、宣言を残したまま `when` により停止したHealth workerのqueueも回収する。
登録済みの生成pathだけを対象とし、filesystem走査・未登録stateの採用・削除をしない。
fresh supervisorに当該health processがなく、runtimeにも選択済みhealth bindingがないことが必要。
queueの `OpenExisting` による排他lockは独立して必須で、missing/foreign stateを空で置換しない。

回収callback中はruntimeのmodule reconcile排他を保持し、条件が変わってもworkerを再生成させない。
現在factを受け付けるruntime mutexはdisk IO中に保持しない。
既存の1枠のreconcile排他をcontext付きsemaphoreへ変更した。回収は期限内で待ち、
Monitorの非待機reconcileは待機者を追い越さない。期限切れを回収失敗として表示し、黙って成功扱いにしない。
ApplyConfigもcontext期限付きで取得する。新しい依存packageは追加していない。

一pass最大8登録を巡回し、各queueは最大16件の一pageを処理する。target保存→local ACKの順を維持し、
宣言済みqueueは空でも登録を残す。永久削除queueだけが空を確認した後に登録解除される。
`health.protocol_events.stopped_pending_relays` は検出済みの宣言済み・停止中・回収待ち件数を返す。
複数pageの途中も不健全のままで、排他的に空を確認したqueueだけが元workerの配送errorを解除する。
他producerのerrorを成功で消さない。既存の不健全/trial確定不可の経路を使う。

履歴の元ID・観測時刻・内容を保持する。設定/revisionを変更せず、停止instanceは404、
HealthCheckを参照するDerived現在値はunknownのまま。現在のUpへの復帰には再有効化した新workerの観測が必要。

## test-firstと実配送の新旧比較

最初のunit testは、宣言済みqueueでtargetの部分容量失敗を起こすべきなのに何も配送せずnilで戻ることをFAILにした。
固定した小さな実daemon harnessでは、旧 `238bc58` が未配信IDの配送待ち10秒でFAILした。
ELFは前回clean build manifestのSHAと一致することを確認した。
同じharnessを新 `df8e93a` に対して実行すると2.65秒PASS、最終 `f5e9cc5` も2.72秒PASSした。
後続の最終拡張harnessとはhash/checkpointを分け、同じ試験を使った比較と追加受入を混同しない。

最終拡張試験は独立したloopback TCP endpoint二つを使う。switchの失敗でwanを停止するが、
sourceやrevisionは編集しない。実SQLite triggerがwan履歴だけを拒否している間に、queueから
target未保存の元eventを読み取る。第一cycleではcontrolをSIGKILLし、新controlが同じ宣言を復元する。
target回復後、wanを再有効化する前に元eventが一度ずつ完全一致で配信されることを確認する。
配送error解除、登録保持、現在instance 404、Derived unknown、旧現在値POSTの409も確認する。
その後にendpointを復旧し、新producer epochの観測でDerivedがUpへ戻る。第二cycleも停止/回収/再有効化を実行する。

## 検出した失敗と修正

最初のnative拡張試験は、二つのproducerとDerivedの通常書き込みにtest用trigger DDLが競合し、
`SQLITE_BUSY` で注入前に失敗した。注入接続一つだけに2秒のbusy timeoutを設定した。
製品のSQLite設定や再試行を変更した結果ではない。

最初の条件停止QEMUは37.48秒FAIL。workerの停止とcontrol再生成は完了したが、
回収待ち件数の5秒観測でtimeoutした。回収エラーログも出ていなかった。
`TryLock` で毎回回収を延期してnilを返す経路を疑い、ローカルで競合を固定した二つのtestを追加した。
旧実装は、ロック所有中でも即nilで戻り、解除後の観測を行わないため両方FAILした。
context期限付き取得に変更し、解除後の回収、期限切れ、fact mutex非保持をRaceで確認した。
この中間product675ab37の新QEMUは56.81秒PASSしたが、既存Derived QEMUは51.55秒FAILした。
controlのsource lockを持って所有権を待つため、通常の履歴POSTが1秒deadlineを繰り返し超えた。
ローカルの実Unix supervisor照会を止めるテストで、待機中のcontrol排他を取れないFAILを再現した。
後続04a4d12は待機中にsource lockを解放し、所有権取得後はsource lockをTryLockする。
設定適用はsource→runtimeの順なので、sourceがbusyなら今回の回収を見送り、逆順で待たない。
両方を持った後にregistryと正本を再読し、以前の所有観測にない名前の配送errorを消さない。
sourceが空いている場合/使用中の場合と、未知の所有観測の回帰testをRaceで確認した。
04a4d12のDerived QEMUは先の箇所を通過したが、SIGCONT後の1秒鮮度の回復待ちで63.54秒FAILした。
全候補が現在のhealth bindingに選択されていても回収排他を取り、reconcileと競合する経路が残っていた。
ローカルで全worker選択済み・reconcile排他使用中・supervisor照会不可を固定したtestを追加し、
何も回収できないのにcontext期限切れとなるFAILを示した。f5e9cc5では全候補が選択済みなら明示falseだけを
callbackへ返し、回収排他とsupervisor IOを使わない。停止候補がある場合の期限付き取得は維持する。
この経路は許可を一つも与えないため、workerがその直後に停止しても回収を次回へ延期するだけで競合しない。
QEMUだけで繰り返し待機時間を伸ばす修正は行っていない。

slow guestでもTCP listen backlogが満杯にならないよう、前提endpointもaccept/closeを続けるtest-only修正を行った。
VM RAM512MiB、vCPU2、test scratch64MiB、製品の観測鮮度・容量・durabilityは維持した。

## 検証結果

| 最終f5e9cc5の経路 | 結果 |
| --- | --- |
| 全体check/build | unit/Python14/Race/vet/static amd64・arm64。100.37秒PASS |
| 実SQLite/所有競合Race | 部分容量失敗、元event/登録保持、17件のpage間pending、busy/deadline、稼働workerの競合なしの拒否、source lock解放/競合、未観測のerror保持。PASS |
| 実daemon | 条件停止2cycle/control SIGKILL/新epoch復帰4.41秒、永久削除1.58秒、Derived11.88秒、二WAN Health/PBR29.89秒。PASS |
| 固定harness新旧比較 | 旧238bc58は配送10秒timeout FAIL、最終f5e9cc5は2.72秒PASS |
| 既存Derived QEMU | worker/control再起動、鮮度失効と復旧、DB失敗、永久削除/registry ACK。97.76秒PASS |
| 二WAN Health/PBR QEMU | 実TCP/UDP・PCAP、切替/復旧、旧通知、期限切れ、confirmed rollback。91.65秒PASS |
| 条件停止QEMU | 設定保持、control SIGKILL、元内容/IDの回収、unknown維持、2回の新epoch復帰。48.37秒PASS |

最終ELF hashはnativeと全体checkのamd64で一致する。check中にdirtyだったのは文書だけで、
開始と完了のproduct commitは同じ。QEMUは別identityのfresh ELFとharnessを生成し、hashを別に保存する。
関連QEMU 3系統の結果であり、全suiteの新aggregate合格とはしない。
初期 `df8e93a` のcheck/build94.04秒・native条件停止4.62秒は途中の結果であり、最終f5e9cc5の結果とは区別する。
初期check開始後に変わったTCP fixture一つと、ELFに埋め込んだrelease identityもmanifestに記録した。

## 残作業

今回の回収はHealthの登録済みqueueとプロセス所有権の範囲であり、他raw protocol producerや
一般status provider/構造化field/Derived連鎖は継続する。新queueのQEMU強制電源断・再boot、
retention/archive/容量alarm・filesystem全体の予算・規模/遅延、製品版間migration/完全DB rollback、
権限付き作用/idempotency・federation、全work packageと対象実機/NIC/性能受入も残る。
作業単位全体やparityを完了扱いにせず、継続ゴールを維持する。

読んだ実装: `internal/{protocolevents,runtimeplan,control,events,serviceconfig,supervisor}/`、
`internal/integration/{health_retirement,derived,health_pbr,bgp}_test.go`、
`images/kernel-test-init/main.go`、`scripts/qemu-network.py`、`Makefile`、適用 `../AGENTS.md`。

[全体check](2026-10-24-health-stopped/check-build.log)・[最終ELF hash](2026-10-24-health-stopped/build-manifest.json)、
[関連Race](2026-10-24-health-stopped/final-wired-race.log)・[実daemon](2026-10-24-health-stopped/native.log)、
[固定harness PASS](2026-10-24-health-stopped/counterfactual-pass.log)・[旧daemon FAIL](2026-10-24-health-stopped/counterfactual-red-excerpt.log)、
[QEMU summary](2026-10-24-health-stopped/qemu-summary.json) を参照。
原logのSHAと抽出/正規化は [provenance](2026-10-24-health-stopped/provenance.json)、
公開ファイルのSHAは [SHA256SUMS](2026-10-24-health-stopped/SHA256SUMS) に保存する。
