# Durable event保存部とBFD監視期限の分離

2026-10-24 UTC。コードcheckpoint: `c2f3612`。全計画の完了ではない。

## CORE-04: 実装済み・検証済み

`internal/events` に、既存の固定pure Go SQLiteを使うWAL/FULL journal、
durable consumer cursor、cursorと同時に保存するconsumer outboxを追加した。
安定したevent IDの同一再送は同じcursorを返し、内容変更は拒否する。
新consumerは現在のtipから開始し、処理後のcursor保存失敗では再配信する。
完了したjobのtombstoneを保持する。外部作用にはtarget側のdurable idempotencyも必要で、
exactly-onceの保証はしない。履歴eventはprotocol leaseやkernel操作の許可ではない。

保存先はprivate directory、単一writer lock、DB identity/versionとread-only事前検査を使う。
symlink/hardlink/FIFO、他者が書けるdirectory、foreign DBを拒否する。
行数とpayload容量を制限し、満杯では未消費履歴や重複排除を捨てずに書込みを拒否する。

実SQLiteで、reopen・並行重複送信・cursor保存失敗・outbox transaction rollback・
`SQLITE_FULL`（error code 13）・connection交換後のFULL/FK・foreign DBのbytes不変を検証した。
実child processをcommit ACK後にSIGKILLし、CloseなしでWALからevent/cursor/jobを復元した。
[Race Detectorを含む試験log](2026-10-24-events-clock/events-clock-race.log) を参照。

このcheckpointは**保存componentまで**。daemon、EventRule/DerivedEvent、federation、
producer stateとoutboxの同時保存、privileged executorには未接続。
保持期限・archive・容量alarmと、QEMU電源断受入も残る。
payload予算はSQLiteファイル全体のサイズ上限ではない。

## NET-07: 追加修正と検証

先行 `959ba98` のACK観測時刻修正後も、同一commit aggregateでは500msのadmin timeoutが再発した。
隔離診断ではGoBGP FSMがIDLEに到達している一方、ACK goroutineがboot IDのファイル読取りにいた。
boot IDをprocess内で一度だけ保持し、BOOTTIMEとwall timeは毎回読む形にした。
初回失敗・不正IDはcacheしない。並行呼出しとretryを固定試験で確認した。
ただし**cache変更だけでも500ms timeoutが再発**したため、それだけを解決とは扱わない。

admin操作のwatchdogを明示的に**2秒**へ変更した。
これは従来500msからの方針変更であり、BFD control freshnessの**500ms**、
packet期限、withdrawal ACK、再開に必要なclose後のfresh receiveは保持する。
転送proofを全peer分まとめてpublishして失効させてから非同期admin操作を開始する。
ACK欠落・失敗・2秒の期限超過ではworkerを停止する。
管理操作の監視期限を延ばしたことは、古いBFD状態による転送許可の延長を意味しない。

proof失効前にdisableを呼べないこと、遅いadmin ACKだけで古いUpを復活させないこと、
ACK欠落時の停止をRace Detector付きで確認した。
実QEMUのBFD→BGP試験は54.22秒でPASS。同じBGP workerで障害・復旧を処理した。
[serial log](2026-10-24-events-clock/qemu-bfd.log)、
[kernel/ELF hash付きmanifest](2026-10-24-events-clock/qemu-manifest.json) を参照。

## 共通検証・残作業

clean `c2f3612` の `make check build-linux` はPASS。
unit/Python/Race Detector/vetとstatic linux/amd64・arm64ビルドを含む。
eventsのarm64 test binaryもCGOなしでcompileできた。
[全体log](2026-10-24-events-clock/make-check-build.log)、
[provenance](2026-10-24-events-clock/provenance.json)、
[公開artifact hash](2026-10-24-events-clock/SHA256SUMS) を保存する。

全20 QEMU系統は同じ固定commitで実行中。二WAN Health/PBRとBFDを含む通過済み部分はあるが、
完走前に全体PASSとはしない。CORE-04、NET-07を含む作業単位・parity判定は完了へ上げない。
対象実機・NIC・性能の受入、全実装計画の残作業を継続する。
