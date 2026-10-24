# Event本体の圧縮退避とhot枠回収

2026-10-24 UTC。最終product `3bca14b`、初回実装 `7210004`。
CORE-04の部分実装。外部archive退避・保持期限/ID世代終了・全計画/対象実機受入は未完了。parityは変更しない。

## 保存と読取の契約

従来は全eventが元payloadのまま100,000件/64MiBの枠を消費した。
controlが約1秒周期に最大16件を、同じSQLite WAL/FULL内の `event_archive` へ移す経路を追加した。
元のevent row/ID/cursorを残してbodyを解放するため、未読consumer、pending outboxのforeign key、
timerの元cursor、consumer/producer state/versionを維持する。履歴をskip/ACKしたり、現在Upやauthorityを復元したりしない。

gzip生成とround trip/SHA検証はDB接続/書込transactionの外で行う。
確定時に元bodyを再確認し、圧縮body/元size/SHAの保存・元payloadの解放・両usage counter更新を同じtransactionで確定する。
並行する退避が既に同じbodyを移した場合は再検証して見送る。違うbodyや保存失敗は確定しない。
予算に収まるprefixだけを確定でき、1件も収まらない場合は元payloadを保持してerrorとなる。

List・再送ACK・history exportはhot/圧縮先の両方を読む。同一IDの同一内容は同じcursorでACKし、内容変更を拒否する。
圧縮bodyの欠損、CRC/長さ/SHA不一致、追加stream/trailing byte、展開量超過は読取と再送ACKを拒否する。
export format1の固定tip・全内容・元cursorと独立verifyの契約は維持した。

容量表示は `events` がhot、`archived_events` が圧縮、`history` が全履歴である。
圧縮領域は既定100万件/256MiB（gzip長+48byte/件）、全履歴は100万件/元byte1GiBの上限を持つ。
後者はformat1の検証上限でもあり、退避だけで元の全履歴chargeを解除しない。first/tip/cursorは論理履歴上の位置を保つ。
各予算の80/100%をwarning/exhaustedへ出し、write失敗は `archive_error` / `last_error`、不健全/confirm拒否へ反映する。
read-only inspectionが成功しただけではwrite失敗の警告を消さず、次のmaintenance成功で解除する。

解放したSQLite pageは新eventへ再利用する。ファイルのtruncate/VACUUM、別媒体へのbackup、保持期限による削除ではない。
内部DBはschema6。D-09に従い未公開1/2/3/4/5はread-onlyで拒否し、prototype移行readerは追加しない。
[操作と保存契約](../event-storage.md) を参照。

## test-firstとローカル検証

先に未読consumer・timer/pending参照、再送/内容衝突、全履歴上限、並行Append/退避/export、
insert/body解放/counter保存失敗、実SQLite FULL、reopen/SIGKILLのtestをcommitし、未実装FAILを保存した。
新actual-daemon fixtureはSHA照合済み旧0b4bb28 ELFで10.40秒timeout FAILとなった。
[旧ELF](2026-10-24-event-archive-tier/before-binaries.json) と
[旧daemon FAIL](2026-10-24-event-archive-tier/native-old-red-excerpt.log) を参照。

[初回関連Race](2026-10-24-event-archive-tier/initial-local-race.log) はevents17.408秒、control47.686秒、
local CLI1.739秒、EventRule controller3.432秒、Derived controller1.605秒PASS。
最初のbackground unit fixtureはHealth ID helperに長い文字列を渡して上限を超えたため失敗した。
入力を1文字へ修正し、安全なenvelope制限は維持した。[失敗log](2026-10-24-event-archive-tier/fixture-id-failure.log) を残す。

大きなeventを圧縮して7 SQLite pageを解放し、新eventが2 pageをDB拡大なしで再利用した。
[page再利用Race](2026-10-24-event-archive-tier/page-reuse-race.log) は1.370秒PASS。
再送元body、counterと元cursorの一致、未読/遅いconsumer、timerとpending jobの参照保持、
圧縮/未圧縮が混在するexport、出力途中の退避とAppendも検証した。
破損試験はcacheをwarmした後にもgzip/hash/長さ/trailing/multistream/bombを変え、誤ったcache ACKを拒否する。

