# routerd2 開発引き継ぎ

更新: 2026-11-01 10:00 JST。最初の製品は未完成であり、[実装計画](implementation-plan.md)、[機械可読台帳](implementation-plan.json)、[試験計画](test-plan.md)、[AGENTS.md](../AGENTS.md)を基準に進める。各実通信試験の直前に試験計画の実行手順と対象項目を読み直す。対象本番の置換はしていない。

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

`ops-management-combined-native120` は通常・特殊端末の元の43,200秒 leaseについて、T1（21,600秒）でのVIP宛unicast REQUEST、同一XID・option 54を持つACK、期限延長、REBINDなしを実packetで確認した。取得済みDNSと両family通信も直後に通った。その後6時間休止していたTailscale経路の最初の新規TCPは、試験側の固定2秒待機で一度timeoutした。failure snapshotはTailscale Running/online、相手peer active、承認済み経路と転送規則を示すが、続く実通信を測らなかった。このためDHCP自然更新は成功、Tailscaleの長時間継続は未合格として分ける。生ログ、鍵、capture、stageはこの記録後に削除する。

これらは実プロセス・独立 peer・Linux kernel/PVE の相互通信結果である。対象 ONU/HGW/NIC、対象 appliance、実回線、全34 work package の受入を表さない。

## main統合と保存設定復元

管理PR #1（`4fcdb89`まで）、選択済み初期状態のディスク入力、HA観測保持修正 `30eaa27` を統合した作業版 `main-management-sam-126` で、管理2ケースとSAM4ケースが実daemon/独立peerで終了0。SAM claimの固定所有経路は設定準備時に一度だけ生成し、観測更新ごとのJSON再読込み・固定アドレス解析を除いた。3配布物の通常buildと静的リンクも確認した。

全57設定の別PVE受入版 `ops-management-integrated-native125` / `4fcdb89` は、DNS変更と保存設定復元の前後で同じPD worker・prefix・元期限を保持した。復元から取得DNS・IPv4/IPv6 TCP/UDP・BGP・特殊端末・Tailscaleの確認まで3.299秒、続く全体再起動後の復帰も終了0。修正前の現用HA版観測の破棄はこの条件で再現済み。専用VMは撤去し、自然更新の版120は変更していない。対象接続先、既存状態の移行、実測負荷、k8s/pve差分とCloud SAMは継続する。

main統合版126のハッシュ一致3実行物でも、通常125の起動イメージを基底にした全57設定のPVE復元・全体再起動が終了0。PD worker・prefix・元期限を保持し、復元から全通信確認まで4.305秒だった。新規126イメージ全体の受入とは区別する。専用VMは撤去済み。

公開は `25e7cf9`（PR #1統合済み）。PVEで固定した126版の変更対象Goソース29件・埋込みHTMLと一致し、試験した3実行物のハッシュも照合した。通常APKから新規作成した同じ126版の二台BIOSディスクも、実電源断・再参加の両方向が終了0。全通信復旧25.881/16.010秒、同じTCP接続とNATを保持して最大応答待ち7.196秒。UDP1,675送信中226期限切れ、DNS951件中20失敗。元60秒/15秒を維持した。旧主系不在の新規自動端末も取得DNSと両IP通信を利用でき、同じTailscale exit選択も再起動後に復旧した。両試験VMは撤去済み。実状態移行・実経路負荷・対象WAN/LANを閉じてからk8s/pve差分、Cloud SAMへ進む。

両homertのTailscale1.104.2と保存先`/var/lib/tailscale/tailscaled.state`（root:root、0600通常ファイル）は読取専用メタデータで確定した。状態本文は未取得で、実状態移行の合格には数えない。製品/VMを除く同じSSH Ethernet伝送自体の対照は102.259/131.067 Mbpsであり、旧候補の130〜149 Mbpsは製品上限の証拠ではない。

2026-11-01: 通常126 APKから起動した隔離PVEの4CPU/4GiB実VMと、同じPVE内の独立LAN/WAN端末を直接接続した。SSHは管理・結果回収だけで、SSH転送に使える二つの別リンクでTCP/5201 packetが0なのをcaptureした。最小の静的IPv4 NAT/IPv6転送構成で15秒単一TCPは、IPv4 NAT受信1.275/1.280Gbps、IPv6受信1.228/1.243Gbpsだった。相手はIPv4でNAT後の`192.168.1.249`、IPv6で元のLAN端末を観測した。このため旧130〜149Mbpsを製品上限とする根拠はなくなった。DS-Lite、DHCP/SLAAC/DNS、HA、対象回線・NICの負荷合格には流用しない。VM、raw disk、capture、パッケージとremote stageは削除済み。

