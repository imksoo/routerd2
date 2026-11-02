# routerd2 開発引き継ぎ

更新: 2026-11-02 13:46 JST。最初の製品は未完成であり、[実装計画](implementation-plan.md)、[機械可読台帳](implementation-plan.json)、[試験計画](test-plan.md)、[AGENTS.md](../AGENTS.md)を基準に進める。各実通信試験の直前に試験計画の実行手順と対象項目を読み直す。対象本番の置換はしていない。

## 次の開発順序（2026-11-03）

`cb0f011` が GitHub の `main` と一致している。`b508524` と `703aaf9` で追加した、homert02/03 と機能・安全性の意図を照合する native `ResourceProgram` 設定例と対応表も同じ履歴に含まれる。旧 routerd 設定の自動変換、旧設定形式との互換読み込み、既存設定の自動適用は実装しない。対象機へ導入するときは、native 設定を新たに組み立てる。

1. `pve07` の既存隔離試験を正本として再開する。VM 151 `routerd2-dhcp-natural-1793418547` は、2026-11-02 06:49:18 UTC に `/run/routerd/network.sock` が存在しない状態で worker が停止し、試験駆動側が消えたまま VM だけが残っている。既存ランナー、起動順序、終了処理を特定し、失敗原因を直してから同じ隔離構成で再実行する。失敗した VM を稼働中の証拠や合格結果として扱わない。
2. current `main` の一台構成を PVE の隔離 WAN・LAN・管理リンク、独立した WAN peer と LAN client で実行する。DHCPv4、DHCPv6 PD、RA、取得 DNS、DS-Lite、NAT、IPv6 転送、MTU/MSS、端末と WAN の自然更新、対象通信負荷を、実プロセス・kernel・packet capture で確認する。任意の試験時間や回数を新設せず、設定された lease と障害条件に従う。
3. homert02/03 の保存設定を読み取り専用で再照合し、変換ではない native 設定を隔離 PVE で組み立てる。機能または安全性が対応しない項目は、対応済みと扱わず明記する。実状態移行は、単体構成の通信結果が得られてから隔離環境だけで予行する。
4. 二 WAN、二台 HA、Tailscale の長時間接続、更新、明示切戻し、自動復旧を、この順に既存 PVE 手順で確認する。対象本番の切替、配備、稼働中サービス停止、共有 bridge の変更は、明示承認なしに行わない。
5. 終了した各隔離試験では、結果要約と再実行手順を残し、VM、ディスク、鍵、packet capture、作業ディレクトリを撤去する。GitHub の `main` と作業ツリーの状態を照合してから次の段階へ進む。

## 現在の実装

`main` は物理 DHCP ingress、Health の readiness、管理・計測の統合を含む。Tailscale は上流 Linux 標準どおり `NetfilterMode=2` と `NoSNAT=false` を用い、上流 daemon がその forwarding/SNAT 規則を所有する。通常配布物は `make build`、appliance image は `make image` で同じ `RELEASE_ID` を明示して作る。`routerd` と `routerd-logging` は CGO0、承認済みの `routerd-modules` だけが CGO1 静的リンクである。

DHCP HA は VRRP が配布役を選び、dnsmasq に VIP と物理 ingress を明示する。旧 routerd の実績に従い、物理 ingress と VIP の対応を dnsmasq の `bridge-interface` で表す。HA DHCP listener と同じ物理 UDP/67 を別 service が共有する構成は、実 kernel の SO_REUSEPORT 配送で更新先が不定になるため拒否する。

## 実通信で確認した範囲

`6cd2404` では、同期断中に待機側だった DHCP owner が昇格しても、元ownerからmTLSで現在のlease stateを受け取るまでdnsmasqを起動しないようにした。current mainで`make check-ha-dhcp-failover`と`make check-ha-state`を実行し、独立Linux namespaceの二台、実supervisor/control/network/DHCP/VRRP/Health worker、ISC DHCPv6、AFTR、独立TCP/UDP peerとpacket captureで、遮断中に配布しないこと、再同期後の重複しない新規lease、既存leaseの更新、DHCP worker再起動後の更新、通常の両方向HA handoffを確認した。通常handoffではUDP 2,992送信中69期限切れ、最大無応答1.109秒、同一TCPの最大応答待ち1.660秒だった。無損失とは扱わない。stage、capture、鍵、namespaceは終了後に削除した。これはPVE二VM、homert実設定、対象ONU/HGW/NICの受入ではない。


