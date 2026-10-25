# routerd2 開発引き継ぎ

2026-10-25。全体ゴールは継続中。対象実機で利用可能とは判断していない。

[AGENTS.md](../AGENTS.md) とユーザー指示に従い、未公開API・保存形式・過去の試験を
維持するための実装は残さない。過去の `docs/evidence` は削除した。
機能範囲は [implementation-plan.md](implementation-plan.md) と
[implementation-plan.json](implementation-plan.json) を参照する。

## 実装した構造

- 宣言はcontrol所有の読み取り専用データとする。plan/programを一度compileし、
  有限の観測factを直接評価する。一時FactStore・仮producer登録・設定のJSON複製は廃止した。
- 通常通知は観測の受理を返し、BGP/VRRPはcoordinatorの実適用を待つ。
  config適用・定期更新・worker起動停止待ちでkernel書込みと完了処理を共有する。
  I/O中は観測mutexを解放し、古い結果で新しい観測を完了扱いにしない。
  kernel障害で受理済み観測を巻き戻さず、最新観測を再適用する。
- 通常監視・起動停止待ち・設定切替は同じreconcileを一回ずつ進める。installedの重複保持と
  停止待ち専用の処理を削除した。supervisorの実登録と設定identityで継続・退役を選ぶ。
  通常監視は停止を要求して戻り、次回に実終了を確認する。設定切替だけが期限付きで待つ。
  切替前は退役workerの終了だけを待つ。継続workerのReadyは新設定適用後に確認し、
  現在の準備失敗が復旧設定を妨げないようにする。停止要求前にapplyingへ更新する。
  前提がI/O中に変わったworkerは起動せず、最新の観測で再評価する。
- instanceファイルは同じ準備済みbytesでdigestを計算し保存する。二重書込みは廃止した。
  ファイル準備・BFD解析は観測mutexの外で行い、停止中の更新をcutoverへ反映する。
- VRRPのgrant発行・返送、仮VIP、期限昇格、受信時刻から作る別リース、失効JSON書換えを
  削除した。選挙観測から直接factを作り、成功したkernel適用の選挙keyだけを保持する。
  `Effects.Apply` と同じservice要求の応答で、実際にVIPを所有したかをworkerへ返す。
- gracefulの準備条件はkernel適用を開始するときの入場条件である。成功後は、I/O中の喪失を
  含む準備喪失だけでは撤去しない。初回適用失敗は入場許可を残さず、再試行で準備を再判定する。
  条件除外・role喪失・producer交替・元のBOOTTIME期限は引き続き所有権を失効させる。
  GARP/NAは適用応答と実netlink検査の後に送る。準備待ちのgraceful選挙は広告だけを続ける。
- config適用終了と定期更新は同じ準備確認を使う。VIP未公開の起動直後や未適用の観測を
  Ready/confirmed commitの成功へ読み替えない。
- Healthの現在監視と履歴配送は分離済み。保存先障害・queue満杯でprobeを止めず、欠落と
  保存エラーをsnapshotへ表示する。全probeの無欠落保存は保証しない。

## 今回の確認

製品は `RELEASE_ID=worker-step`。最終修正で `make check`、runtimeplan/controlの個別Race Detector、
全体vet・fmt確認、amd64/arm64の静的ビルドが成功した。arm64は実行していない。

最終修正の実daemon試験は、設定復旧、条件付きresource lifecycle、BGP FIB/元の期限、
BFDによるBGP停止復旧、VRRP条件付き/graceful、二WAN Health/PBRの7系統が成功した。
実TCP/UDP・capture、worker/control交替、旧通知、期限切れ、confirmed rollbackを含む。
隔離namespace内で実supervisor/control/network/protocol workerとLinux kernelを動かした。

今回直したBGP期限切れの原因は、継続するworkerのstoppingまで退役待ちと扱ったこと。
停止待ちを設定から外す・置き換えるworkerに限定し、failedもkernel適用後に報告する。
通常監視が退役待ちで戻ること、実終了までVIPを残すこと、独立したWAN適用が進むこと、
I/O中に前提を失ったworkerを起動しないこと、切替前にReadyを表示しないこと、
準備失敗中も新設定をkernelへ適用できることをcomponent試験でも確認した。

初回のBFD再接続timeoutは直接原因が未確定。その後の再試験では成功しているが、
再現・診断と負荷下の受入は未完了。成功回数だけでこの問題を閉じない。
DHCP/RA/AFTR/Derived/EventRuleの実daemon全系統、QEMU時計補正、native VRRP IPv4/IPv6 peerは
最終修正後に再実行していない。対象実機・NIC・性能の検証も未実施。

現在のローカル出力は `dist/worker-step-cutover-{check,race,build,native}.log` と
`dist/worker-step-cutover-native-*.log`。過去出力との一致を実装の条件にしない。

## 続ける実装

1. 初回BFD再接続timeoutを再現・診断し、原因を確定する。成功回数で閉じない。
   BGP IPv4経路の元の期限をnetwork writerにも接続する。他workerのdrainと重なる場合の
   失効は追加検証が必要。現状はcontrolが経路撤去を判断する、というコードからの推論。
2. 残るprotocol snapshotの型付き保持、期限切れJSON書換えの削除、条件式の静的計算と、
   retained判定で繰り返すinstance生成・digest計算を整理する。
3. HA-01のtrack/unicast/旧version・auth/cluster方針と独立実装との相互接続を進める。
   全実装計画・全更新復旧経路・外部engine・配備先・対象実機は未完了。

## 環境

PVEの隔離Linux VM、Ubuntu kernel 6.8、amd64。隔離namespaceかNICなしQEMUを使い、
ホストの管理NIC・route・clockは変更しない。通常build/testは `umask 022`。
rootで動かすharness/binaryは一時的なroot所有0700ディレクトリへ配置する。
home配下の権限は緩めない。対象実機・NIC・性能条件は未指定。
