# routerd2 開発引き継ぎ

更新: 2026-11-01 17:36 JST。最初の製品は未完成であり、[実装計画](implementation-plan.md)、[機械可読台帳](implementation-plan.json)、[試験計画](test-plan.md)、[初回状態移行手順](initial-state-migration.md)、[AGENTS.md](../AGENTS.md)を基準に進める。各実通信試験の直前に試験計画の実行手順と対象項目を読み直す。対象本番の置換はしていない。

## 現在の実装

`main` は物理 DHCP ingress、Health の readiness、管理・計測の統合を含む。Tailscale は上流 Linux 標準どおり `NetfilterMode=2` と `NoSNAT=false` を用い、上流 daemon がその forwarding/SNAT 規則を所有する。通常配布物は `make build`、appliance image は `make image` で同じ `RELEASE_ID` を明示して作る。`routerd` と `routerd-logging` は CGO0、承認済みの `routerd-modules` だけが CGO1 静的リンクである。

DHCP HA は VRRP が配布役を選び、dnsmasq に VIP と物理 ingress を明示する。旧 routerd の実績に従い、物理 ingress と VIP の対応を dnsmasq の `bridge-interface` で表す。HA DHCP listener と同じ物理 UDP/67 を別 service が共有する構成は、実 kernel の SO_REUSEPORT 配送で更新先が不定になるため拒否する。

## 実通信で確認した範囲

`6cd2404` では、同期断中に待機側だった DHCP owner が昇格しても、元ownerからmTLSで現在のlease stateを受け取るまでdnsmasqを起動しないようにした。current mainで`make check-ha-dhcp-failover`と`make check-ha-state`を実行し、独立Linux namespaceの二台、実supervisor/control/network/DHCP/VRRP/Health worker、ISC DHCPv6、AFTR、独立TCP/UDP peerとpacket captureで、遮断中に配布しないこと、再同期後の重複しない新規lease、既存leaseの更新、DHCP worker再起動後の更新、通常の両方向HA handoffを確認した。通常handoffではUDP 2,992送信中69期限切れ、最大無応答1.109秒、同一TCPの最大応答待ち1.660秒だった。無損失とは扱わない。stage、capture、鍵、namespaceは終了後に削除した。これはPVE二VM、homert実設定、対象ONU/HGW/NICの受入ではない。

2026-11-01 current `1a3843b`: `make check`（Race Detector・vet を含む）、静的リンクした3実行物の build を完了した。新規4 CPU/4 GiB PVE VMを既存bridge/NICなしで起動し、現行homert03設定を隔離用に変換した実daemonで、Tailscaleの保護状態初期化まで確認した。最初の拒否は試験overlayが親`/etc`を`0775`で作ったことによるもので、製品imageの`/etc`は`0755`だった。overlayを`0755`/`0700`で作り直した再試行では同じ拒否は起きなかった。VM、設定、鍵、disk、serial logは削除済みである。これはWAN/LAN、DNS、PD、転送またはreadyの合格ではない。

同じ`1a3843b`で`make check-health-pbr`を実行し、独立Linux peerと実supervisor/control/network/Health workerによる二WANの`TestDaemonConnectionMarkedEgressPolicy`、`TestDaemonHealthPBRFailoverAndRecovery`、`TestDaemonEgressRouteHealthyMultipath`が通過した。各試験はTCP/UDP、packet capture、Health worker復旧、失効とconfirmed rollbackを対象にする。PVEのfull homert03通信受入や実ONUの資格には流用しない。raw log/capture/stageは保持しない。

同じcurrent buildでDHCP/DNS実daemon suiteを再実行し、独立BusyBox clientによる`TestDaemonImportedDHCPLeases`、`TestDaemonImportedDHCPStickyHistory`、`TestDaemonDHCPFingerprints`、`TestDaemonMigratedDHCPUnmanagedLAN`、`TestDaemonMigratedDHCPLogs`、`TestDaemonDHCPDNSAndOwnershipExpiry`、`TestDaemonDHCPDNSWithDelayedNetworkObservation`、`TestDaemonDHCPDNSWithDelayedStatePublication`、`TestDaemonMigratedDHCPDNSAndOwnershipExpiry`が通過した。これは独立clientを使う単体router経路のDHCPv4/DNS確認であり、PVE full profile、DHCPv6/SLAAC、DoH、NATまたは実回線の合格ではない。raw log/stageは削除する。

