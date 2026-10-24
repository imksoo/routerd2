# 完了outboxの枠回収と重複抑止の保持

2026-10-24 UTC。product `0b4bb28`、最終fixture `b197c17`。
CORE-04の部分実装。全work package・長期運用・対象実機受入の完了ではない。parityは変更しない。

## 解決した問題と保存契約

従来は `pending=0` でも全ての完了jobが元payloadを保持し、待機jobと同じ100,000件/64MiB上限を消費した。
新しいcontrolは約1秒ごとに、consumer/producerそれぞれ32件以下、合計64件以下の完了jobを回収する。
`done` indexからbounded batchを読み、設定排他を保持せずに処理する。規模/処理rateの受入は別途残る。

同じWAL/FULL transactionで、kind/name/key・元event cursorまたは初回state version・正規化payloadの
SHA256を `completed_jobs` へ保存し、元outbox payloadを削除して両容量counterを更新する。
待機中job、consumer cursor/state/version、producer state/version、event履歴、現在factは変更しない。
同一内容の再送/完了ACKは成功し、内容変更・consumer元cursor変更を拒否する。
cursorだけを巻き戻しても既存作用を再実行しない。作用成功から完了ACKまでのcrashには引き続き作用先の冪等性が必要。

小さな記録も有限である。既定100万件/256MiBの独立予算を設け、`show event-store` とhealthに
`tombstones` を表示する。byte数はUTF-8 name/key長+41byteのencoded identity予算で、物理DB sizeとは別である。
80/100% warning/exhaustedを表示し、満杯なら元payloadを保持する。予算に収まるprefixだけを確定できる。
記録を期限で忘れたり、同じkeyを新規作用へ戻したりしない。SHAは内容照合であって署名ではなく、元payloadの復元機能でもない。

保存失敗ではbatchをrollbackし、`storage.compaction_error` / `last_error` とhealth/confirm拒否へ反映する。
read-onlyのfresh inspection成功ではwrite失敗の警告を消さず、次のmaintenance成功で解除する。
これは同一filesystem外へ永続化したalertの保証ではない。

解放したSQLite pageは後続jobに再利用する。DBファイルを縮小したり、filesystemへ即座に空きを返したりしない。
履歴exportのformat1はそのままで、event本体を削除しない。
内部DBはschema5。D-09に従って未公開1/2/3/4をread-only拒否し、旧prototype移行readerは作らない。
[運用契約](../event-storage.md) を参照。

## test-firstとローカル検証

先に枠回収・再送/ACK・内容/元cursorの衝突・cursor巻き戻し・失敗atomicity・容量境界・
並行完了/回収とSIGKILLのtestをcommitし、API未実装FAILを保存した。
実daemonの新fixtureもSHA照合済み旧6006090 ELFで実行すると、自動回収がなく12秒timeout FAILとなった。
[旧ELF](2026-10-24-job-compaction/before-binaries.json) と
[旧daemon FAIL](2026-10-24-job-compaction/native-old-red-excerpt.log) を参照。

[関連Race](2026-10-24-job-compaction/local-race.log) はevents10.466秒/control47.226秒PASS。
insert/delete/counter保存失敗、実SQLite page制限によるSQLITE_FULL、別予算満杯で元payloadを保持し、
解除後の再試行/reopenでstate/version/usage/dedupが一致する。consumer/producer双方の進捗とUTF-8 byte集計も検証した。
実subprocessを完了回収後にSIGKILLし、CloseなしのWAL復旧後に重複作用が戻らないことを確認した。

大きな完了payloadでは24 SQLite pageを解放し、新しい待機jobが16 pageをDB拡大なしで再利用した。
[page再利用Race](2026-10-24-job-compaction/page-reuse-race.log) は1.435秒PASS。
単にcounterを減らしたことを、物理page再利用の証拠に読み替えていない。

clean product0b4bb28の全体check/Race/vet/static amd64/arm64 buildは95.91秒PASS。
その後、追加page test・fixtureのJSON読取修正・三boot比較拡張を含むclean b197c17の
`make check build-linux RELEASE_ID=0b4bb2885a73` は **22.47秒PASS**。
Python全22、Go Race/vetと両arch buildを含み、cache利用はlogのまま区別する。
[最終check](2026-10-24-job-compaction/check-build.log) と
[manifest](2026-10-24-job-compaction/build-manifest.json) に開始/終了commit・dirty=false・SHAを記録した。
release identityをproductへ固定し、nativeの二つのELFのSHAが最終checked pairとも一致することを確認した。

初回の全体checkは実行側が `umask=0002` を引き継ぎ、private ancestorの拒否で13.51秒FAILした。
同じproductのまま0022へ修正して上記PASSとなった。安全なpath検査を緩めず、home/repo権限も変更していない。
[失敗抜粋](2026-10-24-job-compaction/umask-failure-excerpt.log) と
[失敗manifest](2026-10-24-job-compaction/umask-failure-manifest.json) を保存する。

## 実daemon・QEMU

root所有0700 staging内のfresh ELFとRace付きintegration harnessを使い、隔離namespaceで検証した。
新fixtureは実Health TCP probe、DerivedEvent、EventRuleから完了jobを生成する。
元payloadとcompact記録の重複がないこと、両名前空間のSHA/元positionを確認した。
実SQLite triggerの回収失敗でhealth503、fresh inspection後もerror、解除後のhealthy回復、
実control SIGKILL後も元key/position/hashが一致することを検証した。

