# Event容量監視と改変検出可能な履歴export

2026-10-24 UTC。product checkpoint `6006090`、追加publication競合test `1a63d92`。
CORE-04の部分実装。全計画・長期運用・対象実機の受入は未完了で、parityは変更しない。

## 実装した契約

global event journalのtransactional usage counterを使い、event・job・登録/stateの件数とbyte上限、
最初/最後と最遅consumerのcursor、DB/WALと実割当量、filesystemの利用可能領域を表示する。
完了jobのdedup tombstoneも使用量に含める。payload全走査や設定lockを保持したSQLite IOを行わない。
論理予算80%でwarning、100%でexhausted。filesystemはBavailが16MiB未満または10%未満でwarning、
0でexhaustedとなる。実装の操作・保存契約は [event-storage.md](../event-storage.md) にまとめた。

`show event-store` / private `GET /v1/event-store` はfresh read-only inspectionである。
healthは約1秒周期のbounded cacheを返し、通常のpollではDBを読まない。
warningだけでは健全性を落とさず、exhausted/query失敗は明示的なhealth errorとなる。
trial confirmは古いcacheに頼らず再確認する。満杯でも期限切れrollbackの正本復元を妨げず、
その履歴は予約済みauthoritative file outboxに残す。既存logging経路の警告を、外部永続alert保証とはしない。

`routerd events export --output FILE` は開始時に固定したtipまでを16件以下のpageで取得し、
元ID・event全内容・観測時刻・expiry・cursorをJSONL version1へ保存する。
長いsnapshot/store lockを保持せず、Appendと現在のHealth観測は続く。
0600 temporary fileの完成・fsync後、置換禁止のatomic renameとdirectory fsyncで新規出力を公開する。
既存file/symlink/並行作成先を保護し、扱えた失敗では最終partialを公開しない。
`routerd events verify FILE` はofflineでframe・順序・ID重複・件数/byte・SHA256・footer/EOFを独立確認する。
checksumは破損検出であって署名ではない。exportはbackup/import/現在authorityの復元機能ではない。

## test-firstとローカル検証

先にevents/control/local CLI/管理shell/APIの期待testをcommitし、未実装FAILを保存した。
新しいactual-daemon fixtureを、先行公開buildのSHAを照合した旧ELFへ実行すると、
新APIのHTTP404でFAILした。fixture側だけで成功を作っていない。
[旧ELF識別](2026-10-24-event-capacity/before-binaries.json) と
[旧daemon FAIL抜粋](2026-10-24-event-capacity/native-old-red-excerpt.log) を参照。

関連Raceでは、全budget/done tombstone、閉じたstore、並行Append、固定prefix、遅いwriter、
途中欠落/改変/不完全footer/重複ID、checksumを再計算した重複も拒否することを確認した。
既存出力・symlink・書込失敗・cancel、page取得中に別操作が作った出力先の保護も検証した。
controlはwarning cacheのhealth readにDB IOがないこと、freshな満杯confirm拒否と期限切れrollback/
未配信outbox保存を実SQLiteで検証した。追加publication競合unitは全体check後の別test commitである。

clean `6006090` の `make check build-linux RELEASE_ID=6006090751ea` は **102.77秒PASS**。
unit・Python21・Race Detector・vet・static amd64/arm64 buildを含む。
[check log](2026-10-24-event-capacity/check-build.log) と
[build manifest](2026-10-24-event-capacity/build-manifest.json) は開始/終了commit一致・dirty=falseを記録する。
追加test commitをこの全体checkの実行対象に遡って含めず、
[個別Race](2026-10-24-event-capacity/publication-race.log) のPASSとして区別する。

## 実daemon・実kernelで検証した範囲

nativeは上記clean amd64 buildと二つのELFのSHAが一致するfresh pairを、root所有0700 stagingから実行した。
Race付きintegration harnessは実supervisor/control/network/Health workerと隔離network namespaceを使う。
本番host networkと外部NICは変更していない。

| gate | native | QEMUの実kernel boot |
| --- | --- | --- |
| 新規archive/capacity | 1.99秒PASS | 26.13秒PASS |
| live Derived/永続履歴と再起動 | 11.92秒PASS | 89.26秒PASS |
| 二WAN Health→PBR、実TCP/UDP/PCAP | 29.56秒PASS | 87.25秒PASS |