`fff1e29`では`make check-dhcp6`も実行し、独立dnsmasq/radvd/DHCPv6 peerとの`TestDaemonDHCP6RAIndependentPeers`と`TestDaemonDHCP6DuplicateAddressDecline`が通過した。DUID、RA、PDを含むworker交換を確認するが、PVE full profileや実HGW/ONUの受入ではない。raw log/stageは削除する。

`d55486d`では`make check-dslite-pmtu`を実行した。独立PPPoE AC、AFTR、LAN/WAN peerを使い、DS-Lite確立、IPv4 NATと復路、1 MiB IPv4/IPv6 TCPのupload/download、MSS、IPv4 ICMP fragmentation-needed、同一UDP socketの1454→1400 PMTU学習、IPv6 ICMP Packet Too Bigと後続1200-byte UDPを確認した。これはLinux namespace内の対向であり、物理ONU/NIC/switchのMTUやPMTUは証明しない。

同じ`d55486d`で`make check-tailscale-homert`を実行した。独立Headscaleと公式Tailscale peerで、IPv4/IPv6 LAN・subnet・exitのTCP/UDP、実経路とSNAT、保存identity、worker/PID 1再起動、service撤去/再適用、peer再起動、認証失効後の遮断、packet captureを通過した。PVE/実回線のTailscale受入には流用しない。各試験のraw log、capture、鍵、stageは削除済みである。

- `health-ready-native105`: 独立二 WAN namespace peer、実 supervisor/control/network/Health worker で Health→PBR の障害切替・復旧、worker と supervisor の再起動、古い通知・期限切れの拒否、confirmed rollback、TCP/UDP と packet capture を確認した。
- `dslite-pmtu-main-1e317d4`: 本番NICから隔離した一台 Linux namespace の独立 PPPoE/DS-Lite 対向で、IPv4 DF の ICMP fragmentation-needed、同一 UDP socket の1454→1400 PMTU学習、NAT後1368-byte payload/replyを終了0で確認した。対象WAN/NICのPMTU・物理負荷受入ではない。
- `hgw-short-main-20bc21c`: 実HGWへ隔離 macvlan を接続した current main と独立LAN端末二台で、DHCPv4/DHCPv6 information/SLAAC、委譲RDNSS/search domain、IPv4 DS-Lite/IPv6 HTTPS、学習DNS UDP/TCP、条件付きforwarder停止/復帰、WAN blackhole復帰、PD workerのSIGKILL/正常再起動、全daemon再起動後の通信を確認した。続く7,800秒観測は自然PD Renew/Reply 1回、二台の自然DHCP更新、DUID/IAID/prefix保持、zero-lifetime RA withdrawal、726周回のIPv4/IPv6 HTTPSと学習DNS UDP/TCPを終了0で確認した。ONU直結、対象NIC、継続TCP、Tailscale長時間条件、HA、実状態移行は未受入である。
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

現main `b971ea6` の隔離実daemon、独立Headscale、公式Tailscale peerでは、Tailscale workerのSIGKILL、PID 1再起動、peer停止/復帰、service撤去/再適用を実施した。同じpeerのexit選択を保ったままLANとexitのIPv4/IPv6 TCP/UDPが通り、撤去から再適用後のexit通信は6.344秒で復帰した。native120の6時間休止後timeoutと同一条件ではないため、長時間継続の合格には流用しない。試験で生成したengine、peer、鍵、capture、build出力は削除する。

これらは実プロセス・独立 peer・Linux kernel/PVE の相互通信結果である。対象 ONU/HGW/NIC、対象 appliance、実回線、全34 work package の受入を表さない。

## main統合と保存設定復元

管理PR #1（`4fcdb89`まで）、選択済み初期状態のディスク入力、HA観測保持修正 `30eaa27` を統合した作業版 `main-management-sam-126` で、管理2ケースとSAM4ケースが実daemon/独立peerで終了0。SAM claimの固定所有経路は設定準備時に一度だけ生成し、観測更新ごとのJSON再読込み・固定アドレス解析を除いた。3配布物の通常buildと静的リンクも確認した。

全57設定の別PVE受入版 `ops-management-integrated-native125` / `4fcdb89` は、DNS変更と保存設定復元の前後で同じPD worker・prefix・元期限を保持した。復元から取得DNS・IPv4/IPv6 TCP/UDP・BGP・特殊端末・Tailscaleの確認まで3.299秒、続く全体再起動後の復帰も終了0。修正前の現用HA版観測の破棄はこの条件で再現済み。専用VMは撤去し、自然更新の版120は変更していない。対象接続先、既存状態の移行、実測負荷、k8s/pve差分とCloud SAMは継続する。

