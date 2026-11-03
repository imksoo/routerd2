# routerd2 実通信・製品受入計画

更新: 2026-11-03 00:21 UTC。対象は implementation-plan.md と同JSONの34 work package。
目的は、独立端末を接続しただけで家庭用ルーターの通信が成立し、障害・再起動・更新から復旧する最初の製品を完成させること。

## 判定原則

- 旧 routerd の対応実装・運用上の修正と homert02/03 の実設定を、機能に着手する前に読む。実設定の値や秘密は公開しない。
- 実際の routerd2 process、Linux kernel、独立 client/peer を使う。受信アドレス、prefix、経路、DNS、識別子は実観測値を使う。固定予約を確認する場合だけ設定した予約値と照合する。
- DHCPv4/DHCPv6、RA、DNS、NAT、IPv4/IPv6 forwarding を個別に動かしただけでは合格にしない。設定適用から端末の名前解決・インターネット通信までを通す。
- packet capture、kernel FIB/firewall、端末の実通信、worker状態を必要な範囲で照合する。テストコード同士の一致、coverage、CI成功は相互接続の証拠にしない。
- namespace/PVEはそのLinux kernelと構成の証拠であり、ONU、対象NIC、対象機器、別cloudのqualificationではない。実機固有の動作は実機で確認する。
- 失敗時は製品・設定・試験peer・環境の原因を特定して直し、影響した実通信を再確認する。deadline超過だけで製品障害と断定しない。未確認は未確認と記録する。
- 試験開始直前にこの計画全体を読み直す。該当条件、既知の失敗、peer、実行版、観測点、隔離・復旧手順を確認し、計画が現状とずれていたら先に直す。
- 試験完了後は条件・結果・限界・再実行手順だけを残し、不要になったraw log、pcap、VM、stage、build outputを削除する。秘密や実設定をcommitしない。

## 現在の棚卸し

2026-11-03時点で、work packageは in_progress 22件、planned 12件で、製品全体を完了と判定した項目はない。過去の部分合格を全体完成に読み替えない。

- 最新の基準mainは 8ea44d7。ここでの差分は計画、Makefile、試験用ResourceProgram例、実HGW runnerであり、routerd2のGo製品コードは変更していない。
- 現在の作業ツリー（基準HEAD `8ea44d7`、製品Goコードは`c9d7055`から不変）で `make check` はGo/Python checks、Race Detector、vetを含め終了0。`make build DOCKER='sudo -n docker'`も終了0、`readelf`で3実行物にdynamic dependencyがないことを確認し、nDPI source/license noticesも存在する。
- 実supervisor/control/network/Health worker、独立二WAN/Linux peer、TCP/UDP、packet captureによる make check-health-pbr は障害切替・復旧、worker/supervisor再起動、古い通知・期限切れ、confirmed rollbackを通過済み。実対象回線や二台homertの受入ではない。
- 通常Alpine/PID 1 applianceの隔離PVE試験では、独立PPPoE AC/ISC DHCPv6/Linux clientを使うDHCPv4、PD/Renew、SLAAC、両IP通信、署名更新、明示・自動rollbackを通過済み。対象ONU/NICではない。
- 実HGWのmacvlanと独立Linux clientで、過去の短時間試験および7,800秒試験がある。今回のmain由来product build `7c1cb3619acb` による自然PD T1観測は、試験用の生成profileで完了した。自然Renew/ReplyをWAN captureで各1回確認し、Renewはclient UDP/546→server/547、対応ReplyはHGW source port 49153→client/546だった。期限が更新され、初回SolicitとRenewのDUID、IAID=1、委譲prefixは同一。旧routerdの受信実装もsender address/portで制限していない。実LinuxではWAN/LANとも`accept_dad=1`、観測したglobal IPv6 addressはnon-tentative、DAD failureなし。PD worker SIGKILL後1.32秒、graceful restart後3.61秒、全daemon restart後3.99秒で両clientの通信が復旧した。upstream grantをReleaseせずzero-lifetime RAとkernel withdrawalを確認。独立する両clientの自然DHCPv4更新と、IPv4/IPv6 HTTPSおよび学習DNSのUDP/TCPを676観測roundで確認した。raw capture/state/stageは削除済み。生成profileの試験であり、native homert設定・連続TCP・ONU直結・対象物理NICのqualificationではない。
- 2026-11-03に読み直したhomert02/03設定は各110 resourcesで、kind件数は一致する。両方にIPv6 host-traffic policy route（table 120）がある。`homert02-native.yaml` / `homert03-native.yaml` は各14 resources。main上の `single-router-dslite-dhcp-dns.yaml` は13 resources、working native HGW profileは15 resourcesで、いずれも全設定ではない。過去の一時57-resource入力を現行profileや完成証拠に流用しない。第6回native gateは終了0。DHCPv4/PD/RA/DHCPv6/SLAAC、DNS/DoH、DS-Lite IPv4/IPv6通信、WAN遮断復旧、PD workerとdaemon再起動を独立端末で確認した。15-resource profileの合格を110-resource homert受入に読み替えない。
- ens18/ens20はvirtio NICで、NTT ONU直結と対象物理NICのqualificationは未実施。HGW経由の結果はONU直結の証拠ではない。homert実機、arm64実行、PVE/Kubernetes/Cloud全体も未受入。

