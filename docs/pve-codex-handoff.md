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
キュー満杯でworkerを終了・再起動させず、配送で空きができるまで待つ。
待機中は新たなprobeを行わないため、最後の現在観測は期限切れになり得る。
永久的な保存エラーは依然としてエラー。履歴の無制限保持は実装していない。

## この変更で実行した確認

PVEの隔離Linux VM、Ubuntu kernel 6.8、amd64の実daemonとkernelを使用した。
`make check build-linux RELEASE_ID=runtime-redesign` が成功した。
Race Detector、vet、amd64/arm64の静的ビルドを含む。
最初の全体チェックでは、変更したIPCに電源断fixtureの呼出し側が追随しておらず
コンパイルに失敗した。呼出し側を更新した後に全体チェックを完走した。

以下は新しいバイナリとRace Detector付きharnessで成功した。

- Healthの実TCP・HTTP・DNS・raw ICMPソケット。
- `TestDaemonVRRPGracefulAdmission`。未準備時のVIP保留、遅れた準備完了後の
  VIPと実TCP/UDP、準備喪失後の維持、元の期限による失効、worker/control再起動。
  直前の実装で失敗したsupervisor停止を含む復旧経路も通過した。
- `TestDaemonHealthPBRFailoverAndRecovery`。独立二WAN、実TCP/UDPとpacket capture、
  worker再起動、旧通知、期限切れ、confirmed rollback。
- `TestDaemonDerivedHealthCurrentStatusAndDurability`。保存先障害、worker/control再起動、
  履歴再送と重複排除、現在観測の期限と履歴の分離。

`dist/runtime-redesign-check.log` と `dist/runtime-redesign-{0,1,2,3}.log` は
ローカルの実行ログ。過去の成果物とのhash一致を作業の前提にしない。
新IPCでのQEMU電源断試験とarm64での実行は未実施。

## 続ける実装

1. 観測受理・kernel適用・worker起動停止を一つの実行責務へ揃える。
   現状は条件付き構成の一般通知が適用前に成功を返し、VRRPには複数の適用経路と
   grant往復がある。単に全通知を待機させると停止中のcallbackと循環する。
2. 受理済み観測を型付きで保持し、期限切れを表すためのJSON書換えをなくす。
   条件評価の一時FactStore登録と、観測ごとの設定再コンパイルも廃止する。
3. 実装計画の残機能を順に接続し、実通信と障害後の復旧を確認する。
   既存チェックが通っても、全作業完了へ読み替えない。

対象実機・NIC・性能条件での受入、全更新復旧経路、全計画は未完了。
ホストの管理NIC・route・clockを変更しない。試験は隔離namespaceかNICなしQEMUで行う。
通常のbuild/testは `umask 022`。rootで動かすharness/binaryは一時的なroot所有0700
ディレクトリへ配置する。home配下の権限を緩めて実行可能にしない。