同じ`1a3843b`で`make check-health-pbr`を実行し、独立Linux peerと実supervisor/control/network/Health workerによる二WANの`TestDaemonConnectionMarkedEgressPolicy`、`TestDaemonHealthPBRFailoverAndRecovery`、`TestDaemonEgressRouteHealthyMultipath`が通過した。各試験はTCP/UDP、packet capture、Health worker復旧、失効とconfirmed rollbackを対象にする。PVEのfull homert03通信受入や実ONUの資格には流用しない。raw log/capture/stageは保持しない。

旧設定変換に依存した DHCP/DNS fixture とその過去の合格記録は、変換器の削除に伴い現行実装の証拠から外した。現在の受入入口は `scripts/daemon-tests.py` の native `ResourceProgram` 試験である。この作業ツリーでは統合試験をまだ再実行しておらず、実行環境に `CAP_NET_ADMIN` / `CAP_NET_RAW` がないため、実daemonのnetns試験はここでは起動できない。

`fff1e29`では`make check-dhcp6`も実行し、独立dnsmasq/radvd/DHCPv6 peerとの`TestDaemonDHCP6RAIndependentPeers`と`TestDaemonDHCP6DuplicateAddressDecline`が通過した。DUID、RA、PDを含むworker交換を確認するが、PVE full profileや実HGW/ONUの受入ではない。raw log/stageは削除する。

`d55486d`では`make check-dslite-pmtu`を実行した。独立PPPoE AC、AFTR、LAN/WAN peerを使い、DS-Lite確立、IPv4 NATと復路、1 MiB IPv4/IPv6 TCPのupload/download、MSS、IPv4 ICMP fragmentation-needed、同一UDP socketの1454→1400 PMTU学習、IPv6 ICMP Packet Too Bigと後続1200-byte UDPを確認した。これはLinux namespace内の対向であり、物理ONU/NIC/switchのMTUやPMTUは証明しない。

同じ`d55486d`で`make check-tailscale-homert`を実行した。独立Headscaleと公式Tailscale peerで、IPv4/IPv6 LAN・subnet・exitのTCP/UDP、実経路とSNAT、保存identity、worker/PID 1再起動、service撤去/再適用、peer再起動、認証失効後の遮断、packet captureを通過した。PVE/実回線のTailscale受入には流用しない。各試験のraw log、capture、鍵、stageは削除済みである。

- `health-ready-native105`: 独立二 WAN namespace peer、実 supervisor/control/network/Health worker で Health→PBR の障害切替・復旧、worker と supervisor の再起動、古い通知・期限切れの拒否、confirmed rollback、TCP/UDP と packet capture を確認した。
- `dslite-pmtu-main-1e317d4`: 本番NICから隔離した一台 Linux namespace の独立 PPPoE/DS-Lite 対向で、IPv4 DF の ICMP fragmentation-needed、同一 UDP socket の1454→1400 PMTU学習、NAT後1368-byte payload/replyを終了0で確認した。対象WAN/NICのPMTU・物理負荷受入ではない。
- `hgw-short-main-20bc21c`: 実HGWへ隔離 macvlan を接続した current main と独立LAN端末二台で、DHCPv4/DHCPv6 information/SLAAC、委譲RDNSS/search domain、IPv4 DS-Lite/IPv6 HTTPS、学習DNS UDP/TCP、条件付きforwarder停止/復帰、WAN blackhole復帰、PD workerのSIGKILL/正常再起動、全daemon再起動後の通信を確認した。続く7,800秒観測は自然PD Renew/Reply 1回、二台の自然DHCP更新、DUID/IAID/prefix保持、zero-lifetime RA withdrawal、726周回のIPv4/IPv6 HTTPSと学習DNS UDP/TCPを終了0で確認した。ONU直結、対象NIC、継続TCP、Tailscale長時間条件、HAの受入は未完了である。
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

管理PR #1（`4fcdb89`まで）、新規状態ディスクの準備、HA観測保持修正 `30eaa27` を統合した作業版 `main-management-sam-126` で、管理2ケースとSAM4ケースが実daemon/独立peerで終了0。SAM claimの固定所有経路は設定準備時に一度だけ生成し、観測更新ごとのJSON再読込み・固定アドレス解析を除いた。3配布物の通常buildと静的リンクも確認した。

