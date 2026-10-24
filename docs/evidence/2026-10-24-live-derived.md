# HealthCheckからlive DerivedEventへの接続

2026-10-24 UTC。product/test checkpointは `1ba902025d2f14a42eb5378efa57fa2322725855`。
CORE-03 / CORE-04の部分checkpointであり、作業単位全体や対象実機の合格ではない。

ResourceProgramの `derived_event` を実control workerへ接続した。
HealthCheckの有限なtyped current statusを評価し、producer stateと出力jobを同時保存してから
安定IDでjournalへ公開する。その履歴を実EventRuleが集計できる。
`show derived RESOURCE_ID` は履歴ではなくread-onlyの現在値APIを読む。

## 実装と境界

- HealthCheckの宣言済みscalar fieldのみ対応。参照はservice名ではなくresource IDで、
  `service/wan` のようなIDも使える。inactive provider、未観測、期限切れはunknown。
  全sourceを検証し、inactiveな参照先が投影から消えても設定エラーにしない。
- 現在のboot、supervisor、control incarnation、source/revisionとcutover nonce、worker generation、
  producer epoch/sequenceを検証する。元の観測時刻と同じsampleの期限を保持する。
  条件失効はworker停止完了を待たず現在値に反映する。A→B→Aでも古いscopeを再利用しない。
- BOOTTIME hysteresisと、受理した通知ごとのpredicate truth/authority continuityを使う。
  同じtruthの連続更新はholdを保ち、投影間flap・unknown・lease gap・authority変更は数え直す。
  評価自体は現在値のsampled reconciliationであり、全raw protocol遷移のlossless journalではない。
- durable lifecycle、producer state/outbox、publication retryを接続した。
  同一定義の無条件resourceはcontrol再起動で初回出力を重複させず、現在値は必ず取得し直す。
  定義変更・削除・条件失効は旧activationのjobをfenceする。
- 現在値にはinput identityと最大5秒のevaluator leaseを要求し、inputと自身のactivation期限でも制限する。
  GETはlease更新やDB書込を行わない。unknown/inactiveではassertedを省略し、保存/評価失敗は503。
  `health.derived` はrevision/count/pending/errorのbounded metadataで、エラーはtrial確認も防ぐ。
- 同じauthorityの通常のretraction hysteresisは保つ。新worker/control/sourceでは、保存済みUpを
  retraction待ちのUpとして表示せず、新しい観測で現在値が確定するまでbooleanを省略する。
  historical last-transition timestampは現在の権限ではない。

最大128宣言、全体1,024参照。200ms loopで1回16宣言・8出力まで処理する。
無宣言なら未使用componentのregistry IOを行わず、既存EventRuleの保存失敗契約も保つ。
この上限は規模・遅延の性能保証ではない。

## 検証結果

| 経路 | 結果と範囲 |
| --- | --- |
| 全体check/build | clean `1ba9020` の `make check build-linux` exit 0。unit、Python14件、Race Detector、vet、静的amd64/arm64。PASS |
| 関連Race | compiler/runtime/control/controller、純粋evaluator、CLI。元scope復元・inactive provider・新authorityへの旧Up継承の修正後PASS |
| 実daemon / 管理 | root所有0700 stageにfresh ELFとRace integration harnessを置き、隔離namespaceで実行。既存commit/confirm/rollback履歴と実SSH `show derived derived/literal`。8.21秒PASS |
| 実daemon / Derived | 実TCP probe→HealthCheck→DerivedEvent→EventRule、worker/control/logging再起動、旧通知409、同じReady health PIDのmax_age、実SQLite失敗と回復。7.25秒PASS |
| QEMU / Derived | 外部NICなしのfresh guestで同じ実daemon試験。37.17秒PASS |
| QEMU / 管理・実SSH | 現在値表示とcontrol再起動、既存設定/履歴管理。31.72秒PASS |
| QEMU / 既存回帰 | EventRule56.57秒、独立二WAN Health/PBR70.80秒、BFD50.96秒。すべて同じproduct/test sourceからfresh buildして順次実行、PASS |
| 文書example | built CLIによる `config validate`。PASS |

