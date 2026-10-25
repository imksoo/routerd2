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
- BGP経路の元のboot IDとBOOTTIME期限をnetwork workerへ渡し、controlと独立して
  期限切れ経路を撤去する。期限更新だけではkernel経路を置換せず、所有記録を更新する。
  退役時も同じ所有権確認を使い、変更されたforeign経路を残す。table観測と経路選択は
  操作内で索引化した。network journal/runtime宣言はformat 3に直接置換した。
  経路追加は不在確認後に追加予定と元の期限をまとめて永続化し、kernel追加直後の
  writer停止でも復旧側が撤去できるようにした。期限切れ撤去もまとめて保存する。
  journal読取りはatomic置換でunlinkされた開いた旧inodeを許す。lock・書込み先は
  引き続きlink必須で、hard linkは読取り・書込みとも拒否する。

## 今回の確認

製品は `RELEASE_ID=route-authority`。`make check`、networkの最終個別Race Detector、
全体vet・fmt確認、amd64/arm64の静的ビルドが成功した。arm64は実行していない。
専用0700ディレクトリへ6 ELFを配置し、`umask 022` で `make release-built-test` も成功した。
両architectureのruntime宣言・署名bundle・publisher CLI出力を実receiverで確認した。
group書込み可能な作業treeからの入力拒否を回避するために製品の権限検査は緩めていない。

実BGP peerから学習した経路を使い、control/BGPを停止し、network workerをSIGKILLして
再起動した。その後supervisorも停止したまま、元の期限で経路を撤去できた。
追加前の所有予定保存により、kernel追加と保存の間に停止して期限情報を失う窓を除いた。
再起動後の期限保持、期限更新時にkernel経路を置換しないこと、失効後・別bootの再適用で
経路を復活させないこと、foreign置換を残すことはcomponent試験でも確認した。

実daemonのBFD/BGP停止復旧と二WAN Health/PBRも成功した。後者はTCP/UDP・capture、
worker/control/network交替、旧通知、期限切れ、confirmed rollbackを含む。
Healthの再起動待ちは、新workerの世代・digestに一致するHealthy観測と適用済みReady、
実primary経路を待つ。旧観測が残っているだけで復旧成功と判定していた試験を修正した。

隔離namespaceで、実kernelのIPv4/IPv6 PBR・ECMP TCP、NAT/ACL、DS-Lite、bridge/VLAN/
VRF/VXLAN/macvlan、IPv6経路期限、VRRP receive guardとforeign資源保護が成功した。
全体チェックで再現したjournal置換中の読取り失敗も修正し、集中Race試験20回が成功した。

初回BFD再接続timeoutの直接原因は未確定。現在の再試験成功だけでこの問題を閉じない。
DHCP/RA/AFTR/Derived/EventRule/VRRPの実daemon全系統、QEMU時計補正、物理suspendは
この変更後に再実行していない。対象実機・NIC・性能の検証も未実施。

現在の出力は `dist/route-authority-final-check.log`、`dist/route-authority-checked-build.log`、
`dist/route-authority-final-network-race.log`、`dist/route-authority-ready-native*.log`、
`dist/route-authority-intent-kernel.log`、`dist/route-authority-journal-race.log`。
配布物確認は `dist/route-authority-publish.log`。
過去出力との一致を実装の条件にしない。

## 続ける実装

1. 初回BFD再接続timeoutを再現・診断し、原因を確定する。成功回数で閉じない。
   BGP観測期限は稼働中のnetwork writerが独立して失効させる。writer自身が停止した間の
   IPv4失効と、BFD受信期限・再帰next-hopの前提期限を全てwriterへ渡す処理は未検証・未実装。
2. 残るprotocol snapshotの型付き保持、期限切れJSON書換えの削除、条件式の静的計算と、
   retained判定で繰り返すinstance生成・digest計算を整理する。
3. HA-01のtrack/unicast/旧version・auth/cluster方針と独立実装との相互接続を進める。
   全実装計画・全更新復旧経路・外部engine・配備先・対象実機は未完了。

## 環境

PVEの隔離Linux VM、Ubuntu kernel 6.8、amd64。隔離namespaceかNICなしQEMUを使い、
ホストの管理NIC・route・clockは変更しない。通常build/testは `umask 022`。
rootで動かすharness/binaryは一時的なroot所有0700ディレクトリへ配置する。
home配下の権限は緩めない。対象実機・NIC・性能条件は未指定。