main統合版126のハッシュ一致3実行物でも、通常125の起動イメージを基底にした全57設定のPVE復元・全体再起動が終了0。PD worker・prefix・元期限を保持し、復元から全通信確認まで4.305秒だった。新規126イメージ全体の受入とは区別する。専用VMは撤去済み。

公開は `25e7cf9`（PR #1統合済み）。PVEで固定した126版の変更対象Goソース29件・埋込みHTMLと一致し、試験した3実行物のハッシュも照合した。通常APKから新規作成した同じ126版の二台BIOSディスクも、実電源断・再参加の両方向が終了0。全通信復旧25.881/16.010秒、同じTCP接続とNATを保持して最大応答待ち7.196秒。UDP1,675送信中226期限切れ、DNS951件中20失敗。元60秒/15秒を維持した。旧主系不在の新規自動端末も取得DNSと両IP通信を利用でき、同じTailscale exit選択も再起動後に復旧した。両試験VMは撤去済み。実状態移行・実経路負荷・対象WAN/LANを閉じてからk8s/pve差分、Cloud SAMへ進む。

両homertのTailscale1.104.2と保存先`/var/lib/tailscale/tailscaled.state`（root:root、0600通常ファイル）は読取専用メタデータで確定した。状態本文は未取得で、実状態移行の合格には数えない。製品/VMを除く同じSSH Ethernet伝送自体の対照は102.259/131.067 Mbpsであり、旧候補の130〜149 Mbpsは製品上限の証拠ではない。

2026-11-01: homert02/03 の現行 router YAML を読取専用で取得し、隔離VMの明示NIC名へ置換して current main の migration/validation に通した。両方とも56 resource のnative設定になった。これは現行入力と変換器の組合せ確認であり、Tailscale保存状態、旧lease、実WAN、実VM起動の移行受入ではない。入力と変換出力は確認後に削除する。

2026-11-01: 実HGW用macvlan fixtureはDS-Lite復路をcaptureするが、任意のIPv4 ICMP fragmentation-needed、IPv6 Packet Too Big、断片化を発生させる制御可能な実経路を持たない。そのため自然PD観測や隔離DS-Lite PMTU試験を再実行して物理PMTU受入と取り違えなかった。物理受入には、host管理経路から分離したL2、MTUを下げて両ICMPを返せる実対向、router/対向のcapture、試験前後の管理interfaceとhost経路の一致が必要である。

2026-11-01: 現行入力の56 resourceにappliance用の明示管理addressを加えた57 resource候補はcurrent mainで`config validate`を通った。通常APK由来の新規diskを外部へ接続しないQEMU三NIC（`eth0`/`eth1`/`eth2`）で起動し、PID 1、supervisor、network/control/managementと設定workerの起動を確認した。空stateにはHA鍵・独立Tailnet enrollment・独立peerを供給していないため依存workerが停止しreadyに達しなかった。この結果は設定変換後の隔離起動確認に限り、実状態移行・単体router・HA・対象機の受入には数えない。QEMU停止後にhost管理interfaceとIPv4/IPv6経路が同一であることを照合した。disk、image、console、設定コピーを削除した。

同日に、この結果へ圧縮済みの旧full-profile disk/image、設定コピー、raw logを削除し、3.8 GiBを解放した。稼働中のプロセスが参照していないことを確認してから削除しており、実入力・鍵・captureは残していない。

2026-11-01: 初回状態移行の入力、停止境界、既存ディスクを置換しない新規ディスク作成、検査、失敗時の旧サービス復帰を[手順](initial-state-migration.md)として固定した。空の root 専用状態作業ディレクトリを新しい ext4 ディスクへ組み込み、読み取り専用マウントで`lost+found`以外がないこと、同じディスクの隔離 QEMU Linux PID 1起動（37.7秒）を確認した。旧状態本文・鍵・リースは取得していない。実状態移行、Tailscale 識別情報、リース/固定割当履歴の取込、対象機切替は未受入のままである。