## 実行順と合格条件

今回の実作業は現在の実HGWと隔離Linuxで進める。PVE/QEMUと追加のPBR調査は実行しない。以下の全体受入範囲と、今回実行する環境を混同しない。

### 1. 単体ルーターを端末からインターネットまで通す

homert02/03の一台分のintentと旧routerdの実装を照らし、native ResourceProgramへ明示する。互換converterは作らない。既存の例や試験fixtureを実設定の代わりにしない。

独立したHGW/ONU側とLAN端末で、次をひとつの構成として確認する。

- WAN DHCPv4、RA、DHCPv6-PD/Information、DUID/IAID、通常のDAD。
- LAN DHCPv4の動的pool、OUI/MAC scope、予約、router/DNS/domain。予約対象以外の取得値はACKから読む。
- LAN RA、SLAAC、RDNSS/DNSSL、stateless DHCPv6と、必要なstateful DHCPv6の実装状態を分けて確認する。
- 端末が取得したDNSを使うIPv4/IPv6 UDP/TCP名前解決、LAN名、DHCP由来record、条件付き転送、DoH、上流停止・復旧。
- IPv4 NAT/DS-Lite、IPv6 forwarding、4本のDS-Lite経路とbackup WAN、MTU 1454、MSS、DF/PTBを実通信とcaptureで確認する。
- 初回接続、自然lease/PD更新、PD変更・期限、worker/control/router再起動、WAN障害と復帰、通常停止後に端末通信とkernel stateが収束すること。

短時間の機能確認と自然更新を含む長時間確認は別に記録する。未観測の更新や対象機器の挙動を推定で合格にしない。

短時間native gateを、作業中の15-resource例で実HGWへ適用する再実行コマンド:
`make check-uplink DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18 UPLINK_WAN_MAC=02:72:6f:75:74:03 UPLINK_CONFIG=examples/single-router-dslite-dhcp-dns.yaml UPLINK_SECONDS=120 UPLINK_WAN_OUTAGE=1 UPLINK_FULL_RESTART=1` を使い、WAN DHCPv4は観測したlease、予約clientは設定した予約と照合する。OUI clientの動的割当はDHCP ACKから読み、宣言したscope内かを確認する。独立端末でDHCPv6 information、SLAAC、RAと一致する委譲IPv6 DNS listener、router A/AAAA、DHCP予約由来DNS、条件付き転送停止/復旧、DoH上流、DS-Lite/IPv6通信、WAN outage、PD workerとdaemon再起動後の復旧を確認する。この例のIPv4 leaseは12時間なので短時間gateではDHCPv4自然T1を主張しない。PD自然T1は上記の生成profile gateで別に完了している。native gateの合否と制限は実行後に追記する。

第6回はmain-derived build `8ea44d776dc3`、実HGWへつないだens18 virtio uplink、隔離macvlan router namespace、独立Linux LAN client 2台で実行し終了0。15-resource ResourceProgramを適用し、WAN DHCPv4 lease、予約/OUI scope DHCPv4、DHCPv6 Information、RA/SLAACと委譲DNSを取得値から確認した。LANからrouterのIPv4/IPv6 DNS、予約record、条件付きDNSの正常応答・上流停止時のfailure isolation・復旧が通った。routerのDoH既定経路と、独立clientのIPv4/IPv6 HTTPSおよびDNS A/AAAA UDP/TCPを確認。WANをblackholeしてIPv4/IPv6が失敗し、解除後0.79秒で復旧、PD identityを保持した。PD worker SIGKILL後1.57秒、graceful restart後3.53秒、全daemon restart後3.78秒で端末通信が復旧し、DUID/IAID/prefixを維持。停止時のzero-lifetime RA、委譲LAN addressのkernel withdrawal、上流DHCPv6 Releaseなしも確認した。12回の通信round中、自然PD Renew/Replyは0回（T1まで約7,176秒）、DHCPv4 T1未到達。試験stage/capture/namespaceは自動撤去済み。制限:15-resource profileだけで、連続TCP、110-resource homert、ONU直結、対象物理NIC qualificationではない。

### 2. 二WANと障害切替