全57設定の別PVE受入版 `ops-management-integrated-native125` / `4fcdb89` は、DNS変更と保存設定復元の前後で同じPD worker・prefix・元期限を保持した。復元から取得DNS・IPv4/IPv6 TCP/UDP・BGP・特殊端末・Tailscaleの確認まで3.299秒、続く全体再起動後の復帰も終了0。修正前の現用HA版観測の破棄はこの条件で再現済み。専用VMは撤去し、自然更新の版120は変更していない。対象接続先、実測負荷、k8s/pve差分とCloud SAMは継続する。

main統合版126のハッシュ一致3実行物でも、通常125の起動イメージを基底にした全57設定のPVE復元・全体再起動が終了0。PD worker・prefix・元期限を保持し、復元から全通信確認まで4.305秒だった。新規126イメージ全体の受入とは区別する。専用VMは撤去済み。

公開は `25e7cf9`（PR #1統合済み）。PVEで固定した126版の変更対象Goソース29件・埋込みHTMLと一致し、試験した3実行物のハッシュも照合した。通常APKから新規作成した同じ126版の二台BIOSディスクも、実電源断・再参加の両方向が終了0。全通信復旧25.881/16.010秒、同じTCP接続とNATを保持して最大応答待ち7.196秒。UDP1,675送信中226期限切れ、DNS951件中20失敗。元60秒/15秒を維持した。旧主系不在の新規自動端末も取得DNSと両IP通信を利用でき、同じTailscale exit選択も再起動後に復旧した。両試験VMは撤去済み。実経路負荷・対象WAN/LANを閉じてからk8s/pve差分、Cloud SAMへ進む。




2026-11-01: 実HGW用macvlan fixtureはDS-Lite復路をcaptureするが、任意のIPv4 ICMP fragmentation-needed、IPv6 Packet Too Big、断片化を発生させる制御可能な実経路を持たない。そのため自然PD観測や隔離DS-Lite PMTU試験を再実行して物理PMTU受入と取り違えなかった。物理受入には、host管理経路から分離したL2、MTUを下げて両ICMPを返せる実対向、router/対向のcapture、試験前後の管理interfaceとhost経路の一致が必要である。


同日に、この結果へ圧縮済みの旧full-profile disk/image、設定コピー、raw logを削除し、3.8 GiBを解放した。稼働中のプロセスが参照していないことを確認してから削除しており、実入力・鍵・captureは残していない。



2026-11-01: 通常126 APKから起動した隔離PVEの4CPU/4GiB実VMと、同じPVE内の独立LAN/WAN端末を直接接続した。SSHは管理・結果回収だけで、SSH転送に使える二つの別リンクでTCP/5201 packetが0なのをcaptureした。最小の静的IPv4 NAT/IPv6転送構成で15秒単一TCPは、IPv4 NAT受信1.275/1.280Gbps、IPv6受信1.228/1.243Gbpsだった。相手はIPv4でNAT後の`192.168.1.249`、IPv6で元のLAN端末を観測した。このため旧130〜149Mbpsを製品上限とする根拠はなくなった。DS-Lite、DHCP/SLAAC/DNS、HA、対象回線・NICの負荷合格には流用しない。VM、raw disk、capture、パッケージとremote stageは削除済み。

今回の生成物・ログ等823.2MiBと専用Docker imageに続き、終了した二台試験のstage約612MiB（capture・一時鍵・state・disk）を削除した。main統合・未変更・稼働参照なしを確認した完了済みworktree二つも `git worktree remove` で撤去し、約1,012MiBの重複出力を解放した。稼働参照がある別tree、次の受入に使う126版image/runnerと既存`AGENTS.md`変更は保持した。native120の根拠を上記へ圧縮した後、同じくstageを削除する。空きは約38GiB。

## 直近の実行

統合候補 `20ea125` / `ops-management-integrated-native123` では、管理機能六経路の統合と隔離実通信、二台の通常APK/BIOSディスクによる双方向電源断切替・再起動が終了0だった。既存TCPを維持し、新規端末の取得DNSとIPv4・IPv6通信も復旧した。UDPは1,763送信中233欠落、DNSは1,017問い合わせ中17失敗であり、無損失ではない。詳細と限界は [試験計画](test-plan.md) の統合候補版の節に記録した。GitHubの `main` ブランチへの取り込みと対象実機の受入は別に残る。