2026-11-01: current main の正規 image を使う `check-initial-state-migration` を隔離 PVE で終了0にした。停止済み旧 owner の選択 Tailscale state、SSH host key、DHCP lease/sticky 入力を新規 disk に取り込み、disk内stateを読取検査した。独立 Headscale/公式 peerで同じnode IDとping、旧DHCP更新・別MACの新規lease、DNS UDP/TCP、LAN/WAN IPv4/IPv6 TCP/UDPを実通信とcaptureで確認し、新 PID 1停止後に旧入力が未変更のまま単独復帰できた。旧 routerdのSQLite table formatをfixtureが生成したものであり、実homert state・ONU/NIC・HAの受入ではない。VM、disk、鍵、capture、raw logは削除済みである。

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

## 物理実機試験の読取専用事前調査

2026-11-01にhomert02/03、各PVE node、接続bridge、guest NIC、link state、経路、稼働routerd processを読取専用で確認した。両VMは稼働中の4 vCPU/4 GiB・三VirtIO NIC構成であり、現行routerdと実回線用workerが動作している。active uplinkは既存PVE bridgeと稼働guestに接続済みで、未接続の物理portはcarrierを持たない。稼働中のshared bridgeや現在のONU/HGW経路を変更せずに、独立した一台を実回線へ接続できる未使用L2 portは確認できなかった。

物理試験を開始するには、専用switch VLAN/portとONU/HGWへ至る専用回線、または既存経路の計画停止のいずれかが必要である。前者なら、新規の隔離VMまたは専用機へ三NICを接続し、試験専用のIPv4/IPv6 address、DHCP identity、観測用LAN client、WAN/LAN capture点を割り当てる。開始直前と切戻し後に、対象VM設定、NIC link/route、routerd設定のSHA-256を読取比較する。後者ではhomert02/03停止または物理ケーブル差替えの範囲、予定時間、旧VM再起動と元bridge/NIC/route hash一致を切戻し条件として明示した承認が必要である。現時点ではいずれの変更も実施していない。

同日、管理hostの実HGW接続に、別MAC・private network namespace・独立LAN clientだけを作る`make check-uplink UPLINK_SECONDS=60`をcurrent `4dc020c`で実行した。WAN RA/DHCPv6 PD初回交換、実AFTR DS-LiteとIPv4復路、LAN DHCPv4/DHCPv6/SLAAC、IPv4/IPv6 HTTPS、学習DNS UDP/TCP、条件付きDNS障害/復帰、PD workerのSIGKILL/正常再起動、zero-lifetime RA撤去を確認した。自然PD Renewは既存の7,800秒受入結果を再利用し、この60秒試験では待たなかった。namespace、別MAC、capture、stateは終了時に削除した。本番homert02/03、管理address、IPv4/IPv6 route、resolver、PVE VM設定は操作しなかった。route/resolver/PVE設定のhashは開始前後で一致した。管理interfaceの生JSONにはDHCP lifetimeが含まれて毎秒変わるため、今後の比較はaddress本体のみを正規化してhash化する。これはONU直結、物理NIC MTU/PMTU、連続TCPまたは切替受入ではない。

1. homert02/03 の単体構成で、独立端末が DHCPv4/DHCPv6/SLAAC、DNS（条件付き forwarder と DoH を含む）、IPv4 NAT、IPv6 native forwarding、WAN DHCPv4/PD、RA と自然更新を使ってインターネットへ到達すること。
2. 二 WAN の Health/PBR、PPPoE、DS-Lite、firewall、既存 TCP と UDP mapping を実回線条件で確認すること。
3. 二台 HA の長時間 lease、PD、DHCPv6、VRRP、conntrack、障害と復帰を対象構成で受け入れること。
4. RR/leaf の実 WG/BGP、/32所有、NAT なし端末通信、移動・drain・revoke・capture を確認すること。
5. 更新・復旧、対象 appliance/NIC/ONU、ARM、PVE/Kubernetes/Cloud の各環境をそれぞれ受け入れること。

2026-11-01 の読取専用監査時点で、homert02/03 の置換は**不可**である。現行設定の変換と隔離ディスクの空状態起動、実HGWでの単体PD自然更新は確認済みだが、対象状態・lease/sticky の実移行、対象WAN/LAN、二台HA、既存接続、対象NIC/ONUの受入が未完了である。稼働中の旧サービスを停止せず、本番切替・本番配備を行わない。

稼働中の試験資源だけを操作対象から外す。終了済み stage、生 capture、重複 image、詳細 log は結果と再現手順を短く記録後に削除する。秘密を含む実入力・鍵・identity・pcap・インフラ詳細は公開しない。

