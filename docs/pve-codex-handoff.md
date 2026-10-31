# routerd2 開発引き継ぎ

更新: 2026-10-31 09:01 UTC。最初の製品は未完成であり、[実装計画](implementation-plan.md)、[機械可読台帳](implementation-plan.json)、[試験計画](test-plan.md)、[AGENTS.md](../AGENTS.md)を基準に進める。各実通信試験の直前に試験計画の実行手順と対象項目を読み直す。対象本番の置換はしていない。

## 現在の実装

`main` は物理 DHCP ingress、Health の readiness、管理・計測の統合を含む。Tailscale は上流 Linux 標準どおり `NetfilterMode=2` と `NoSNAT=false` を用い、上流 daemon がその forwarding/SNAT 規則を所有する。通常配布物は `make build`、appliance image は `make image` で同じ `RELEASE_ID` を明示して作る。`routerd` と `routerd-logging` は CGO0、承認済みの `routerd-modules` だけが CGO1 静的リンクである。

DHCP HA は VRRP が配布役を選び、dnsmasq に VIP と物理 ingress を明示する。旧 routerd の実績に従い、物理 ingress と VIP の対応を dnsmasq の `bridge-interface` で表す。HA DHCP listener と同じ物理 UDP/67 を別 service が共有する構成は、実 kernel の SO_REUSEPORT 配送で更新先が不定になるため拒否する。

## 実通信で確認した範囲

- `health-ready-native105`: 独立二 WAN namespace peer、実 supervisor/control/network/Health worker で Health→PBR の障害切替・復旧、worker と supervisor の再起動、古い通知・期限切れの拒否、confirmed rollback、TCP/UDP と packet capture を確認した。
- `dhcp-ingress-native105`: 隔離 PVE の appliance で、通常・特殊 DHCPv4 client が 120 秒 lease の自然 T1 に VIP `172.18.0.1:67` 宛て unicast REQUEST を送り、同じ XID の ACK と option 54 を受信した。cold boot/reboot 後の DNS UDP/TCP（IPv4/IPv6）、TCP/UDP、BGP、NTP、Tailscale も通過した。
- `pd-rebind-pve`: 隔離 PVE（4 CPU/4 GiB）の update/reboot で ISC DHCPv6 を appliance reboot 前に停止し、新しい PPP を観測後に再起動した。WAN capture の最初の復帰 PD 交換は Rebind で Solicit なし、初回観測の DUID/IAID/prefix と一致した。RA withdrawal/PADT、署名更新、明示 rollback、Health 障害による候補自動 rollback、復帰後の LAN DNS と IPv4/IPv6 TCP/UDP まで終了0で確認した。
- `ops-native106`: 隔離 Linux namespace の二台 HA で、通常・特殊 DHCPv4、旧 lease と元期限の取込、片系停止中の新規端末、取得 DNS UDP/TCP、IPv4 TCP/UDP、両方向切替と復帰後の lease 同期を確認した。ここでの更新は明示的に促したものである。
- `ops-tailnet-defaults-native111`: 隔離 PVE の二台 full-profile appliance と独立 Headscale/公式 Tailscale peer で、各機を IPv4/IPv6 exit node にした TCP/UDP、LAN 双方向通信、direct/relay、service 撤去・再適用、worker/PID 1 再起動を確認した。停止した同じ exit 選択は再起動後に 6.386 秒と 4.938 秒で回復した。停止中の DNS/UDP 損失と初期3失敗は記録し、無停止・無損失とは扱わない。
- `sam-enrollment-native`: 隔離 Linux namespace で、独立した旧 routerd RR と current routerd2 の supervisor/control/SAM worker を接続した。署名済み claim の admission と control status の RR/direct snapshot、control API 経由 worker 再起動後の新 producer、RR 停止中の元期限を延長しない RR fallback、RR 復帰後の revoke と旧 RR による拒否を確認した。これは CLOUD-03 の component contract に限る。WG/BGP、/32、NAT なし端末通信、packet capture、CLOUD-04 は未確認である。
- `sam-transport-release`: 同じ current product stack を独立旧 routerd RR と FRR に接続し、admitted dynamic RR endpointからIPIP/BGPを構成。root Linux netns内の独立端末間でNATなしTCP/UDP双方向通信とpacket captureを確認した。明示revoke試験および元admission期限切れ試験の双方でtunnel linkとkernel FIB routeが撤去され、転送が停止した。通常・期限切れとも実通信試験終了0。WG暗号化FOU、/32 mobility/provider action、leaf/drain/分断復旧、対象PVE・実機・クラウドではない。
- `sam-wireguard-final-release`: 実routerd2と独立旧 routerd RR/FRRでSAMからWG peerとIPIP/BGPを作り、WG handshake、NATなしTCP/UDP双方向通信を確認。明示revokeと元期限の自然失効でtunnel/FIB route撤去と通信停止を確認した。失効試験中に再投影ごとに共有Firewall設定へruleを積み増し、2 MiB control API上限に達してBGP workerを停止させる問題を検出・修正した。正常・revoke・期限切れは修正版同一release IDで通過。FOU、/32 mobility/provider action、planned leaf/drain、partition recovery、対象PVE/実機/cloudは未受入。
- `sam-mobility-candidate`: claimが所有する`203.0.113.51/32`をleaf BGPから独立FRRへ広告し、独立RRの明示revokeと元admission期限切れの両方で撤回する経路を追加・確認。IPIP/WireGuard、NATなしTCP/UDPとkernel撤去を含む4実通信ケースが通過し、`make check`も終了0。単一claimの経路広告までで、所有権移動・複数leaf・provider capture/drain・partition recovery・PVE/実機/cloudは未確認。
- `homert-disk-native113`: 通常 APK から作る BIOS disk を二台の隔離 PVE appliance で VirtIO/SCSI に接続し、通常の `DISK_ETC_DIR` 入力で設定と trust file を配備した。取得 DHCP/SLAAC/DNS、IPv4/IPv6 TCP/UDP、BGP、DS-Lite、NTP、Tailscale と電源断後の disk 再起動を確認した。切替中には通信損失を観測しており、これは disk boot/provisioning/recovery の受入であって無停止 HA の受入ではない。