同じ候補版123への署名付き更新、120への明示切戻し、正常性確認失敗による自動復旧も隔離PVEで終了0だった。通信確認までそれぞれ36.278秒、34.725秒、137.304秒で、最後は90秒の試行期限を含む。設定と既存の識別情報を保持した。公開済みSAM修正 `c85c7a1` を取り込んだ `79813e7` は関連パッケージの競合検査・静的検査が終了0であり、候補版123の実通信結果とは区別する。

本番対象は各4仮想CPU・4 GiBのPVE仮想機械である。次の対象受入は本番接続先、Ubuntu/Linux環境、実測負荷との差分を確認する。新しい物理機器の要件をhomert置換の前提へ追加しない。

自然更新の専用job `ops-management-combined-native120` は終了した。通常端末は16:37:31.917 JST、特殊端末は16:37:32.409 JSTに、元の43,200秒lease・21,600秒T1のunicast REQUESTをVIPへ送り、同一XIDとoption 54を持つACKを受け、期限を延長した。job全体は後続Tailscale TCP timeoutのため終了1であり、Tailscale長時間継続の合格には数えない。

DHCPv6 PD の再起動後 Rebind と、不良 candidate から known-good runtime へ自動 rollback する PVE 経路は完了した。capture と一時 stage は削除済みであり、これは対象 ONU/HGW や実回線の受入ではない。

自然更新コホートは故障注入や設定交換に転用しない。故障・切替は別 stage で行う。終了済みの native93 と native95 は現在のコホートではなく、前者は終了1、後者は終了0である。過去の個別結果を current HEAD や実機の受入へ流用しない。

## 残る受入

## 物理実機試験の読取専用事前調査

2026-11-01にhomert02/03、各PVE node、接続bridge、guest NIC、link state、経路、稼働routerd processを読取専用で確認した。両VMは稼働中の4 vCPU/4 GiB・三VirtIO NIC構成であり、現行routerdと実回線用workerが動作している。active uplinkは既存PVE bridgeと稼働guestに接続済みで、未接続の物理portはcarrierを持たない。稼働中のshared bridgeや現在のONU/HGW経路を変更せずに、独立した一台を実回線へ接続できる未使用L2 portは確認できなかった。

物理試験を開始するには、専用switch VLAN/portとONU/HGWへ至る専用回線、または既存経路の計画停止のいずれかが必要である。前者なら、新規の隔離VMまたは専用機へ三NICを接続し、試験専用のIPv4/IPv6 address、DHCP identity、観測用LAN client、WAN/LAN capture点を割り当てる。開始直前と切戻し後に、対象VM設定、NIC link/route、routerd設定のSHA-256を読取比較する。後者ではhomert02/03停止または物理ケーブル差替えの範囲、予定時間、旧VM再起動と元bridge/NIC/route hash一致を切戻し条件として明示した承認が必要である。現時点ではいずれの変更も実施していない。

2026-11-02: ユーザーが示した `chatty.lain.local:~/homert02` の設定を読み取り専用で確認した。homert02/03は各109 resourceで、NTPはDHCPv6 informationから学習するsourceと `ntp.jst.mfeed.ad.jp` / `ntp.nict.jp` の条件付きstatic fallbackを使う。続いてcurrent source treeから `make check-ntp-dhcp6 DHCP6_SERVER=/usr/sbin/dhcpd DOCKER='sudo -n docker'` をexit 0で実行した。実supervisor/control/network/NTP/DHCPv6/VRRP worker、ISC DHCPv6、Keepalived、chronyd、独立LAN chronyd、実DNS peerを使い、設定中の両hostnameをDNS照会し、実NTP packet drop後の復旧、learned source期限切れ/fallback/reacquisition、VRRP切替、supervisor/control停止中の期限切れと復旧、LAN NTP応答をpcapで確認した。これは独立Linux namespaceの実通信であり、homertのDoH/条件付きDNSサービスそのもの、通常PD lifetime、実ONU/NIC、全設定を載せたPVE VM受入ではない。リモートrepo/configは変更していない。PVE VM IDと実行時設定差分は未確認で、現在の開発hostには仮想NICしかない。対象機器での受入には、隔離L2接続先、独立有線client、WAN/LAN capture点が必要である。既存共有経路を使う場合は計画停止と復旧手順が必要。