[全体check/build](2026-10-24-live-derived/check-build.log)・[静的ELF hash](2026-10-24-live-derived/build-manifest.json)、
[実daemon](2026-10-24-live-derived/native.log)・[native provenance](2026-10-24-live-derived/native-manifest.json)、
[Derived QEMU](2026-10-24-live-derived/qemu-derived.log)・[manifest](2026-10-24-live-derived/qemu-derived-manifest.json)、
[管理QEMU](2026-10-24-live-derived/qemu-management.log)・[manifest](2026-10-24-live-derived/qemu-management-manifest.json)、
[関連回帰manifest](2026-10-24-live-derived/regression-manifest.json) に結果を保存した。
回帰logは [EventRule](2026-10-24-live-derived/qemu-eventrule.log)、
[二WAN TCP/UDP・packet capture・復旧](2026-10-24-live-derived/qemu-health-pbr.log)、
[BFD/BGP](2026-10-24-live-derived/qemu-bfd.log)。各ELF/kernel hashは個別manifestに保持する。
QEMUは専用ELFをfresh buildし、native buildとはhashを分ける。QEMU実行中の未commit変更は文書だけで、
product/test sourceは上記commitと一致する。全22系統をこのcommitで一括再実行した主張はしない。

Derived実daemon試験では、TCP listener停止/再開を2回通してEventRule count=2を確認した。
healthをSIGSTOPし、supervisorでは同じPID/generationがReady、producerの2分leaseが残る状態で、
consumerの1秒max_ageが失効して条件付きDerivedEventがinactiveになることを確認した。
別sequenceの元観測時刻を混ぜず、同じsampleとBOOTTIMEを比較する。
health再起動後に旧通知を送って409、control再起動後に初回eventが重複しないことも確認した。

実SQLiteにproducer更新を拒否するtriggerを入れ、DB失敗中はhealth/current viewが503となり、
部分的なretractionを履歴へ公開しないことを確認した。trigger解除後は未処理状態から回復し、
期待した3回目のretractionを一度だけ公開した。CLIも503で部分出力や古いUpを表示しない。
これはQEMUの電源断や全filesystem rollbackの代用ではない。

## 検出した失敗

API未実装のcompile failure、CLI未実装のfailureを先に固定した。
source A→B→Aがscopeを再利用する失敗と、inactiveになってもまだ動くworkerのUpが漏れる失敗を
[source/provider修正前](2026-10-24-live-derived/source-provider-red.log) に保存した。
cutover nonceと現在のprovider lifecycle確認で修正し、[関連Race](2026-10-24-live-derived/source-fixed-race.log) がPASS。

新producer/controlがDownを報告していても、過去のUpがretraction graceとして表示された。
[修正前FAIL](2026-10-24-live-derived/authority-red.log) を先行test commit `6a1996a` で固定し、
`1ba9020` で最後のassertionを裏付けたauthorityを別保存するよう修正した。
[同producerのgrace保持・replacementのunknown・安定Down完了のRace](2026-10-24-live-derived/authority-fixed-race.log) と
上記全体/実daemon/QEMU試験で確認した。

初回接続時に、Derived未宣言でもregistry IOが走って既存EventRule fault testを壊した失敗も
[初回関連Race](2026-10-24-live-derived/wiring-first-race.log) に保存した。
無宣言時のlazy initializationで修正し、既存testを弱めず
[回帰修正Race](2026-10-24-live-derived/control-regression-fixed.log) を通した。
これらのFAILは合格結果に含めない。

公開logはworkspace/private stage path、Go pointer、公開kernel certificate identifierを正規化した。
原log SHAとコマンドは[provenance](2026-10-24-live-derived/provenance.json)、公開版は
[SHA256SUMS](2026-10-24-live-derived/SHA256SUMS) で区別する。秘密鍵や実環境一覧は含めない。

## 残作業・未検証

他status provider Kind、構造化field、DerivedEvent連鎖、全status reference契約は未対応。
raw protocol event producer、全条件の実packet受入、新live producerのQEMU abrupt power-loss/reboot、
作用の権限/idempotency、federation、retention/archive/容量alarmと規模/遅延は残る。
残りの全work packageと多binary runtime/startup handoff等の全受入も継続する。
対象機種・NIC・性能条件は未確定で、実機利用可能とは判定しない。parityは変更しない。

読んだ実装・契約は `internal/derived/{engine,snapshot,reference}.go`、
`internal/derivedcontroller/controller.go`、`internal/runtimeplan/derived_inputs.go`、
`internal/resourceprogram/`、`internal/control/derived.go`、`internal/cli/`、
`internal/protocol/health/`、`internal/integration/{derived,daemon}_test.go`、
`scripts/qemu-network.py`、`images/kernel-test-init/main.go`、固定routerdの
`pkg/{api,derived,healthcheck}` とchainの起動経路。ルールは `/home/imksoo/AGENTS.md` を確認した。
