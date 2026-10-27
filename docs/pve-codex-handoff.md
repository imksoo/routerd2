# routerd2 開発引き継ぎ

2026-10-27。全34 work packageのゴールは継続中。homertの本番置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を参照する。
実行順序はhomert02/03、実態を確認したk8s-rt/pve-rt、その後既存文書に基づくCloud SAM。

## 現在の変更: ha-conntrack / native-29

homertで使われるconntrackd FTFWを、IPv4 VRRP workerが所有するengineとして接続した。
独立した新しいcoordinator・registry・shell hookは追加していない。
旧routerdの`cmd/routerd-vrrp-vmac/main.go`と`pkg/render/conntrackd.go`を直接比較した。

- 昇格は`-c → -R → -B`を進めてから、既存control/network経路へelected状態を渡す。
  peer cacheのkernel commit完了を確認してからWAN identityとLAN VIPを公開する。
  rescan/broadcast要求の受付は、peerへの全転送完了とは区別する。
- 降格はwriterの撤去ACK後に`-t → -n`を要求する。engineは最終撤去まで維持する。
  engine異常終了はVRRPの撤去とworker交換へ結ぶ。
- `forwarding.conntrack_tcp_liberal=true`を明示し、writerが適用・元値の保存・撤去時の復元を行う。
  VRRPはこの設定、同期interfaceと宣言されたlocal addressへ依存する。
- conntrackd 1.4.9を固定Alpine入力からbuildする。実行ファイル・loader/library・protocol DB・licenseを
  regular-file imageへ収め、元source/APKBUILD/patch/build scriptを別artifactとして署名で結ぶ。
  FD5と空のprivate mount/chrootを使い、host binary/config/libraryへfallbackしない。
- 元のCLI exit0は部分commit失敗を表さない。GPLの小さなpatchで実commit終了時のconntrack/
  expectation失敗件数を返し、失敗または完了応答なしでは公開しない。stdoutと診断stderrは分ける。
- boot ABI1・worker ABI5・manifest v4。compilerはnative-29、network journalは10。
  旧開発journalのreader/migrationは作らない。既存`pppimage`は二つの実engineが使う`engineimage`へ移した。

実装・設定例・所有権の範囲は[conntrack同期](../internal/protocol/conntracksync/README.md)を参照。

## 実行した範囲

`ha-conntrack`のPVE Ubuntu Linux6.8/amd64、二つの署名済み実PID1 treeで確認した。
supervisor/control/network/management/logging/VRRPとconntrackdを実際に起動し、別Linux端末と
別Linux serverのTCP/UDPをNAT転送した。物理portの静的address、WAN VMAC/VIP、LAN VIP、
route/firewallとconntrack sysctlはproduct設定で作り、手動補修や残存addressへ依存させない。

- 一つのTCP socketと一つのUDP socketで連続送信し、A→B→Aの切替と旧worker/engine再起動後も
  同じTCP接続・NAT対応を維持した。`tcp_loose=0`で途中TCPの作り直しを防いだ。
- 稼働中engineを実SIGKILLし、撤去、peerへの切替、worker/engine再作成後の同じ通信を確認した。
- captureはclient SYNが一つで、FTFWの実packetと同じNAT tupleを確認した。
  最終cohortのUDP損失は0、先行cohortでは切替中に1 datagram失った。無損失保証ではない。
- 設定から機能を削除し、workerのreapと元のconntrack sysctl値0への復元を確認した。
- 同期だけを外した比較では、初期TCP/UDPの成立後、最初の切替で元TCPのreadがtimeoutした。
  最初の比較の観測処理はstderr混入でも失敗した。診断出力を分離した再実行で、このTCP失敗を確認した。
- `make check build-linux build build-publisher RELEASE_ID=ha-conntrack`成功。
  全Go/Python、Race Detector、vetとamd64/arm64の6静的Go ELFを含む。
- `make check-health-pbr RELEASE_ID=ha-conntrack`成功。実2 WANのTCP/UDP切替/復旧、
  Health/control/network再起動、古い通知、元期限/利用側鮮度、writer再起動時の撤去、confirmed rollbackを確認した。
  最終6 ELFの署名catalog/publisher/stageもRace Detector付きで成功した。ARM64 engine build/実行は行っていない。

部分commit応答のdecoderは局所試験で確認したが、実kernel tableの枯渇は未実施。
factory imageへの同梱、同期link分断/gap/古いpeer、認証付き参加、VRF/zone、大量状態、
物理NIC/負荷/電源断の受入は未完了。今回のTCP継続をHA-03全体やhomert置換完了に数えない。

## 直近の環境と再実行

`umask 022`でproject Makefileのbuild経路を使う。

- `make check-ha-conntrack DOCKER='sudo -n docker' RELEASE_ID=<release>`
- `make conntrack-engine DOCKER='sudo -n docker' CONNTRACK_ARCH=<arch>`
- `make check-health-pbr RELEASE_ID=<release>`

通信fixtureはprivate network/mount/PID namespaceとprivate `/run/netns`を使い、独立Linux端末を置く。
必要なkernel moduleは`nf_conntrack`と`nf_conntrack_netlink`。TCP looseは隔離namespaceだけで0へ設定する。
期限とkernelのPID namespace終了で子processを止め、完了したログ・capture・catalogは削除する。
host管理NIC/address/route/clock、稼働中homertの設定・serviceは変更しない。
配布に必要な対応sourceとlicenseは残す。過去の診断出力を再保存しない。

## 現行homertで確認した条件

2026-10-27に両nodeの設定とControl APIを読み取り専用で確認した。
homert02はmaster/activation Ready、03はbackup/Standby、優先度150/100・preempt=false。
主系はPD取得とPD由来DS-Lite分散経路を使い、待機系は代替経路を選ぶ。PPPは設定にない。
両系のTailscaleはRunning/online、subnet/exit-node広告、acceptDNS/acceptRoutesはfalse。
conntrackdも両系activeで、FTFW・外部cache・起動時resync・expectation同期を使う。
DHCPv4/PD同期は主系から15秒間隔でSynced。DHCPv6 serverはstatelessで、PD由来DNS/domain-searchを配り、
待機系ではWhenFalse。表示上のRunning/Syncedを、同期内容や既存TCP継続の証明とは扱わない。

実上流からの重要な知見は引き続き適用する。DHCPv6 Replyの送信元49153を受け入れ、通常DADを待つ。
RAには現在のMACを使い、元のlease期限を受信時刻で付け直さない。DS-Liteは選択FIBとAFTR往復を確認する。
以前のnative-27約130分の実上流cohortは最新ABIの長時間試験・ONU直結・物理NIC資格判定ではない。
`f1ce267`までのcontrol交換と永続ext4のfactory reboot/PD/TCP/UDP/DNS確認も、今回のCtd factory受入とは別である。

## 残作業

次はHA-02のDHCPv4貸出・PD identity/期限の引継ぎと、homertの一台分の未接続機能を進める。
stateless DHCPv6/DNS配布、Tailscale、HA-01の全readiness/phase、複数DS-Lite/代替回線と管理経路を
現行設定へ接続し、二台切替・復帰で必要な状態を引き継ぐ。HA-03の分断/再同期/古い状態と負荷も残る。
全34 work packageは計画の範囲を維持する。BGP/BFD全profile、DHCP server全pool、VRF、cluster、
cloud/SAM、権限分離、全運用surface、SSH更新・trial/fallback、OS A/B・source配布、
ARM64と対象物理機の受入は未完了。独立して進められる実装・実通信を継続する。