同日、管理hostの実HGW接続に、別MAC・private network namespace・独立LAN clientだけを作る`make check-uplink UPLINK_SECONDS=60`をcurrent `4dc020c`で実行した。WAN RA/DHCPv6 PD初回交換、実AFTR DS-LiteとIPv4復路、LAN DHCPv4/DHCPv6/SLAAC、IPv4/IPv6 HTTPS、学習DNS UDP/TCP、条件付きDNS障害/復帰、PD workerのSIGKILL/正常再起動、zero-lifetime RA撤去を確認した。自然PD Renewは既存の7,800秒受入結果を再利用し、この60秒試験では待たなかった。namespace、別MAC、capture、stateは終了時に削除した。本番homert02/03、管理address、IPv4/IPv6 route、resolver、PVE VM設定は操作しなかった。route/resolver/PVE設定のhashは開始前後で一致した。管理interfaceの生JSONにはDHCP lifetimeが含まれて毎秒変わるため、今後の比較はaddress本体のみを正規化してhash化する。これはONU直結、物理NIC MTU/PMTU、連続TCPまたは切替受入ではない。

1. homert02/03 の単体構成で、独立端末が DHCPv4/DHCPv6/SLAAC、DNS（条件付き forwarder と DoH を含む）、IPv4 NAT、IPv6 native forwarding、WAN DHCPv4/PD、RA と自然更新を使ってインターネットへ到達すること。
2. 二 WAN の Health/PBR、PPPoE、DS-Lite、firewall、既存 TCP と UDP mapping を実回線条件で確認すること。
3. 二台 HA の長時間 lease、PD、DHCPv6、VRRP、conntrack、障害と復帰を対象構成で受け入れること。
4. RR/leaf の実 WG/BGP、/32所有、NAT なし端末通信、移動・drain・revoke・capture を確認すること。
5. 更新・復旧、対象 appliance/NIC/ONU、ARM、PVE/Kubernetes/Cloud の各環境をそれぞれ受け入れること。


稼働中の試験資源だけを操作対象から外す。終了済み stage、生 capture、重複 image、詳細 log は結果と再現手順を短く記録後に削除する。秘密を含む実入力・鍵・identity・pcap・インフラ詳細は公開しない。


2026-11-01: `b5e0d5a`のQEMU stream/TAP直接中継を、閉域bridgeとLinux netnsで実frame双方向（stream→TAP 25 byte、TAP→stream 22 byte）確認した。current APK `b5e0d5a0fea4`、現行109→57 resource設定、新規管理DHCP identity・HA/Headscale鍵・空stateを使い、新規PVE VMで再起動した。既存ユーザーSSH control masterでsparse diskを転送し、2 GiBの論理サイズ、sparse allocation、SHA-256一致を確認した。guestは管理DHCP Bound、独立Headscale login、WAN RA prefix/default routeを実packetで確認した。disk builderへ渡した管理SSH公開鍵がguestに受理されず、control APIでのworker状態、DHCPv6-PD、LAN/DNS/forwarding以降を実施できなかった。未達項目は合格に数えない。VM、remote stage、disk、鍵、state、log、peer process、namespace、bridge、TAPを削除し、本番接続は変更していない。次はdisk boot overlayのSSH authorized key配備を実guestで修正・確認して同じ閉域受入を再開する。

2026-11-01: `f7cf57e`時点の管理SSH不達は、disk builderが元initramfsの後ろへ別gzip memberとしてoverlayを連結し、実Linux kernelが先頭memberだけを展開したことが原因だった。builderは元initramfsをprivate mount namespace内で展開し、選択した`/etc`・`boot.json`を上書きして単一CPIO/gzipへ再構成するよう修正した。新規2 GiB diskをオフラインで検査し、`boot.json`が一件だけ、指定`admin`・管理公開鍵・`/dev/vda2` state設定を持つことを確認した。`images/config-qemu.json`を使う新規QEMU Linux/PID 1で、鍵付きmanagement SSHの`show status`/network/config/version、network worker restart/recovery、およびSSH経由poweroffまでを`qemu-smoke.py`で実行し、guestの正常poweroffを確認した。公開鍵・private key・serial log・diskは保持しない。この修正は管理鍵配備の境界を満たすが、PVE閉域の57-resource受入、DHCPv6-PD、LAN DHCP/DNS、forwarding、DS-Lite、MTU/MSS、HAは未再実行である。

