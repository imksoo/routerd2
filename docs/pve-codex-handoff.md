# routerd2 開発引き継ぎ

2026-10-25。全体ゴールは継続中。実機利用可能とは判断していない。

[AGENTS.md](../AGENTS.md) とユーザー指示に従い、未公開API・保存形式・過去の試験を
維持するための実装は残さない。過去checkpointの記録は現行製品の受入根拠にしない。
機能範囲は [implementation-plan.md](implementation-plan.md) と
[implementation-plan.json](implementation-plan.json) を参照する。

## 実装した構造

- Healthの現在観測と履歴配送を分離した。履歴保存先の故障やqueue満杯でprobeを止めない。
  履歴欠落・保存エラーは現在snapshotへ表示する。全probeの無欠落保存は保証しない。
- 条件評価の一時FactStore・仮producer登録・設定のJSON複製を廃止した。
  control所有の読み取り専用宣言からplanを一度compileし、観測時には直接Factを評価する。
  EventRule/DerivedEventも計算済みprogramを再利用する。
- 通常通知は検証済み観測の受理を返し、kernel適用はcoordinatorが所有する。
  BGP/VRRPは実適用を待つ専用要求へ変更した。条件の有無で通知の意味を変えない。
  config適用・定期更新・worker起動停止待ちのkernel書込みと完了処理を共有する。
- kernel I/O中の観測mutex保持と、適用失敗で受理済み観測を巻き戻す処理を削除した。
  新しい観測は保持して再適用し、古い結果で新しい通知を完了扱いにしない。
  未適用の観測・適用中の処理があればReady/confirmed commitを拒否する。
- config適用で同じinstanceを二度書く処理を削除した。digest計算と保存に同じbytesを使い、
  ファイル準備・BFD解析は観測用mutexの外で行う。停止中の最新観測を採用し、
  instance構成が変わった場合だけ準備と停止を繰り返す。

## 今回の確認

`make check build-linux RELEASE_ID=coordinator` が成功した。表示の鮮度確認の最終変更にも
runtimeplan/control/serviceruntime/legacyconfigのRace Detector、全体vet、静的ビルドが成功した。
Linux amd64/arm64の3 ELFずつをビルドした。arm64は実行していない。

PVEのUbuntu kernel 6.8で、新バイナリとRace付きharnessの実daemon 14系統が成功した。
独立二WANの実TCP/UDP・packet capture、障害切替/復旧、worker再起動、旧通知、期限切れ、
confirmed rollbackを確認した。BGPの実FIBとBFDによるセッション遮断・再接続、
BGP/BFD-derived、VRRP通常/条件付き/graceful、DHCP4、DHCP→DNS、RA-client、AFTR、
resource lifecycle、履歴障害中のHealthも通過した。

初回試験では、切替途中のkernelと表示を同時に変わると仮定したassert、期限切れBGPにも
成功を期待するassert、VRRP仮適用を本適用完了と扱う待機、RA再開時の古い準備状態を
使う待機が失敗した。適用中/鮮度切れの表示を修正し、試験を実際の完了・収束に合わせて
再実行した。元の期限・実通信・障害復旧の検査は残した。

独立したnetwork namespaceでHealthの実TCP/HTTP/DNS/raw ICMPが成功した。
NICなしQEMU、Alpine kernel 6.18.58-virtでは、実UTCの前進・後退後もVRRPの元BOOTTIME期限と
kernel VIP失効・再適用・通信を確認した。実機の時計は変更していない。

ローカルログは `dist/coordinator-final-check.log`、`dist/coordinator-final-related.log`、
`dist/coordinator-final-native*.log`、`dist/coordinator-health-sockets.log`、
`dist/coordinator-vrrp-clock.log`。保存した過去の出力とのhash一致は実装の前提にしない。
全更新復旧経路・QEMU電源断・arm64実行・対象実機・全実装計画は今回完了していない。

続くinstance準備の整理では `RELEASE_ID=instance-preparation` をビルドした。
`make fmt-check vet build-linux` と runtimeplan/config/control/legacyconfig のRace Detectorが成功。
最終バイナリでは実daemonの設定変更・再起動/rollback、Health/PBR二WAN、VRRP条件付き/graceful、
resource lifecycleの5系統が成功した。ログは `dist/instance-preparation-{related,build,native}.log`
と `dist/instance-preparation-native-TestDaemon*.log`。この整理後のQEMU・残りのdaemon系統は
再実行していない。

## 続ける実装

1. VRRPのadmission grant往復・複数latchを減らし、準備判定とVIP適用を単純化する。
   起動停止待ちと通常更新は同じ適用処理を使うが、worker選択の重複処理は残る。
2. protocol snapshotを受理時から型付きで保持し、期限切れを表すJSON書換えを廃止する。
   条件式の静的計算と、retained判定で繰り返すinstance生成・digest計算も整理する。
3. 実装計画の残機能を接続し、独立peerとの実通信と障害後の復旧を確認する。
   全更新復旧経路、外部engine、配備先、arm64実行、対象実機は未完了。

## 環境

PVEの隔離Linux VM、Ubuntu kernel 6.8、amd64。試験は隔離namespaceかNICなしQEMUで行い、
ホストの管理NIC・route・clockは変更しない。通常build/testは `umask 022`。
rootで動かすharness/binaryは一時的なroot所有0700ディレクトリへ配置する。
home配下の権限は緩めない。対象実機・NIC・性能条件は未指定。