新gateではactual CLIの出力をoffline verifyし、archiveの各元event/cursorをlive journal prefixと
独立比較した。既存出力の拒否、同じproducer generation/epochでsequenceが進むことも確認した。
さらにprivate mount namespace内の **8MiB tmpfs** で、実際のfilesystem警告をmandatoryに要求した。
control/Healthがhealthyのままwarningを表示し、4件/cursor4の完全exportとauthority継続が **1.65秒PASS**。
これはSQLITE_FULL後の空き領域回収や無制限負荷試験ではない。

二WAN gateはprimary障害/backup障害/復旧、worker再起動、旧通知・期限切れ、confirmed rollback、
TCPとUDPの実経路とcapture、probeのsocket mark、foreign nft保持を通す。
QEMUは512MiB/2vCPU/外部NICなし、64MiB scratchで、内部に独立WANのnamespaceを作る。
関連3gateの合格であり、全QEMU suiteの新aggregate合格とはしない。
[native](2026-10-24-event-capacity/native.log)・[実quota警告](2026-10-24-event-capacity/native-warning.log)、
[archive manifest](2026-10-24-event-capacity/daemon-event-archive-manifest.json)・
[Derived manifest](2026-10-24-event-capacity/daemon-derived-manifest.json)・
[PBR manifest](2026-10-24-event-capacity/daemon-health-pbr-manifest.json) と各serialを保存する。

## 二度の電源断回帰

同じ新productで既存の三boot/two-SIGKILL gateも **72.65秒PASS** した。
build headは追加testだけが異なる `1a63d92` で、ELFのidentityは上記native/QEMU pairと別に保存した。
外部NICなし・1GiB/2vCPU、同じprivate256MiB ext4 regular-file diskを実製品がmountする。
最初の2件に次bootで受理した1件を加えた全3件が一度ずつ公開され、
二度目のcut後も全内容とcursor4/5/6が一致した。local ACKは空、登録失敗は保持され、
障害解除後は登録0/健全へ戻り、旧authority/currentは復活しない。
produce24.73秒とregistry24.12秒はhost SIGKILL/-9、recovered23.80秒は正常poweroff/0。
UART/次回initramfs witnessであり、cut時のguest witness fsyncや次boot前のhelper mountはない。
[manifest](2026-10-24-event-capacity/power-manifest.json) と
[produce](2026-10-24-event-capacity/power-produce.log)・
[registry](2026-10-24-event-capacity/power-registry.log)・
[recovered](2026-10-24-event-capacity/power-recovered.log) を保存する。
これはguest process/device memoryの喪失試験で、host/物理storage cacheの電源断や全cut点の網羅ではない。

## 残作業

このexportは履歴のprefix copyであり、source event・consumer cursor・outbox・dedup記録を削除しない。
**満杯のjournalを空ける回収処理は未実装**。自動archive退避、永続化の確認と安全なhot領域回収、
再送ID/dedupの保持期限、consumer/timerが参照する履歴との契約が必要である。
総filesystem予算、外部永続alert、規模/遅延/長時間運用、他raw producer/status provider、
構造化field/Derived連鎖、作用/idempotency・federation、製品版間migration/full DB rollback、
残work packageと対象実機/NIC/性能受入も継続する。schema4とparity flagsは変更しない。

実コードを読んだ範囲: `internal/events/{store_linux,capacity_linux,history}.go`、
`internal/control/{control,event_store,server,state_views}.go`、`cmd/routerd/{main,events_linux}.go`、
`internal/cli/{session,completer,event_store}.go` と各test、
`internal/integration/{event_archive,derived,health_pbr}_test.go`、`images/kernel-test-init/`、
`scripts/{qemu-network,qemu-health-power}.py`、`Makefile`、適用 `../AGENTS.md`。

原logのSHA/抽出/正規化は [provenance](2026-10-24-event-capacity/provenance.json)、
公開版のSHAは [SHA256SUMS](2026-10-24-event-capacity/SHA256SUMS) に保存する。
ELF/initramfs/diskはcommitしない。native hostのmemory/filesystem容量診断と旧fixture console詳細は公開logから省いた。