独立したWAN peerと端末からHealthの送信元、interface、mark、実probeをcaptureし、Health→PBR/FIB→packetを照合する。通常・障害・復帰・両WAN断を通し、worker再起動、古い通知、元期限切れ、confirmed rollback後に新規TCP/UDPと既存flowを分けて確認する。片方のNAT sourceを別回線へ流さない。

### 3. 二台HA

単体で通ったnative profileを二台へ広げる。VRRP readiness/VIP、GARP/NA、DHCP/PD/DUID/IAID、DHCP lease、conntrack/NAT、既存TCP socket、UDP mapping、新規clientを両方向の電源断・復帰で確認する。短いnamespace合格をhomert二台や連続障害の合格にしない。homertで使うBGP、NTP、Tailscale、DNSも同じ端末利用経路へ統合する。

### 4. その他のwork package

routerdの対応実装・実運用を参照し、実peerを用いて次を個別に進める。homert試験の不具合で独立作業全体を止めない。

- SYS: sysctl/capability、NTP client/serverと時刻品質、権限分離/watchdog。
- NET: PPPoE、link/VRF/VXLAN、DHCPv6 stateful/relay、DNS ownership、BGP/BFD、WireGuard。
- CORE/OPS: ResourceProgram transaction/lifecycle、durable event、dynamic config/plugin、ControlAPI/CLI/Web、logging/retention/DPI。
- HA/EXT: VRRP/cluster、lease/PD/conntrack同期、Tailscale、IPsec。
- CLOUD/DIST: SAM/federation/provider、signed runtime・boot・更新・A/B、物理機・arm64、各cloud image、Kubernetes。
- QUAL: 全field・default・lifecycle・status・event、実通信、障害復旧、配布先の差分が閉じるまで初回製品全体を完了扱いにしない。

make check、Race Detector、vet、buildは変更ごとの補助確認としてMakefile経由で実行する。実通信の代替にしない。実行版、peer、設定profile、結果、未確認範囲をcommit前に照合し、まとまった変更ごとにcommit/pushする。

## 最新の実HGW自然更新試験

直近の実HGW自然更新gateは終了0。実HGWに試験専用MACのrouter namespaceを接続し、main由来build `7c1cb3619acb` と試験用生成profile、独立Linux LAN client 2台を使った。WAN/LAN設定とhost管理経路は変更していない。WAN captureでは自然Renewと対応Replyを1回ずつ観測し、client UDP/546→server/547とHGW UDP/49153→client/546、DUID・IAID=1・委譲prefixの一致、期限更新を確認した。initial DADはWAN/LAN両方で有効、global addressはnon-tentative、DAD failureなし。PD worker SIGKILL、graceful restart、daemon全体restart後の復旧は各1.32秒、3.61秒、3.99秒。zero-lifetime RAとupstream grantを保持したkernel withdrawal、両clientの自然DHCPv4更新、IPv4/IPv6 HTTPS・学習DNS UDP/TCPを676観測roundで確認。終了後にnamespace、capture、state、stageを削除した。制限:生成profileで、native full profile、連続TCP、ONU直結、対象物理NIC qualificationではない。

最初の5回で見つかった設定/runner不一致は、DNS server数を1件に限定した比較、B4 sourceの誤ったLAN `/64`割当、IPv6 host routeの不足だった。DNS受信は複数serverを許容し、B4 sourceをWAN `/128`、host routeをWAN RA由来のdefault routeとDS-Lite source bypassへ修正した。第6回でこの15-resource profileの短時間受入を完了した。過去のraw outputは保持せず、再現手順と現在の結果・限界のみを記録する。

## work package 対応

| 受入経路 | work package |
| --- | --- |
| homert単体・DNS/DHCP/RA/NAT/DS-Lite | CORE-01, NET-05, NET-06, QUAL-01 |
| 二WAN/Health/PBR/firewall/PPPoE | NET-01, NET-02, NET-03, NET-04 |
| BGP/BFD/WireGuard | NET-07, NET-08 |
| HA/lease/PD/conntrack | HA-01, HA-02, HA-03 |
| 設定/runtime/event | CORE-02, CORE-03, CORE-04, CORE-05, CORE-06 |
| OS/time/privilege | SYS-01, SYS-02, SYS-03 |
| management/observability | OPS-01, OPS-02, OPS-03 |
| IPsec/Tailscale | EXT-01, EXT-02 |
| provider/federation/SAM | CLOUD-01, CLOUD-02, CLOUD-03, CLOUD-04 |
| image/update/physical/cloud/Kubernetes | DIST-01, DIST-02, DIST-03, DIST-04 |

実機、回線、providerが使えない範囲は必要な接続・peerを明記し、使用可能な独立環境で実装を続ける。GitHub Actions完了を待たず、関連するローカル検査と実通信を完了する。
