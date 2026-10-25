# routerd2 開発引き継ぎ

2026-10-25。全体ゴールは継続中。対象実機で利用可能とは判断していない。

[AGENTS.md](../AGENTS.md) とユーザー指示に従い、未公開API・保存形式・過去の試験を
維持するための実装は残さない。過去checkpointは現行製品の受入根拠にしない。
機能範囲は [implementation-plan.md](implementation-plan.md) と
[implementation-plan.json](implementation-plan.json) を参照する。

## 実装した構造

- 宣言はcontrol所有の読み取り専用データとする。plan/programを一度compileし、
  有限の観測factを直接評価する。一時FactStore・仮producer登録・設定のJSON複製は廃止した。
- 通常通知は観測の受理を返し、BGP/VRRPはcoordinatorの実適用を待つ。
  config適用・定期更新・worker起動停止待ちでkernel書込みと完了処理を共有する。
  I/O中は観測mutexを解放し、古い結果で新しい観測を完了扱いにしない。
  kernel障害で受理済み観測を巻き戻さず、最新観測を再適用する。
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

`make check` が成功。最後のfact/準備確認の整理後にも、runtimeplan/control/serviceruntime/
vrrpのRace Detector、全体vet・fmt確認、amd64/arm64の静的ビルドが成功した。
製品バイナリは `RELEASE_ID=vrrp-ownership`。arm64はビルドのみで実行していない。

最終動作で実daemon 8系統が成功した。元のBOOTTIME、準備判定、実VIP TCP/UDP、
packet capture、worker/control交替、旧通知、条件付きdependentの起動停止を検査した。
二WAN Health/PBR障害切替・復旧・期限切れ・confirmed rollback、BGP/BFD、resource lifecycle、
設定変更・再起動/rollbackも通過した。resource lifecycleは起動直後のHealthy即時assertで
一度失敗した。実際の準備完了を期限付きで待つよう変更し、source・kernel・通信・復旧の
検査を保持したまま単独で再実行し成功した。
Race付きのnative VRRP peer試験でIPv4/IPv6の実raw socket、選出、VMAC/VIP、packet loss後の
切替・撤去が成功した。二つのrouterd2 engineを使う試験であり、独立実装との相互接続の証明
には数えない。NICなしQEMUでの実UTC前進・後退、元のBOOTTIME期限、VIP失効・再適用と
通信も、最後の準備確認の変更後に再実行し成功した。

現行のログは `dist/vrrp-ownership-check.log`、`dist/vrrp-ownership-readiness.log`、
`dist/vrrp-ownership-ready-build.log`、`dist/vrrp-ownership-ready-native*.log`、
`dist/vrrp-ownership-ready-retry*.log`、
`dist/vrrp-ownership-peer.log`、`dist/vrrp-ownership-ready-clock.log`。
過去の出力とのhash一致は実装の前提にしない。

## 続ける実装

1. 通常更新とworker起動停止待ちに残るworker選択の重複処理を揃える。
2. 残るprotocol snapshotの型付き保持、期限切れJSON書換えの削除、条件式の静的計算と、
   retained判定で繰り返すinstance生成・digest計算を整理する。
3. HA-01のtrack/unicast/旧version・auth/cluster方針と独立実装との相互接続を進める。
   全実装計画・全更新復旧経路・外部engine・配備先・対象実機は未完了。

## 環境

PVEの隔離Linux VM、Ubuntu kernel 6.8、amd64。隔離namespaceかNICなしQEMUを使い、
ホストの管理NIC・route・clockは変更しない。通常build/testは `umask 022`。
rootで動かすharness/binaryは一時的なroot所有0700ディレクトリへ配置する。
home配下の権限は緩めない。対象実機・NIC・性能条件は未指定。