2026-11-01: `7c775a2`で新規diskを作り、閉域PVEの新規三NIC VMへQEMU stream/TAPだけで接続した。管理DHCP、routerd2固有SSH banner、pinしたhost key、公開鍵認証、PID 1、`ready=true`、全required worker ready、WAN RA受信、独立Headscale enrollmentまでを実通信で確認した。full homert03 profileのLAN DHCPはVRRPがVIPを公開せず開始しなかった。worker状態と設定を照合すると、firewallの`public-dns` address setは五つの公開DNS名を`127.0.0.1:53`へ問い合わせ、core resolverの既定upstreamは実DoH（NextDNS/Cloudflare）である。閉域の通常DNSだけではDoH TLS検証を満たせず、address setがunresolvedとなりVRRPは`ReadinessTimeout`を維持した。成功として扱わない。次回は実DoH到達性を持つ独立上流、または試験用CAと明示したDoH peerを構成してから同じ単体受入を再開する。VM、remote stage、disk、鍵、capture、namespace、bridge、TAP、relay、Headscaleを削除し、VM 182不在を確認した。


2026-11-01: current main `f046419` で `sudo make check-health-pbr` を実行した。実supervisor/control/network/Health workerと独立二WAN/LAN namespace peerを使う `TestDaemonConnectionMarkedEgressPolicy`、`TestDaemonHealthPBRFailoverAndRecovery`、`TestDaemonEgressRouteHealthyMultipath` が通過した。各ケースはTCP/UDPとpacket capture、Health worker/supervisor再起動、古い通知、期限切れ、confirmed rollbackを含む。試験stageとraw logは終了後に削除した。対象PVE二VM、実ONU/HGW/NICの受入ではない。

2026-11-01: current main `0d77003` の正規 appliance image（manifest `RELEASE_ID=0d770030664d`）で、`make qemu-pppoe QEMU_PVE_HOST=root@pve07.lain.local QEMU_PVE_JUMP=chatty.lain.local` を終了0にした。新規隔離 PVE VM（4 vCPU、4 GiB、既存 bridge/NIC なし）をPID 1として起動し、独立PPPoE AC・ISC DHCPv6 server・DNS peer・Linux LAN clientと接続した。LAN DHCPv4、CHAP、DHCPv6 PD/SLAAC、DNS UDP/TCP、IPv4/IPv6 TCP/UDP、PD Renew、PPP worker restart、control restart後の20秒継続通信を確認した。captureではzero-lifetime RAがPADTに先行し、DHCPv6 Releaseを送らず、DUID・設定revision・DNS worker generationを保持した。VM 182、PVE stage、raw capture、serial/peer log、local stageは結果記録後に削除済みである。これは実ONU/HGW/NICまたは二台HAの受入ではない。同じcommitで`make check`（test、全体Race Detector、vet、Python checks）は終了0である。

2026-11-01: current main `68846a3` の実HGW接続 `ens18` で、`make check-uplink UPLINK_SECONDS=90 UPLINK_WAN_OUTAGE=1 UPLINK_FULL_RESTART=1` を終了0にした。別MACのprivate namespace内で実daemonを起動し、独立LAN client二台のDHCPv4、DHCPv6、SLAAC、LAN IPv4/IPv6通信、条件付きDNSの障害分離と回復、実HGW WANの遮断・復帰、PD worker SIGKILL/通常再起動、daemon全体再起動、zero-lifetime RA、IPv4/IPv6 HTTPS、学習DNS UDP/TCPを確認した。上流grantはReleaseせず、DUID/IAID/prefixを保持した。namespace、別MAC、capture、raw log、stageは終了確認後に削除した。ONU直結、物理NIC MTU/PMTU、連続TCP、homert02/03本番切替、二WAN/HAの受入はこの結果に含めない。