最終clean `3bca14b` の `make check build-linux RELEASE_ID=3bca14b5bd6b` は **106.40秒PASS**。
unit・Python23・Race Detector・vet・static amd64/arm64 buildを含む。
[check](2026-10-24-event-archive-tier/check-build.log) と
[manifest](2026-10-24-event-archive-tier/build-manifest.json) に開始/終了commit一致・dirty=falseとSHAを保存した。
初回7210004の同じ全体check106.88秒PASSも別に残す。その合格だけでQEMU回帰を合格扱いにはしない。

## QEMUで見つかった回帰と修正

初回QEMU Derivedは **53.11秒FAIL** した。real protocol履歴送信、EventRule、容量monitorがdeadlineに達し、
logging再起動後の二度目のassertion待ちが失敗した。512MiB/2vCPU、64MiB scratchの空きは十分だった。
[失敗manifest](2026-10-24-event-archive-tier/failed-derived-manifest.json) と
[serial](2026-10-24-event-archive-tier/failed-derived-serial.log) を保存する。

ローカルではwarmした同じ16件の読取でもDEFLATE windowを毎回作り、**736,221byte/page**を割り当てることを
[新testのFAIL](2026-10-24-event-archive-tier/warm-allocation-red.log) で再現した。
検証済みbodyを4MiB payload/4,096件以下のLRU cacheへ保存し、各hitでも現在のSQL圧縮byte・元size・SHAの一致を確認する。
同じfocused Race fixtureは **71,755byte/page** へ減り、[6.729秒PASS](2026-10-24-event-archive-tier/cache-race.log)。
80個の大きなbodyを読むeviction・読み直し・Close後の解放も [7.458秒PASS](2026-10-24-event-archive-tier/cache-bounds-race.log)。
これは特定pageのallocation測定であり、製品全体のthroughput/RSS受入ではない。

同時に、圧縮/round tripをSQLite writer transaction外へ移し、確定時の再確認を追加した。
並行Appendやほかの退避との競合はRaceで検証する。元bodyが変わった場合に黙って置き換えない。
修正後QEMU Derivedは **110.31秒PASS**。RAM/vCPU/scratch、freshness・operation・child/parent期限は変更していない。
[修正後manifest](2026-10-24-event-archive-tier/daemon-derived-manifest.json) とserialを保存する。

## 実daemon・実kernelで確認した範囲

最終native pairは上記clean buildのamd64二ELFとSHAが一致し、root所有0700 staging内からRace harnessで実行した。
実supervisor/control/network/Health、DerivedEvent、EventRuleと隔離network namespaceを使った。
新fixtureは圧縮済みbodyを独立gzip readerとSHAで検証し、live/export prefixの元event全内容と比較する。
actual archive書込失敗のhealth503/read-only後も警告保持、障害解除後の回復、control SIGKILL後の
元compressed bytes/size/hash/cursorの一致も確認した。待機jobを捨てたり現在authorityを更新したりしない。

| gate | 最終native | 修正後QEMU |
| --- | --- | --- |
| mixed history export・退避/回収・書込失敗/復旧/control SIGKILL | 6.75秒PASS | 41.97秒PASS |
| live Derived/raw履歴・worker再起動・停止/永久削除queue回復 | 11.76秒PASS | 110.31秒PASS |
| 二WAN Health→PBR、実TCP/UDP/PCAP・全復旧sequence | 30.12秒PASS | 90.25秒PASS |

[native](2026-10-24-event-archive-tier/native.log) と各QEMU manifest/serialを保存する。
二WANはguest内の独立WAN namespaceであり、本番host networkや外部NICを変更していない。
packet capture、socket mark、foreign nft保持、worker再起動、古い通知・期限切れ・confirmed rollbackを含む。
最終8MiB private tmpfsではwarningをmandatoryに要求し、同じ退避/保存失敗/回復/worker crashが **6.45秒PASS**。
[quota log](2026-10-24-event-archive-tier/native-warning.log) を保存する。

