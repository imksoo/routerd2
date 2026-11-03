# routerd2 開発引き継ぎ

更新: 2026-11-03 01:27 UTC。main `646ee1c`由来buildでBGPを含む22-resource native profileが実HGW/独立FRR・client gateを通過。Goコード不変、今回のPython checks/static build/設定検証も通過。全homert、HA、対象hardwareは継続中。

## 現在の実作業

22-resource native profile、main由来build `646ee1c19db9`で実HGW統合gateが終了0。独立FRR 8.4.4/ZebraとのBGPで、peerの203.0.113.0/24とLAN198.18.0.0/24を相互広告し、両kernelのpeer next hopとFRRの受信RIBを確認した。両LAN clientが取得したsource IPv4のままTCP/UDPを往復し、既存captureでもBGP交換とNATなしの転送を確認した。全daemon restart後に同じ経路・両client通信が復旧。管理DNS/SSH/Web履歴・NTP（WAN学習源stratum 3）と両IP Internet通信も維持した。PD worker SIGKILL/通常再起動後の端末復旧2.81/4.97秒、full restartから全確認完了17.91秒（NTP client測定込み）。2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。自然T1・連続TCP・二BGP peers全体・長時間GR・HA・全homert・ONU直結/対象NICは今回未受入。stage/capture/FRR runtime/namespaceは撤去済み。

19-resource native profile（基準HEAD `f64580e` + SSH修正、実行build ID `f64580e932b2`）の実HGW gateは終了0。loopback/管理/LAN両IP DNS、上流停止中のcache応答、未取得名のfailure isolationと復旧、query log API/Web閲覧、管理端末から鍵認証SSH、LANからWebへの遮断を確認した。SSHはaddress設定前から指定addressへ待受し初回generation=1/restarts=0。全daemon再起動後も同じhost keyでSSH接続でき、保存済みDNS履歴を閲覧できた。WAN学習NTP源stratum 3と両LAN chronyd client応答も通過。DHCP/PD/RA/SLAAC/DS-Lite/IPv6 HTTPS・DNS UDP/TCPを2 round、PD worker SIGKILL/通常再起動後3.10/4.94秒で復旧した。全daemon restartから管理/NTPを含む全確認完了17.63秒はclient測定時間を含む。zero-lifetime RA/kernel withdrawal、upstream Releaseなしを確認。自然T1、連続TCP、全homert/HA、ONU直結・対象NICは今回未受入。stage/capture/一時鍵/namespaceは撤去済み。

- 実HGWに接続した実daemonと独立Linux端末による自然DHCPv6-PD T1試験は終了0。自然Renew/Reply、UDP/49153からのreply、DUID/IAID/prefixの維持、lease期限更新をcaptureで確認した。normal DAD、PD worker/daemon再起動、zero-lifetime RA、端末IPv4/IPv6/DNS通信も通過し、capture/state/stageは削除済み。条件と限界は[実通信計画](test-plan.md)に記録した。
- NTPを含む16-resource gateは終了0。WAN学習NTP源（stratum 3）への同期とLAN 2 clientの時刻応答、daemon全体restart後の再同期・応答を確認した。conntrack TCP liberal=1も実kernelで確認。既存DHCP/DNS/両IP Internet経路を維持した。詳しい条件・限界は[実通信計画](test-plan.md)にある。
- ユーザー提供のhomert02/03設定を値・秘密を出さずに読取専用で再集計した。両方110 resources、各kind countは一致。IPv6 host-traffic policy route（table 120）とWAN `/128` DS-Lite sourceの運用を確認した。
- native例はhomert02/03各14 resources、single-routerはBGPを含む22 resources。110-resource設定全体の受入ではない。

## 次に行うこと

1. 次は旧routerdのTailscaleNode運用をnative単体設定へ接続する。次の実通信直前に `docs/test-plan.md` 全体を読み、homert02/03の110-resource intentと旧routerd実装を機能・caller単位で照合し、単体native profileへ全体機能を移す。converterや秘密入り設定は作らない。
2. full single profileを実HGW・独立Linux clientでcold start、通常通信、障害、worker/daemon restart、自然renewalまで受け入れる。
3. 一つのHGW gateが長引いても、独立するwork packageの棚卸しと実装を続ける。現在の実HGW/隔離Linuxでhomert単体、二台HA、残るprotocol/management/distributionを進める。今回PVE/QEMUと追加のPBR調査は実行しない。

## 到達している範囲と制限

`make check`、Race Detector、vet、build、隔離PVEのappliance/update gate、実supervisor/control/network/Health workerによる二WAN Health→PBR gateは[実通信計画](test-plan.md)に記録した。これらは110-resource homert全体や対象ハードウェアの受入ではない。

この実行hostの `ens18`/`ens20` はvirtio NIC。実HGW接続はあるが、NTT ONU直結・対象物理NIC qualificationは未実施。2026-10-24 11:28:25 UTCの開始から01:20 UTC時点で9日13時間51分、記録済み期限を4日8時間23分超過している。根拠のない新しい完成日は提示しない。

コミット前に関連する `make check`、Race Detector、vet、buildを実行する。各実通信後にcapture/log/stageを片付け、不要なorphan process・worktree・build outputを残さない。