今回の生成物・ログ等823.2MiBと専用Docker imageに続き、終了した二台試験のstage約612MiB（capture・一時鍵・state・disk）を削除した。main統合・未変更・稼働参照なしを確認した完了済みworktree二つも `git worktree remove` で撤去し、約1,012MiBの重複出力を解放した。稼働参照がある別tree、次の受入に使う126版image/runnerと既存`AGENTS.md`変更は保持した。native120の根拠を上記へ圧縮した後、同じくstageを削除する。空きは約38GiB。

## 直近の実行

統合候補 `20ea125` / `ops-management-integrated-native123` では、管理機能六経路の統合と隔離実通信、二台の通常APK/BIOSディスクによる双方向電源断切替・再起動が終了0だった。既存TCPを維持し、新規端末の取得DNSとIPv4・IPv6通信も復旧した。UDPは1,763送信中233欠落、DNSは1,017問い合わせ中17失敗であり、無損失ではない。詳細と限界は [試験計画](test-plan.md) の統合候補版の節に記録した。GitHubの `main` ブランチへの取り込みと対象実機の受入は別に残る。

同じ候補版123への署名付き更新、120への明示切戻し、正常性確認失敗による自動復旧も隔離PVEで終了0だった。通信確認までそれぞれ36.278秒、34.725秒、137.304秒で、最後は90秒の試行期限を含む。設定と既存の識別情報を保持した。公開済みSAM修正 `c85c7a1` を取り込んだ `79813e7` は関連パッケージの競合検査・静的検査が終了0であり、候補版123の実通信結果とは区別する。

本番対象は各4仮想CPU・4 GiBのPVE仮想機械である。次の対象受入は本番接続先、Ubuntu/Linux環境、既存状態の移行、実測負荷との差分を確認する。新しい物理機器の要件をhomert置換の前提へ追加しない。

自然更新の専用job `ops-management-combined-native120` は終了した。通常端末は16:37:31.917 JST、特殊端末は16:37:32.409 JSTに、元の43,200秒lease・21,600秒T1のunicast REQUESTをVIPへ送り、同一XIDとoption 54を持つACKを受け、期限を延長した。job全体は後続Tailscale TCP timeoutのため終了1であり、Tailscale長時間継続の合格には数えない。

DHCPv6 PD の再起動後 Rebind と、不良 candidate から known-good runtime へ自動 rollback する PVE 経路は完了した。capture と一時 stage は削除済みであり、これは対象 ONU/HGW や実回線の受入ではない。

自然更新コホートは故障注入や設定交換に転用しない。故障・切替は別 stage で行う。終了済みの native93 と native95 は現在のコホートではなく、前者は終了1、後者は終了0である。過去の個別結果を current HEAD や実機の受入へ流用しない。

## 残る受入

1. homert02/03 の単体構成で、独立端末が DHCPv4/DHCPv6/SLAAC、DNS（条件付き forwarder と DoH を含む）、IPv4 NAT、IPv6 native forwarding、WAN DHCPv4/PD、RA と自然更新を使ってインターネットへ到達すること。
2. 二 WAN の Health/PBR、PPPoE、DS-Lite、firewall、既存 TCP と UDP mapping を実回線条件で確認すること。
3. 二台 HA の長時間 lease、PD、DHCPv6、VRRP、conntrack、障害と復帰を対象構成で受け入れること。
4. RR/leaf の実 WG/BGP、/32所有、NAT なし端末通信、移動・drain・revoke・capture を確認すること。
5. 更新・復旧、対象 appliance/NIC/ONU、ARM、PVE/Kubernetes/Cloud の各環境をそれぞれ受け入れること。

稼働中の試験資源だけを操作対象から外す。終了済み stage、生 capture、重複 image、詳細 log は結果と再現手順を短く記録後に削除する。秘密を含む実入力・鍵・identity・pcap・インフラ詳細は公開しない。
