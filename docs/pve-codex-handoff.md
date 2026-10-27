# routerd2 開発引き継ぎ

2026-10-27。全34 work packageのゴールは継続中。homertの本番置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を参照する。
実行順序はhomert02/03、実態を確認したk8s-rt/pve-rt、その後既存文書に基づくCloud SAM。

## 現在の変更: peer-receive / native-30

旧routerdの受理・利用経路と照合し、実通信を遮る追加制限を修正した。

- `conntrackconfig.Validate`の`IsGlobalUnicast()`を外した。同期interfaceと異なる二つのIPv4
  unicast addressを指定でき、IPv4リンクローカルも使える。未指定・multicast・limited broadcast
  は対向unicast addressとして扱わない。旧`pkg/config/validate_firewall.go`と
  `pkg/render/conntrackd.go`にはglobal-unicast制限がなかった。
- DS-Liteの`b4_address`/`b4_peer`にも同じ追加制限があり、解除した。
  旧`validateTunnelAddress`と`TunnelInterfaceController`はIPv4 prefix/addressを利用する。
- RAのframingが成立する不正PIO/MTU/RIOだけを無視する。重複RIOはwire順で更新する。
  以前はRA全体を拒否していた。旧`pkg/raobserver/raobserver.go`は観測専用であり、
  旧Linux環境の実際のRA適用者はkernelである。観測器の受理だけを旧転送の実績には数えない。
  checksum・hop limit・link-local送信元・packet境界の検査は保持する。
- DHCPv6のIA_NA/IA_PDで、不正な一割当だけを無視し、同じIAの有効割当を保持する。
  prefix外のhost bitsはmasked prefixへ正規化する。旧`pkg/pdclient/message.go`もmaskする。
  [RFC 8415 §21.6/21.22](https://www.rfc-editor.org/rfc/rfc8415.html#section-21.22)の
  不正preferred lifetimeの破棄対象は一つのaddress/prefixである。
- AFTRのAAAA集合に使えないaddressがあっても、有効候補を利用する。旧
  `cmd/routerd/runtime_network.go:resolveAAAA`と`chain/phase2.go:resolveAFTRIPv6`は
  一つの不適格候補で集合全体を捨てる処理を持たない。DNS交換の識別・CNAMEとRRset寿命は保持する。
- DHCPv4 serverは実送信元の確認後、DISCOVER/option50付き要求の残存`ciaddr`を0へ正規化する。
  旧routerdのLAN配布はdnsmasqであり、公式dnsmasq2.90 `src/rfc2131.c:150–153`にも同じ処理がある。
  これは特定端末での実観測や旧稼働dnsmasqの厳密な版を確認したという意味ではない。
- RAのDNS server/search domainが満杯なら、[RFC 8106 §6](https://www.rfc-editor.org/rfc/rfc8106.html#section-6.2)
  に従って最も早い期限を除く。32件ずつの上限を保ち、独立するdefault route更新を拒否しない。
- 正当なIPv6の大文字・省略・先頭0表記をBFD/DS-Liteの入力で正規化する。
  旧`pkg/controller/bfd/controller.go`と`phase2.go:localIPv6Address`も解析後に正規化する。
  BGPのBFD参照/local addressも同じ値へ揃え、参照検査は文字列でなくaddressで比較する。
  compilerはnative-30。新たな互換readerやadapterは持たない。

PVE Linux6.8/amd64の`peer-receive`で、既存fixtureと実product processを使用した。
二つの署名済みPID1 tree/実conntrackd、独立Linux client/server、専用`169.254.42.2/16`↔
`169.254.42.3/16`同期linkで、同じTCP socket/UDP NAT対応のA→B→A、engine SIGKILL/復旧、
設定削除時のsysctl復元が通った。captureは一つのTCP SYNとlink-local FTFWを確認し、UDP損失は0。
修正前binaryでは同じlink-local設定がbootstrapで拒否された。
不正独立RA option/重複RIOと不正IA siblingを含む実socket受信も、修正前では経路/PD取得に失敗し、
修正後はRA経路、PD再番号付け/expiry/再取得、LAN RAと実TCP/UDPが通った。
内側IPv4 link-localのDS-Liteでも、独立Linux AFTRへ実TCP/UDPを転送した。
DNS AAAAにmulticastと有効AFTRを同梱しても同じ通信が通った。
実supervisor配下のDHCP workerでは、既存IPv4を送信元と`ciaddr`に残したDISCOVER/REQUESTで
同じ貸出へのOFFER/ACKが通り、DNS反映・元期限失効・journal復旧も確認した。
RAの40 DNS server/40 search domainは32件ずつに収まり、既存default routeの1秒期限を
更新して実TCP/UDPが通った。長い期限のresolverも保持した。新DNSを先頭へ置く順序と
起動直後の空module一覧を試験観測側で誤認した先行runは失敗し、観測処理を直して再実行した。
大文字/先頭0のIPv6設定では、独立Linux転送peerを越えるBFDの障害/復旧/worker交換と、
同じendpointの二VRF/default domainでBFD→BGP→学習経路→TCP/UDPが通った。
DS-Liteの大文字/先頭0 local/resolver設定も、独立DNS/Linux AFTRと転送・期限失効が通った。

`make check`（Go/Python・Race Detector・vet）成功。`make build build-publisher`と
`make build-linux RELEASE_ID=peer-receive`でamd64/arm64の6静的Go ELFを生成した。
最後の参照正規化変更は関連packageのRace DetectorとIPv6 BFD/BGP実通信でも再確認した。
`gitleaks protect --staged --redact --no-banner`成功。ARM64の実行・物理機の受入は未実施。

これは全受信経路の監査完了ではない。DHCPv4 clientのsocket/BPF/decoderと旧実装の送信元port条件、
DHCPv6の高位送信元port受理、RAのRS/RA送信元規則、HealthとVRRPの設定/受信経路も読み比べたが、
残るDHCP server/relay、DNS、PPP、BGP/BFD、WireGuard等の全option・件数・寿命・失敗範囲は未監査。
大容量時の拒否範囲も残る。AGENTS.mdには出力/保存表記を入力拒否へ転用せず、入力と参照を
一緒に正規化する原則を一文だけ補足した。
HA-02のDHCP/PD同期、HA-03の残受入、Tailscaleと全34 packageの残作業は継続する。
raw診断・capture・一時署名catalogは試験終了後に削除する。物理NIC/ONU/負荷/電源断の資格確認ではない。

## 直前の変更: ha-conntrack / native-29

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