2026-11-01: current main `cc619fa` で `sudo make check-ha-state` を終了0にした。実supervisor/control/network/VRRP/DHCP worker、conntrackd、dnsmasq、独立client/serverとcaptureを使い、DHCP owner、相互TLS、両方向takeover、同一TCP socket、UDP NAT mapping、engine SIGKILL/recovery、実lease更新/期限/sticky hold、sync worker撤去と元sysctl復元を確認した。UDPは3,034送信中70件期限切れ、最長無応答1.225秒、同一TCPの最長応答待ちは1.675秒だった。これは隔離Linux namespaceの二台経路であり、二台PVE appliance、実回線、対象NIC、無停止HAの受入ではない。stage、capture、raw logは終了確認後に削除する。

2026-11-01: current main `62e60f9` で `sudo make check-ha-egress` を終了0にした。実 supervisor/control/network/VRRP/DHCP/DHCPv6/RA/Health worker、conntrackd、dnsmasq、ISC DHCPv6、独立AFTR・backup WAN・client/server と packet capture を使い、PD由来B4/DS-Lite と backup WAN のTCP/UDP切替・復帰、VIPの全WAN喪失時撤去、DHCP owner、相互TLS、両方向PD handoff、同一TCP socketとUDP NAT mapping、engine SIGKILL/recovery、lease/PD期限、DHCP worker restart、設定撤去を確認した。captureは共有leaseのReleaseなしのRebind、IPv4-in-IPv6往復、backup WANのTCP/UDP、1本のTCP SYN、conntrack同期を確認した。UDPは3,510送信中398件期限切れ、最長無応答3.828秒、同一TCPの最長応答待ちは6.568秒だった。これは隔離Linux namespaceの二台経路であり、二台PVE appliance、実回線、対象NIC、無停止HAの受入ではない。stage、capture、raw logは終了確認後に削除する。

2026-11-01: current main `43f94e3` の通常appliance imageで、`make qemu-ha-pve QEMU_PVE_HOST=...` を終了0にした。新規の二台PVE VM（各4 vCPU、4 GiB、既存bridge/NICなし）と独立Linux LAN DHCP client・WAN TCP/UDP peerをprivate QEMU stream NICで接続した。DHCP lease、VRRP、conntrackd同期、NAT TCP/UDP、packet captureを確認し、a→bとb→aの強制電源断で同一TCP socketとUDP通信を維持した。PVE VM、remote/local stage、state disk、鍵、capture、namespace、TAPを削除済みである。この基本IPv4 HA経路は合格だが、PD/DS-Lite/backup WAN、DNS、実HGW/ONU/NIC、24時間安定性試験は別の未完了範囲である。

同じ二台PVE経路の短い継続確認では、再参加後の両機と独立端末を65秒動かし、DHCP renewal、同一TCP/UDP、captureを終了0で確認した。`QEMU_HA_STABILITY_SECONDS`で同じ試験の継続時間を明示する。これは24時間安定性試験の開始前にrunnerのduration経路を確認した結果であり、24時間の合格ではない。

2026-11-01: current `5072de5` で `sudo -n make check-uplink UPLINK_INTERFACE=ens18 UPLINK_AFTR=2404:8e00::feed:100 UPLINK_SECONDS=90 UPLINK_WAN_OUTAGE=1 UPLINK_FULL_RESTART=1` を実HGW接続に一度実行した。稼働homertの固定AFTR・MTU 1454に合わせた実daemonと独立Linux clientでWAN DHCPv4/PD/RA、LAN DHCPv4/DHCPv6/SLAAC、DNS UDP/TCPと条件付き転送の障害・復帰、DS-Lite/NAT IPv4、native IPv6、IPv4/IPv6 HTTPS、worker/daemon再起動を確認した。kernelの`dslite0` MTUは1454、実TCP SYNのpacket captureはMSS 1414、各familyで10 MB HTTPS転送に成功した。IPv4 DF 1426-byte payload（1454-byte packet）は2/3応答で、1 packetにLAN routerから`Destination Net Unreachable`が返った。同時刻のroute captureがなく原因は未確定で、この結果を完全なMTU受入とはしない。IPv6 1452-byte payload（1500-byte packet）は3/3応答。実PDのT1は試験開始時点で約7,186秒先、90秒試験で自然Renew/Replyは0件だった。これは自然更新の合格ではなく、既存の7,800秒実HGW結果とも合算しない。ランナーのPASS表示とnamespace/stage撤去を確認したが、実行セッションから数値終了statusは受け取れず、終了statusは未確認として記録する。full homert設定、対象router appliance/NICの実機資格、ONU直結は未受入。

