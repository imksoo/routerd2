# routerd2 開発引き継ぎ

更新: 2026-10-31 04:58 UTC。最初の製品は未完成であり、[実装計画](implementation-plan.md)、[機械可読台帳](implementation-plan.json)、[試験計画](test-plan.md)、[AGENTS.md](../AGENTS.md)を基準に進める。各実通信試験の直前に試験計画の実行手順と対象項目を読み直す。対象本番の置換はしていない。

## 現在の実装

`main` は物理 DHCP ingress、Health の readiness、管理・計測の統合を含む。Tailscale は上流 Linux 標準どおり `NetfilterMode=2` と `NoSNAT=false` を用い、上流 daemon がその forwarding/SNAT 規則を所有する。通常配布物は `make build`、appliance image は `make image` で同じ `RELEASE_ID` を明示して作る。`routerd` と `routerd-logging` は CGO0、承認済みの `routerd-modules` だけが CGO1 静的リンクである。

DHCP HA は VRRP が配布役を選び、dnsmasq に VIP と物理 ingress を明示する。旧 routerd の実績に従い、物理 ingress と VIP の対応を dnsmasq の `bridge-interface` で表す。HA DHCP listener と同じ物理 UDP/67 を別 service が共有する構成は、実 kernel の SO_REUSEPORT 配送で更新先が不定になるため拒否する。

## 実通信で確認した範囲

- `health-ready-native105`: 独立二 WAN namespace peer、実 supervisor/control/network/Health worker で Health→PBR の障害切替・復旧、worker と supervisor の再起動、古い通知・期限切れの拒否、confirmed rollback、TCP/UDP と packet capture を確認した。
- `dhcp-ingress-native105`: 隔離 PVE の appliance で、通常・特殊 DHCPv4 client が 120 秒 lease の自然 T1 に VIP `172.18.0.1:67` 宛て unicast REQUEST を送り、同じ XID の ACK と option 54 を受信した。cold boot/reboot 後の DNS UDP/TCP（IPv4/IPv6）、TCP/UDP、BGP、NTP、Tailscale も通過した。
- `ops-native106`: 隔離 Linux namespace の二台 HA で、通常・特殊 DHCPv4、旧 lease と元期限の取込、片系停止中の新規端末、取得 DNS UDP/TCP、IPv4 TCP/UDP、両方向切替と復帰後の lease 同期を確認した。ここでの更新は明示的に促したものである。
- `ops-tailnet-defaults-native111`: 隔離 PVE の二台 full-profile appliance と独立 Headscale/公式 Tailscale peer で、各機を IPv4/IPv6 exit node にした TCP/UDP、LAN 双方向通信、direct/relay、service 撤去・再適用、worker/PID 1 再起動を確認した。停止した同じ exit 選択は再起動後に 6.386 秒と 4.938 秒で回復した。停止中の DNS/UDP 損失と初期3失敗は記録し、無停止・無損失とは扱わない。

これらは実プロセス・独立 peer・Linux kernel/PVE の相互通信結果である。対象 ONU/HGW/NIC、対象 appliance、実回線、43,200 秒 lease の自然 T1、全34 work package の受入を表さない。

## 直近の実行

current HEAD の release build は成功している。元の 43,200 秒 DHCPv4 lease を通常・特殊 client で実時間観測中である。両者について、実際に受けた lease/T1 を使い、T1 の unicast REQUEST、同一 XID ACK、option 54、期限延長、REBIND がないこと、取得 DNS と IPv4/IPv6 通信の継続を確認する。capture は T1 周辺だけに限定し、判定後に削除する。

署名更新の不良 candidate から自動で known-good runtime へ戻る PVE 試験も、DHCPv6 PD の再起動後 Rebind を packet capture で判定する形に更新して再実行中である。PD Rebind と正常 candidate の確認は済んだが、自動 rollback 全経路はまだ合格に数えない。

自然更新コホートは故障注入や設定交換に転用しない。故障・切替は別 stage で行う。終了済みの native93 と native95 は現在のコホートではなく、前者は終了1、後者は終了0である。過去の個別結果を current HEAD や実機の受入へ流用しない。

## 残る受入

1. homert02/03 の単体構成で、独立端末が DHCPv4/DHCPv6/SLAAC、DNS（条件付き forwarder と DoH を含む）、IPv4 NAT、IPv6 native forwarding、WAN DHCPv4/PD、RA と自然更新を使ってインターネットへ到達すること。
2. 二 WAN の Health/PBR、PPPoE、DS-Lite、firewall、既存 TCP と UDP mapping を実回線条件で確認すること。
3. 二台 HA の長時間 lease、PD、DHCPv6、VRRP、conntrack、障害と復帰を対象構成で受け入れること。
4. 更新・復旧、対象 appliance/NIC/ONU、ARM、PVE/Kubernetes/Cloud の各環境をそれぞれ受け入れること。

稼働中の試験資源だけを操作対象から外す。終了済み stage、生 capture、重複 image、詳細 log は結果と再現手順を短く記録後に削除する。秘密を含む実入力・鍵・identity・pcap・インフラ詳細は公開しない。
