# supervisorの世代境界と更新・永続状態の受入

2026-10-24 UTC。コードcheckpoint: `9a07f1a9a6eac386e7441a7e6448b57ba1b3049d`。
全実装計画・対象実機の受入を完了する継続ゴールの部分checkpoint。

## 実装済み

supervisorは起動ごとに暗号学的乱数の128bit `instance_id` を生成し、各workerへ継承する。
設定や親からの同名環境変数ではこの値を選択できない。
protocolのUpdateに `supervisor_instance` を持たせ、controlが継承した値と異なる通知は
regular経路・独立BFD経路のcacheを触る前に409で拒否する。
新しいsupervisorでworker generationが1から始まっても、旧supervisorの大きい番号でcacheを汚染しない。
既存worker内のproducer epoch・sequence、config digest、boot・有限期限の検査も保持する。

`instance_id` は再起動境界の識別子である。role/peer credential認可とprivileged IPCの権限制御はCORE-06の残作業。
worker単位の初回通知からの厳密なauthority照合も、完成した権限分離としては扱わない。
未公開内部IPCの変更であり、識別子を欠く旧Updateを受理する互換経路は追加しない。

更新・ストレージ試験のbuilderは、`go.mod` が選択するGoの実体を先に解決するよう修正した。
その実体で `GOTOOLCHAIN=local` を指定し、実際のversionをmanifestへ保存する。
OS標準のGo 1.22.2がプロジェクト用Go 1.27.3へ切り替わらずビルドに失敗する問題を再現して修正した。

## 検証済み

| 検証 | 結果・証跡 |
| --- | --- |
| 単体・race・vet・build | `make check build-linux` PASS。static amd64/arm64。[log](2026-10-24-supervisor-authority/make-check-build-release-initial.log) |
| 空cacheへの旧通知 | DHCP4とBFDの両方で、旧supervisorのgeneration/sequence 9999を拒否し、新producerの最初の通知を受理 |
| supervisor実プロセス | 継承された偽値を上書き。全workerが同じIDを継承し、worker再起動中はsupervisor IDを保持 |
| clean実daemon二WAN | PASS、31.17秒、`dirty: false`。[manifest](2026-10-24-supervisor-authority/result.json)、[log](2026-10-24-supervisor-authority/test.log) |
| 同じ固定commitのQEMU Health/PBR | PASS、75.03秒。[manifest](2026-10-24-supervisor-authority/qemu-health-manifest.json) |
| amd64/arm64の実build署名・stage・CLI publish | PASS。[log](2026-10-24-supervisor-authority/release-built-private.log)、[入力hash](2026-10-24-supervisor-authority/release-input-hashes.json) |
| 署名trialのPID 1実行・自動確認 | PASS、97.66秒。[manifest](2026-10-24-supervisor-authority/release-boot-manifest.json) |
| 永続ext4・2回起動・未確認trial fallback | PASS、139.57秒。[公開subset](2026-10-24-supervisor-authority/storage-manifest.json)、[log](2026-10-24-supervisor-authority/release-storage.log) |

実daemonでは、既存の障害切替・hold・expiry・worker再起動・confirmed rollback後にsupervisor全体を停止し、
同じ設定storeで新しいsupervisorを起動する。workerのgenerationが以前より小さくなったことを確認し、
以前のプロセス群の通知を実control socketで409にする。revision 3を保持し、実TCP/UDPがAで復旧する。
[milestones](2026-10-24-supervisor-authority/milestones.json)、
[WAN-A](2026-10-24-supervisor-authority/wan0.pcap)、[WAN-B](2026-10-24-supervisor-authority/wan1.pcap) を保存した。

2回起動試験は、新しく作成した256MiBのprivate regular-file diskだけをQEMUへ接続する。
1回目の未確認trialを2回目にbaselineへ戻し、設定revision 2・SSH identity・sentinelの保持を確認する。
hostのblock device・filesystem mount・外部NICは使用しない。
これは現在の署名付きcommand pairの受入であり、多binary runtimeやkernel/rootfs更新の完成ではない。

release CLIの最初の実build試験は、group writableなworkspaceの入力を正しく拒否した。
同じbuildの4 ELFをhash照合してprivate temporary directoryへコピーし、mode 0600の入力で再実行して合格した。
製品側のpath所有権・書込権限検査は緩和していない。

## 失敗からの修正と公開証跡

QEMUでは、max_ageによる撤去がHealthのUnhealthy通知より先に起きる場合を確認した。
packet gateは両方を待ってthreshold障害を確認するよう修正した。
holdによる先行撤去をproducer期限切れと取り違えた待ち合わせも修正し、
実CLOCK_BOOTTIMEが期限を超え、status理由がholdではなくindeterminateになったことを確認する。
受入条件を緩和する再試行やtimeout延長ではなく、異なる失効原因を別々のpredicateで待つ変更である。

公開subsetには生statusのboot IDやsupervisor IDを含めない。
storage manifestのboot IDは `boot-1` / `boot-2` のaliasで同一性を保持し、
SSH keyのhash/fingerprintは保持確認のbooleanへ置き換えた。元manifestのhashも記録した。
log中のworkspaceとprivate input directoryはplaceholderにした。
生artifactは `dist/health-pbr-test/20261024T121357Z/`、`dist/release-boot-test/`、`dist/storage-test/` に保持する。
公開subsetのhashは [SHA256SUMS](2026-10-24-supervisor-authority/SHA256SUMS)。
release/storageはtoolchain修正のcommit前run、QEMU Health/PBRは固定commitの別worktreeでのrunである。

## 未完了

全20 QEMU gateを同じcommitで実行するaggregateは継続中。以前のaggregateはtoolchain選択や待ち合わせで停止した。
CORE-04のdurable journal/outbox/cursor、CORE-03の一般status reference・残りのlifecycle、
CORE-06のauthority/権限分離、runtime catalog/startup store handoff等を続ける。
対象実機・NIC・性能条件の受入、HA/cloud profileを含む全work packageの完了も残る。
既存parity判定を引き上げない。