2026-11-02: current product build `e0ea27e` と、DHCP固定予約を除いた作業中runnerを使い、実HGW `ens18` で `sudo -n make check-uplink UPLINK_INTERFACE=ens18 UPLINK_AFTR=2404:8e00::feed:100 UPLINK_SECONDS=60` を一度実行、exit 0。DHCP client二台は手動の割当固定なしに`198.18.0.2`と`.7`を取得した（`.2`はpool先頭の通常割当）。条件付きDNS用peerはpool外の`.254`。kernel FIBはDHCP clientの送信元から`dslite0`を選び、実トンネルMTUは1454だった。独立LAN peerから1426-byte payload（IPv4 total 1454）のDF pingを3送信し3応答、RTT 4.67–5.26 ms。routerの既存`any` capture（Linux SLL2）は同じICMP id/sequence 1–3をLAN ifindex 8で受信、`dslite0` ifindex 9へNAT後送出、返信を同tunnelとLANへ戻す様子を示した。`dslite0`からの返信は実AFTR `2404:8e00::feed:100`のトンネル復号後である。新しい試験では前回の1/3 `Destination Net Unreachable` は再現せず、前回の原因は依然未確定。終了時にrunnerがstage/namespace/captureを削除したため、outer WAN上の同一ICMP sequenceの生記録は保持していない。自然PD T1は約7,184秒先で、自然Renew/Replyは0件。PD worker SIGKILL/正常停止後の端末通信は1.31/3.26秒で復帰した。WAN outage・全daemon再起動・ONU直結・対象NIC資格は今回の範囲外。

2026-11-02: current main `44620b8` の更新・切戻し実受入は未実施。更新候補のPID 1採用と復帰には再起動が要るが、現用homertは停止しない。開発hostの`ens18`/`ens20`はvirtio_netで実ONU/NICがなく、既存の隔離boot経路はlocal QEMU/PVEであるため、今回の指示に従いどちらも起動しなかった。current main用Alpine imageはbuild成功後、bootせず削除した。旧`ops-management-integrated-native125`/`4fcdb89`の更新・明示rollback・Health失敗復帰は現mainと異なる候補の結果である。

Tailscaleの最新独立peer実daemon結果は`b971ea6`で、worker/PID 1再起動、peer停止復帰、service撤去/再適用後のTCP/UDPを確認し、exit再適用復帰は6.344秒だった。別の6時間idle試験は初回TCPを固定2秒でtimeout扱いし、その後の通信を観測していない。current mainにTailscale実装差分はなく、既存runnerには6時間idle経路がないため、短い同一試験を重ねず長時間条件は未受入として残す。

2026-11-02: `HEAD=3a69867`の未コミット変更を含む作業ツリーから`make check-uplink UPLINK_INTERFACE=ens18 UPLINK_AFTR=2404:8e00::feed:100 UPLINK_SECONDS=120 UPLINK_WAN_OUTAGE=1 UPLINK_FULL_RESTART=1 DOCKER='sudo -n docker'`を実行した。実HGW接続`ens18`に別MACの隔離namespaceを作り、実daemonと独立LAN端末で、DHCPv4 ACKから得た割当IPによるIPv4/IPv6 HTTPS、学習DNS UDP/TCP、条件付きDNS障害分離/復帰、二端末のDHCPv4自然更新、DHCPv6 information/SLAAC、WAN遮断後0.9秒での復帰、PD worker強制終了/通常再起動（1.76/2.93秒）、daemon再起動（3.8秒）、DUID/IAID/prefix保持、zero-lifetime RA撤去を確認した。WAN管理アドレスとdefault routeは試験前後で同一、namespace/stageは撤去済み。端末・WAN DHCPv4/PDの割当IPを固定していない。自然PD T1は約7,182秒先で今回未到達。runnerは各通信確認のPASSとcleanupを出力したが、実行ラッパーから最終終了statusを取得できなかったため終了コードは未確認とする。これは109-resource homert設定全体、実router appliance/NIC、ONU直結、連続TCP、HAの受入ではない。