2026-11-01: current main `4e70be1` で、homert03の現行設定を読取専用で取得し、現行migration policyと新規管理DHCP Option 61、隔離用HA証明書、独立Headscale enrollmentへ差し替えた。109入力resourceから57 native resourceを生成し、`routerd config validate`を通した。通常APKから作った新規BIOS diskを、PVEの新規4CPU/4GiB VM（ID 182、bridge/NICなし）で起動した。PID 1、supervisor、network/control/managementとworker起動までは実行したが、PVE UNIX socketとhost TAPを結ぶ既存QEMU `hubport`中継がEthernet frameを転送しておらず、管理DHCP、RA/DHCPv6-PD、Headscaleへpacketが到達しなかった。従ってready、LAN/DNS/forwarding、HA、Tailscaleの受入には数えない。VM、PVE remote stage、disk、鍵、capture/log、namespace、TAPを同じ作業で削除した。本番VM/NIC/WAN/Tailnet状態には変更していない。次はこの中継を実フレーム転送できる隔離L2へ置換してから、同じ57-resource current imageで再実行する。

2026-11-01: `b5e0d5a`のQEMU stream/TAP直接中継を、閉域bridgeとLinux netnsで実frame双方向（stream→TAP 25 byte、TAP→stream 22 byte）確認した。current APK `b5e0d5a0fea4`、現行109→57 resource設定、新規管理DHCP identity・HA/Headscale鍵・空stateを使い、新規PVE VMで再起動した。既存ユーザーSSH control masterでsparse diskを転送し、2 GiBの論理サイズ、sparse allocation、SHA-256一致を確認した。guestは管理DHCP Bound、独立Headscale login、WAN RA prefix/default routeを実packetで確認した。disk builderへ渡した管理SSH公開鍵がguestに受理されず、control APIでのworker状態、DHCPv6-PD、LAN/DNS/forwarding以降を実施できなかった。未達項目は合格に数えない。VM、remote stage、disk、鍵、state、log、peer process、namespace、bridge、TAPを削除し、本番接続は変更していない。次はdisk boot overlayのSSH authorized key配備を実guestで修正・確認して同じ閉域受入を再開する。

2026-11-01: `f7cf57e`時点の管理SSH不達は、disk builderが元initramfsの後ろへ別gzip memberとしてoverlayを連結し、実Linux kernelが先頭memberだけを展開したことが原因だった。builderは元initramfsをprivate mount namespace内で展開し、選択した`/etc`・`boot.json`を上書きして単一CPIO/gzipへ再構成するよう修正した。新規2 GiB diskをオフラインで検査し、`boot.json`が一件だけ、指定`admin`・管理公開鍵・`/dev/vda2` state設定を持つことを確認した。`images/config-qemu.json`を使う新規QEMU Linux/PID 1で、鍵付きmanagement SSHの`show status`/network/config/version、network worker restart/recovery、およびSSH経由poweroffまでを`qemu-smoke.py`で実行し、guestの正常poweroffを確認した。公開鍵・private key・serial log・diskは保持しない。この修正は管理鍵配備の境界を満たすが、PVE閉域の57-resource受入、DHCPv6-PD、LAN DHCP/DNS、forwarding、DS-Lite、MTU/MSS、HAは未再実行である。

2026-11-01: `7c775a2`で新規diskを作り、閉域PVEの新規三NIC VMへQEMU stream/TAPだけで接続した。管理DHCP、routerd2固有SSH banner、pinしたhost key、公開鍵認証、PID 1、`ready=true`、全required worker ready、WAN RA受信、独立Headscale enrollmentまでを実通信で確認した。full homert03 profileのLAN DHCPはVRRPがVIPを公開せず開始しなかった。worker状態と設定を照合すると、firewallの`public-dns` address setは五つの公開DNS名を`127.0.0.1:53`へ問い合わせ、core resolverの既定upstreamは実DoH（NextDNS/Cloudflare）である。閉域の通常DNSだけではDoH TLS検証を満たせず、address setがunresolvedとなりVRRPは`ReadinessTimeout`を維持した。成功として扱わない。次回は実DoH到達性を持つ独立上流、または試験用CAと明示したDoH peerを構成してから同じ単体受入を再開する。VM、remote stage、disk、鍵、capture、namespace、bridge、TAP、relay、Headscaleを削除し、VM 182不在を確認した。