これらは実プロセス・独立 peer・Linux kernel/PVE の相互通信結果である。対象 ONU/HGW/NIC、対象 appliance、実回線、43,200 秒 lease の自然 T1、全34 work package の受入を表さない。

## main統合と保存設定復元

管理PR #1（`4fcdb89`まで）、選択済み初期状態のディスク入力、HA観測保持修正 `30eaa27` を統合した作業版 `main-management-sam-126` で、管理2ケースとSAM4ケースが実daemon/独立peerで終了0。SAM claimの固定所有経路は設定準備時に一度だけ生成し、観測更新ごとのJSON再読込み・固定アドレス解析を除いた。3配布物の通常buildと静的リンクも確認した。

全57設定の別PVE受入版 `ops-management-integrated-native125` / `4fcdb89` は、DNS変更と保存設定復元の前後で同じPD worker・prefix・元期限を保持した。復元から取得DNS・IPv4/IPv6 TCP/UDP・BGP・特殊端末・Tailscaleの確認まで3.299秒、続く全体再起動後の復帰も終了0。修正前の現用HA版観測の破棄はこの条件で再現済み。専用VMは撤去し、自然更新の版120は変更していない。対象接続先、既存状態の移行、実測負荷、k8s/pve差分とCloud SAMは継続する。

main統合版126のハッシュ一致3実行物でも、通常125の起動イメージを基底にした全57設定のPVE復元・全体再起動が終了0。PD worker・prefix・元期限を保持し、復元から全通信確認まで4.305秒だった。新規126イメージ全体の受入とは区別する。専用VMは撤去済み。

## 直近の実行

統合候補 `20ea125` / `ops-management-integrated-native123` では、管理機能六経路の統合と隔離実通信、二台の通常APK/BIOSディスクによる双方向電源断切替・再起動が終了0だった。既存TCPを維持し、新規端末の取得DNSとIPv4・IPv6通信も復旧した。UDPは1,763送信中233欠落、DNSは1,017問い合わせ中17失敗であり、無損失ではない。詳細と限界は [試験計画](test-plan.md) の統合候補版の節に記録した。GitHubの `main` ブランチへの取り込みと対象実機の受入は別に残る。

同じ候補版123への署名付き更新、120への明示切戻し、正常性確認失敗による自動復旧も隔離PVEで終了0だった。通信確認までそれぞれ36.278秒、34.725秒、137.304秒で、最後は90秒の試行期限を含む。設定と既存の識別情報を保持した。公開済みSAM修正 `c85c7a1` を取り込んだ `79813e7` は関連パッケージの競合検査・静的検査が終了0であり、候補版123の実通信結果とは区別する。

本番対象は各4仮想CPU・4 GiBのPVE仮想機械である。次の対象受入は本番接続先、Ubuntu/Linux環境、既存状態の移行、実測負荷との差分を確認する。新しい物理機器の要件をhomert置換の前提へ追加しない。

自然更新の現行環境は、版 `ops-management-combined-native120` を使う専用ジョブである。最初のACKは通常端末16:37:31.917 JST、特殊端末16:37:32.409 JSTで、元の43,200秒リース・21,600秒のT1を維持する。判定予定は22:39:02 JST。旧版108の観測経路は中断しており、その実行を待機中・合格とは扱わない。稼働中の二つの専用環境を変更しない。

元の 43,200 秒 DHCPv4 lease を通常・特殊 client で実時間観測中である。両者について、実際に受けた lease/T1 を使い、T1 の unicast REQUEST、同一 XID ACK、option 54、期限延長、REBIND がないこと、取得 DNS と IPv4/IPv6 通信の継続を確認する。capture は T1 周辺だけに限定し、判定後に削除する。

DHCPv6 PD の再起動後 Rebind と、不良 candidate から known-good runtime へ自動 rollback する PVE 経路は完了した。capture と一時 stage は削除済みであり、これは対象 ONU/HGW や実回線の受入ではない。

自然更新コホートは故障注入や設定交換に転用しない。故障・切替は別 stage で行う。終了済みの native93 と native95 は現在のコホートではなく、前者は終了1、後者は終了0である。過去の個別結果を current HEAD や実機の受入へ流用しない。

## 残る受入

1. homert02/03 の単体構成で、独立端末が DHCPv4/DHCPv6/SLAAC、DNS（条件付き forwarder と DoH を含む）、IPv4 NAT、IPv6 native forwarding、WAN DHCPv4/PD、RA と自然更新を使ってインターネットへ到達すること。
2. 二 WAN の Health/PBR、PPPoE、DS-Lite、firewall、既存 TCP と UDP mapping を実回線条件で確認すること。
3. 二台 HA の長時間 lease、PD、DHCPv6、VRRP、conntrack、障害と復帰を対象構成で受け入れること。
4. RR/leaf の実 WG/BGP、/32所有、NAT なし端末通信、移動・drain・revoke・capture を確認すること。
5. 更新・復旧、対象 appliance/NIC/ONU、ARM、PVE/Kubernetes/Cloud の各環境をそれぞれ受け入れること。

稼働中の試験資源だけを操作対象から外す。終了済み stage、生 capture、重複 image、詳細 log は結果と再現手順を短く記録後に削除する。秘密を含む実入力・鍵・identity・pcap・インフラ詳細は公開しない。
