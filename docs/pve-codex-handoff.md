# routerd2 開発引き継ぎ

2026-10-25。全体の開発ゴールは継続中。実機利用可能とは判断していない。

## 方針

[AGENTS.md](../AGENTS.md) と最新のユーザー指示に従う。未公開API・IPC・保存形式・
過去の試験を維持するために複雑な実装を残さない。必要なら両端と試験を作り直す。
過去checkpointの試験一覧は現行製品の受入判定に使わない。
実装計画の機能範囲は [implementation-plan.md](implementation-plan.md) と
[implementation-plan.json](implementation-plan.json) を参照する。

## 今回変更した動作

Health履歴配送を現在のworker登録・supervisorの準備状態から分離した。
登録済みキューの履歴を `{resource, events}` で受け付け、現在の観測と経路変更の
世代・期限チェックは別のservice APIで行う。旧producer envelopeの受付経路は削除した。
満杯時の待機も監視を止めるため廃止した。履歴の保存・再送・storeの再openは
専用の処理が所有し、監視は待たない。履歴用のメモリchannelは16件、永続queueは
64件/64KiB。永続化できなかった観測は欠落し、検出した件数とエラーを現在の
Health snapshotの `history_dropped` / `history_error` へ表示する。
件数はproducer epoch内の値で、再起動でresetする。未保存のchannel内イベントは
異常終了・通常停止でも失われ得る。全probeの無欠落保存は保証しない。

## 実行した確認

PVEの隔離Linux VM、Ubuntu kernel 6.8、amd64の実daemonとkernelを使用した。
最初のHealth配送変更では `make check build-linux RELEASE_ID=runtime-redesign` が成功。
Healthの実TCP・HTTP・DNS・raw ICMP、VRRP復旧、二WAN切替、履歴復旧が通った。
電源断fixtureの旧IPC呼出しによるコンパイル失敗は、呼出し側を更新して解消した。

一時FactStore廃止後に `make check build-linux RELEASE_ID=direct-facts` が成功した。
その後の履歴recorder分離では、関連6 packageの単体・Race Detectorと、最終recorderの
関連3 packageのRace Detector、全体vet・format確認、amd64/arm64の静的ビルドが成功。
最終製品バイナリは `RELEASE_ID=direct-facts`。

最終バイナリとRace Detector付きharnessで、以下を確認した。

- `TestDaemonHealthContinuesWithFullHistory`。履歴保存先のSQL障害でqueueを満杯にした
  状態でも、実TCP通信先の停止・復旧へ現在の疎通判定が追従した。履歴の欠落を表示し、
  同じworkerのまま配送も復旧した。
- `TestDaemonVRRPGracefulAdmission`。未準備時のVIP保留、遅れた準備完了後の
  VIPと実TCP/UDP、準備喪失後の維持、元の期限による失効、worker/control再起動。
  直前の実装で失敗したsupervisor停止を含む復旧経路も通過した。
- `TestDaemonHealthPBRFailoverAndRecovery`。独立二WAN、実TCP/UDPとpacket capture、
  worker再起動、旧通知、期限切れ、confirmed rollback。
- `TestDaemonDerivedHealthCurrentStatusAndDurability`。保存先障害、worker/control再起動、
  保存済み履歴の再送と重複排除、現在観測の期限と履歴の分離。
  初回は保存済みの先頭要素の再送にも503を期待する旧assertで失敗した。
  旧世代拒否に由来する余分なPOST/assertを削除し、実際のworker再送・保存内容・
  重複数・現在観測の検査を再実行して成功した。製品の再変更はしていない。

ローカルログは `dist/direct-facts-check.log`、`dist/health-history-continuity-check.log`、
`dist/health-recorder-check.log`、`dist/direct-facts-native-{0,1,2,3}.log` と
再実行の `dist/direct-facts-native-derived.log`。
新IPCでのQEMU電源断試験、arm64実行、対象実機は未検証。
過去成果物とのhash一致は作業の前提にしない。

## 続ける実装

1. 観測受理・kernel適用・worker起動停止を一つの実行責務へ揃える。
   現状は条件付き構成の一般通知が適用前に成功を返し、VRRPには複数の適用経路と
   grant往復がある。単に全通知を待機させると停止中のcallbackと循環する。
2. 条件評価の一時FactStore・仮producer登録は廃止した。値・元の期限・実producerの
   識別情報を一つのFactへまとめ、直接評価する。続けて受理時の型付き保持を統一し、
   期限切れを表すJSON書換えと、観測ごとの設定再コンパイルを廃止する。
3. 実装計画の残機能を順に接続し、実通信と障害後の復旧を確認する。
   既存チェックが通っても、全作業完了へ読み替えない。

対象実機・NIC・性能条件での受入、全更新復旧経路、全計画は未完了。
ホストの管理NIC・route・clockを変更しない。試験は隔離namespaceかNICなしQEMUで行う。
通常のbuild/testは `umask 022`。rootで動かすharness/binaryは一時的なroot所有0700
ディレクトリへ配置する。home配下の権限を緩めて実行可能にしない。