2026-11-01: current main `1460bda` の `make check-initial-state-migration INITIAL_STATE_PVE_HOST=root@pve07.lain.local INITIAL_STATE_PVE_JUMP=chatty.lain.local` を実行した。新規 PVE VM `routerd2-initial-state-*`（4 vCPU、4 GiB、既存 bridge/NIC なし）を通常 build の disk で起動し、停止済み旧 owner の選択済み Tailscale state、SSH host key、DHCP lease/sticky record を新しい state disk に取り込んだ。独立 Headscale/公式 Tailscale peerで同一 node ID と ping、元 DHCP client の更新と別 MAC の新規 lease、DNS UDP/TCP、LAN/WAN の IPv4/IPv6 TCP/UDP を実通信・WAN capture で確認した。新 PID 1を停止して旧 owner を復帰し、同一 ID の単独所有も確認した。試験 VM、remote stage、local stage、capture、鍵、raw log は終了確認後に削除した。旧 routerd の SQLite table formatをfixtureで生成する移行経路であり、実homert state、二台HA、ONU/HGW/NICの受入ではない。

2026-11-01: current main `f046419` で `sudo make check-health-pbr` を実行した。実supervisor/control/network/Health workerと独立二WAN/LAN namespace peerを使う `TestDaemonConnectionMarkedEgressPolicy`、`TestDaemonHealthPBRFailoverAndRecovery`、`TestDaemonEgressRouteHealthyMultipath` が通過した。各ケースはTCP/UDPとpacket capture、Health worker/supervisor再起動、古い通知、期限切れ、confirmed rollbackを含む。試験stageとraw logは終了後に削除した。対象PVE二VM、実ONU/HGW/NICの受入ではない。

2026-11-01: current main `0d77003` の正規 appliance image（manifest `RELEASE_ID=0d770030664d`）で、`make qemu-pppoe QEMU_PVE_HOST=root@pve07.lain.local QEMU_PVE_JUMP=chatty.lain.local` を終了0にした。新規隔離 PVE VM（4 vCPU、4 GiB、既存 bridge/NIC なし）をPID 1として起動し、独立PPPoE AC・ISC DHCPv6 server・DNS peer・Linux LAN clientと接続した。LAN DHCPv4、CHAP、DHCPv6 PD/SLAAC、DNS UDP/TCP、IPv4/IPv6 TCP/UDP、PD Renew、PPP worker restart、control restart後の20秒継続通信を確認した。captureではzero-lifetime RAがPADTに先行し、DHCPv6 Releaseを送らず、DUID・設定revision・DNS worker generationを保持した。VM 182、PVE stage、raw capture、serial/peer log、local stageは結果記録後に削除済みである。これは実ONU/HGW/NICまたは二台HAの受入ではない。同じcommitで`make check`（test、全体Race Detector、vet、Python checks）は終了0である。

2026-11-01: current main `68846a3` の実HGW接続 `ens18` で、`make check-uplink UPLINK_SECONDS=90 UPLINK_WAN_OUTAGE=1 UPLINK_FULL_RESTART=1` を終了0にした。別MACのprivate namespace内で実daemonを起動し、独立LAN client二台のDHCPv4、DHCPv6、SLAAC、LAN IPv4/IPv6通信、条件付きDNSの障害分離と回復、実HGW WANの遮断・復帰、PD worker SIGKILL/通常再起動、daemon全体再起動、zero-lifetime RA、IPv4/IPv6 HTTPS、学習DNS UDP/TCPを確認した。上流grantはReleaseせず、DUID/IAID/prefixを保持した。namespace、別MAC、capture、raw log、stageは終了確認後に削除した。ONU直結、物理NIC MTU/PMTU、連続TCP、homert02/03本番切替、二WAN/HAの受入はこの結果に含めない。

2026-11-01: current main `cc619fa` で `sudo make check-ha-state` を終了0にした。実supervisor/control/network/VRRP/DHCP worker、conntrackd、dnsmasq、独立client/serverとcaptureを使い、DHCP owner、相互TLS、両方向takeover、同一TCP socket、UDP NAT mapping、engine SIGKILL/recovery、実lease更新/期限/sticky hold、sync worker撤去と元sysctl復元を確認した。UDPは3,034送信中70件期限切れ、最長無応答1.225秒、同一TCPの最長応答待ちは1.675秒だった。これは隔離Linux namespaceの二台経路であり、二台PVE appliance、実回線、対象NIC、無停止HAの受入ではない。stage、capture、raw logは終了確認後に削除する。