| gate | native | QEMU 実kernel boot |
| --- | --- | --- |
| archive + 自動回収/失敗/復旧/control SIGKILL | 4.70秒PASS | 35.54秒PASS |
| live Derived・raw履歴・worker/queue回復 | 11.93秒PASS | 121.51秒PASS |
| 二WAN Health→PBR TCP/UDP/PCAP・全復旧sequence | 29.85秒PASS | 96.05秒PASS |

[修正後archive](2026-10-24-job-compaction/native-archive-fixed.log)、
[個別に通過したDerived/PBR抜粋](2026-10-24-job-compaction/native-related-excerpt.log)、各QEMU manifest/serialを保存する。
QEMUは512MiB/2vCPU/外部NICなし、64MiB scratchで、二WANはguest内部に独立namespaceを作る。
関連3gateの合格であり、全QEMU suiteの新aggregate合格や性能受入とはしない。

初回native aggregateは、新fixtureの復旧待ちだけが失敗した。
省略されたoptional JSON errorを、再利用したGo構造体の旧値として残したことが原因だった。
実controlのrecovered logを保存し、読取先を毎回初期化するtest-only修正e18871d後、archive単体が通過した。
初回aggregateを合格と数えず、そこに含まれるDerived/PBRの個別PASSと分ける。
[fixture失敗抜粋](2026-10-24-job-compaction/native-fixture-failure-excerpt.log) を参照。

private mount namespace内の実8MiB tmpfsでも、warningをmandatoryに要求して **4.41秒PASS**。
容量warning・producer/controlのhealthy継続、自動回収の障害と復旧、control SIGKILLを含む。
[quota log](2026-10-24-job-compaction/native-warning.log) を保存した。
最初のquota起動はtest flagの綴り誤りでfixture開始前に終了し、修正後の試験と区別してprivate logに保持した。

## 二度のVM電源断でも記録を比較する

専用三boot gateを拡張し、最初のcut前には実Derived producer、二度目のcut前には
実producerとEventRule consumer双方の完了回収を要求した。fixtureが完了jobを注入したり、
製品の代わりに回収APIを実行したりしない。readonly queryでkind/name/key・元position・SHAを取り、
次bootのguestと独立したhost verifierの双方が、過去の全記録が同じ内容で残ることを比較する。
新たな完了だけを許し、欠落/変更/重複/position変更を拒否する。Go/Pythonのnegative testを先にFAILさせた。

同じ256MiB ext4 disk、実製品PID1/supervisor/control/network/Health、1GiB/2vCPU/外部NICなしで
**75.27秒PASS**。produce25.15秒とregistry27.03秒は生存中のQEMUをhost SIGKILL/-9、
recovered23.09秒は正常poweroff/0。三つのkernel boot IDは全て異なる。

| boot | 受理済みraw履歴 | 完了回収記録 | 結果 |
| --- | --- | --- | --- |
| produce | 2件がlocal queue、target未保存 | producer1件 | UART witness後にcut |
| registry | 元2件+追加2件、cursor4/5/6/7 | consumer1+producer2件、元1件を保持 | target保存/local ACK済み、登録ACK失敗中にcut |
| recovered | 同じ4件・内容・cursor | 同じ3件・全key/position/SHA | 登録失敗を保持し、解除後健全/登録0へ |

現在Health/Derivedは復活せず、旧history/current通知は409で拒否され、保持EventRuleが履歴を消費した。
[manifest](2026-10-24-job-compaction/power-manifest.json)、
[produce](2026-10-24-job-compaction/power-produce.log)・
[registry](2026-10-24-job-compaction/power-registry.log)・
[recovered](2026-10-24-job-compaction/power-recovered.log) を保存する。

期待値はUART/次回test initramfsで運び、cut前にguest witness diskの書込・fsync/checkpointを行わない。
後続bootはhelperが先にmount/recover/repairせず、製品が同じdiskをmountする。
factory command pairのgateであり、署名runtime lifecycleの受入とは区別する。
これはguest VMのprocess/device memoryの喪失試験で、host/物理storage cacheの電源断や全cut点の網羅ではない。

## 残作業

完了jobのhot枠回収は実装したが、event本体のarchive退避/回収、dedup記録の長期保持/世代終了とretention policy、
総filesystem予算、外部永続alert、規模/rate/長時間運用の受入は残る。
他raw producer/status provider、構造化field/Derived連鎖、作用/idempotency・federation、
製品版間migration/full DB rollbackと署名runtime lifecycle、残work package、対象実機/NIC/性能の受入も継続する。

読んだ実装: `internal/events/{types,store_linux,journal,outbox,producer,checkpoint,capacity_linux,compaction,history}.go` と各test、
`internal/control/{event_store,server,control}.go` と各test、`internal/integration/{event_archive,derived,health_pbr,bgp}_test.go`、
`images/health-power-test/guest/{main,probe,history,completed}.go` と各test、
`scripts/{qemu-health-power,qemu-network}.py`・`scripts/qemu_health_power_test.py`、Makefileと適用 `../AGENTS.md`。

原logのSHA/抽出/正規化は [provenance](2026-10-24-job-compaction/provenance.json)、
公開power witnessでは、認証鍵ではない出力job識別子のfield名だけを `key` から `work_id` に変更した。
値は全て保持し、原logのSHAと変換をprovenanceへ記録する。
公開版のSHAは [SHA256SUMS](2026-10-24-job-compaction/SHA256SUMS) を参照。
ELF/initramfs/disk・host memory/filesystem診断・private fixture console詳細はcommitしない。