初回nativeは誤記したcustom test release identityでも三gateが通ったが、配布ELFとのSHA照合を満たすよう
canonical pairで再実行した。初回7210004 canonical/native・quotaと初回archive QEMUの合格も最終3bca14b受入と分け、
private原logのSHA/metadataをprovenanceへ記録した。関連gateの合格を全QEMU suiteの新aggregate合格とはしない。

## 圧縮済みraw履歴を二度目の電源断後にも読む

専用三boot fixtureは、二度目のcut前に受理済みraw全件についてhot bodyが空であり、
圧縮表の元cursor・元size・SHAが元履歴と一致することをreadonly queryで要求する。
三度目も同じ検査と論理履歴の全内容/cursor比較を行う。hostも `archived_observations` が全件数と一致することを要求し、
部分件数・bool等の偽値をtest-firstで拒否した。fixtureが退避APIを呼んで製品のmaintenanceを肩代わりしない。

最終productの実PID1/supervisor/control/network/Healthと同じprivate256MiB ext4 disk、
1GiB/2vCPU/外部NICなしで **72.91秒PASS**。三つのkernel boot IDは異なる。

| boot | raw event | 圧縮済みraw | 完了job記録 | 終了 |
| --- | --- | --- | --- | --- |
| produce | 2件がlocal queue、target未保存 | 0 | producer1件 | 24.60秒、host SIGKILL/-9 |
| registry | 元2件+追加2件、cursor4/5/6/7 | 全4件 | consumer1+producer2件 | 25.10秒、host SIGKILL/-9 |
| recovered | 同じ4件・全内容・cursor | 全4件、元hot bodyは空 | 同じ3件・元key/position/SHA | 23.21秒、正常poweroff/0 |

登録ACK失敗は三度目にも再発見され、解除後は登録0/健全へ戻った。
Health/Derived現在値は復活せず、旧通知/history POSTは409。保持したEventRuleは回収履歴を消費した。
[manifest](2026-10-24-event-archive-tier/power-manifest.json)、
[produce](2026-10-24-event-archive-tier/power-produce.log)・
[registry](2026-10-24-event-archive-tier/power-registry.log)・
[recovered](2026-10-24-event-archive-tier/power-recovered.log) を保存する。

期待値はUART/次回test initramfsで渡し、cut時のguest witness disk書込/fsync/checkpointや、
後続bootでhelperによる先行mount/recovery/repairを行わない。署名runtime lifecycleは別gateである。
これはguest process/device memoryの喪失試験で、host filesystem cacheや物理controllerの電源断、全cut点の網羅ではない。

## 残作業

同じfilesystemの圧縮保存は外部永続archive/backupの保証ではない。event/IDを忘れず保持するため、全履歴は依然有限である。
外部退避、ID/consumer/producerの世代終了と保持期限、安全な削除/retention policy、総filesystem予算、
外部永続alert、rate/scale/latency/長時間と物理storageの電源断受入は残る。
他raw producer/status provider、構造化field/Derived連鎖、作用/idempotency・federation、
製品版間migration/full DB rollbackと署名runtime lifecycle、残work packageと対象実機/NIC/性能受入も継続する。

読んだ実装: `internal/events/{types,store_linux,journal,checkpoint,outbox,producer,compaction,archive,archive_cache,capacity_linux,history}.go` と各test、
`internal/control/{event_store,server,observations}.go` と各test、`internal/{eventcontroller,derivedcontroller}/`、
`internal/integration/{event_archive,derived,health_pbr,bgp}_test.go`、`images/health-power-test/guest/`、
`scripts/{qemu-network,qemu-health-power}.py` とhost test、Makefileと適用 `../AGENTS.md`。

原logのSHA/抽出/正規化は [provenance](2026-10-24-event-archive-tier/provenance.json)、
公開power witnessでは、認証鍵ではない出力job識別子のfield名だけを `key` から `work_id` へ変更した。
値は保持し、原logのSHAと変換をprovenanceへ記録した。
公開版のSHAは [SHA256SUMS](2026-10-24-event-archive-tier/SHA256SUMS) に保存する。
ELF/initramfs/disk、native host memory/filesystem診断とprivate fixture console詳細はcommitしない。
