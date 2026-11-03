# routerd2 開発引き継ぎ

更新: 2026-11-03 00:59 UTC。実行版の基準mainは `30006ea`。16-resource native profileで実HGW/独立clientのNTP・DNS・両IP通信とrestart gateが終了0。`make check`（Race Detector・vet込み）、static buildと設定検証を通過。34 work packageは未完了で、全110-resource homertと対象hardwareは未受入。

## 現在の実作業

- 実HGWに接続した実daemonと独立Linux端末による自然DHCPv6-PD T1試験は終了0。自然Renew/Reply、UDP/49153からのreply、DUID/IAID/prefixの維持、lease期限更新をcaptureで確認した。normal DAD、PD worker/daemon再起動、zero-lifetime RA、端末IPv4/IPv6/DNS通信も通過し、capture/state/stageは削除済み。条件と限界は[実通信計画](test-plan.md)に記録した。
- NTPを含む16-resource gateは終了0。WAN学習NTP源（stratum 3）への同期とLAN 2 clientの時刻応答、daemon全体restart後の再同期・応答を確認した。conntrack TCP liberal=1も実kernelで確認。既存DHCP/DNS/両IP Internet経路を維持した。詳しい条件・限界は[実通信計画](test-plan.md)にある。
- ユーザー提供のhomert02/03設定を値・秘密を出さずに読取専用で再集計した。両方110 resources、各kind countは一致。IPv6 host-traffic policy route（table 120）とWAN `/128` DS-Lite sourceの運用を確認した。
- native例はhomert02/03各14 resources、single-routerはNTPを含む16 resources。110-resource設定全体の受入ではない。

## 次に行うこと

1. 次の実通信直前に `docs/test-plan.md` 全体を読み、homert02/03の110-resource intentと旧routerd実装を機能・caller単位で照合し、単体native profileへ全体機能を移す。converterや秘密入り設定は作らない。
2. full single profileを実HGW・独立Linux clientでcold start、通常通信、障害、worker/daemon restart、自然renewalまで受け入れる。
3. 一つのHGW gateが長引いても、独立するwork packageの棚卸しと実装を続ける。現在の実HGW/隔離Linuxでhomert単体、二台HA、残るprotocol/management/distributionを進める。今回PVE/QEMUと追加のPBR調査は実行しない。

## 到達している範囲と制限

`make check`、Race Detector、vet、build、隔離PVEのappliance/update gate、実supervisor/control/network/Health workerによる二WAN Health→PBR gateは[実通信計画](test-plan.md)に記録した。これらは110-resource homert全体や対象ハードウェアの受入ではない。

この実行hostの `ens18`/`ens20` はvirtio NIC。実HGW接続はあるが、NTT ONU直結・対象物理NIC qualificationは未実施。2026-10-24 11:28:25 UTCの開始から00:59 UTC時点で9日13時間31分、記録済み期限を4日8時間03分超過している。根拠のない新しい完成日は提示しない。

コミット前に関連する `make check`、Race Detector、vet、buildを実行する。各実通信後にcapture/log/stageを片付け、不要なorphan process・worktree・build outputを残さない。
