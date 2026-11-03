# routerd2 実通信・製品受入計画

更新: 2026-11-03 01:39 UTC。全体範囲は[実装計画](implementation-plan.md)と[34 work package](implementation-plan.json)。独立端末を接続しただけで通常通信が成立し、障害・再起動・更新から復旧する製品を完成させる。

## 判定と実行原則

- 着手する機能の旧routerd実装・caller・運用修正とhomert02/03の実設定を読む。設定値・秘密は公開しない。
- 毎回の実通信直前にこの計画全体を読み直し、対象・既知の問題・peer・実行版・観測点・隔離と復旧手順を確認する。現状とのずれを先に直す。
- 実product processとkernel、独立したpeer/clientを使う。DHCP/PD/RA、DNS、NATを個別に動かしただけで受入にせず、設定から端末のInternetまで通す。
- 動的address/prefix/DNS/identityは実応答から取得する。固定値と比較するのは明示した予約・管理addressなどに限る。同じIPv6はparseして比較する。
- packet、端末結果、kernel FIB/firewall、worker状態を必要な範囲で照合する。異常時は原因を決める観測を行い、原因不明の同じ再試行や試験機構の増築を続けない。
- 試験側の失敗と製品の失敗を区別する。未実行・未確認を合格にせず、短時間の通信を自然更新・既存TCP継続・実機資格へ読み替えない。
- 自然更新待ちの間は独立する実装を進める。関連チェックが通った単位でcommit/pushし、CIの完了を待たない。
- 試験後は再実行手順と簡潔な結果・限界だけを残し、不要なraw log/pcap/key/state/namespace/build outputを削除する。現在はPVE/QEMUと追加のPBR調査を実行しない。

## 現在の実通信と限界

25-resource native profile、基準main `5b46865` + OS trust-store修正、build ID `5b4686577c4a`で実HGW/独立FRR・Headscale・Tailscale・Linux clientの統合gateが終了0。CA指定を省略してHTTPS登録でき、LAN→tailnet peerのIPv4/IPv6 TCP/UDP、peer→広告LANのIPv4 TCP/UDP、product exit node経由のIPv4/IPv6 HTTPS・UDP DNSが通った。全daemon再起動後もnode identityを保持して同じ通信が復旧した。BGPの相互広告・両kernelの経路・LAN 2端末のNATなしTCP/UDP、DNS/cache/history・SSH/Web・WAN学習NTP源stratum 3と両IP Internetも同時に通過。PD worker SIGKILL/通常再起動後1.89/5.09秒で端末通信が戻り、full restartから全確認完了17.80秒（NTP測定を含む）。2通信round、zero-lifetime RA/kernel withdrawal、upstream Releaseなしを確認。host CA bundleのSHA-256は試験前後で一致。stage/key/CA mount/control DB/capture/namespaceは撤去済み。

`make check`（Go/Python、Race Detector、vet）、通常Makefile build、設定検証が終了0。3実行物は`readelf`でdynamic dependencyなし、nDPI source/license noticesを確認した。補助検査は実通信の代替ではない。

homert02/03の保存中の実設定は各110 resources。native単体例は25 resourcesであり、resource数の一致を移植目標にはしないが、全意図の受入は未完了。34 work packageもin_progress22/planned12のまま。ens18/ens20はvirtio NICで、NTT ONU直結、対象NIC/実機、arm64、HA、cloud/Kubernetes全体を資格済みとは扱わない。

## 現行単体gateの再実行

```sh
make check-uplink DOCKER='sudo -n docker' \
  UPLINK_INTERFACE=ens18 UPLINK_WAN_MAC=02:72:6f:75:74:03 \
  UPLINK_CONFIG=examples/single-router-dslite-dhcp-dns.yaml \
  UPLINK_SECONDS=20 UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1
```

必要setup: 許可された実HGW uplink、sudo/root、iproute2、BusyBox、dhcpcd、dnsmasq、curl/dig/tcpdump、OpenSSL/SSH、FRR/Zebra/vtysh。Makefileが製品とengineを通常buildし、checksum-pinnedのHeadscale/Tailscale peerを準備する。host管理address/route/clockを変更せず、専用macvlan MACとnamespaceを使う。LANはbenchmark prefix198.18.0.0/24、管理は198.19.0.0/24、BGP peerは192.0.2.0/24、tailnet control/peerは198.51.100.0/24へ隔離する。router側のlink/address/forward/NAT/serviceはnative profileが所有し、runnerで追加変換しない。

独立peer setupは次のとおり。

- LAN: BusyBox予約clientとdhcpcdのOUI scope client。取得値をACK/実addressから読む。通常のDADを有効にし、DHCPv6 Information/SLAAC/RDNSS/DNSSLを確認する。
- DNS: dnsmasqの独立上流を停止・復旧し、両IPのcacheと未取得名のSERVFAIL隔離を確認する。cache確認だけ`dig +nocookie`を使い、毎回異なるCookieを別cache keyにする試験側の不一致を避ける。通常DNSのUDP/TCP、local/reservation A/AAAA、DoH、loopback/管理DNSと履歴API/Webも確認する。
- 管理: 一時鍵でSSH `show status`、Web履歴とLANからWebへの遮断。SSHは管理address設定前から指定addressで待受し、初回generation=1/restarts=0。再起動後もhost key・履歴を保持する。
- BGP: 独立FRR8.4.4/ZebraとLAN198.18.0.0/24/peer203.0.113.0/24を相互広告し、両kernelのnext hop、独立FRR受信RIB、LAN 2端末のNATなしTCP/UDPを確認する。旧routerdのexport/connected allowlistをまとめる明示広告、import nextHopRewrite、GR intentを既存engineへ設定する。1 IPv4 peerの試験で、2 peers全体や長時間GR保持/期限は未受入。
- NTP: 実WAN学習源へのfreshな同期と独立chronyd2 clientへの応答。host modeのengine/peerは`-x`、clientは`-Q`で時計を変更しない。fallback選択とappliance時計補正は別確認。
- Tailscale: 独立Headscale/Tailscale1.104.2。LAN IPv4 subnetとexitを広告し、acceptDNS/acceptRoutes=falseを保つ。OS標準CAの引継ぎを確認するため、ownerだけのmountへ元のCAと試験CAを合わせる。hostのtrust storeは変えず、auth key/control DBはprivate `/run`/stageへ置く。
- Tailscaleのpeer→LANはupstream既定のsubnet SNATを使う。旧routerdも既定値を上書きしていない。LAN→peer IPv6は明示NATでproductのtailnet addressを使い、IPv6 LAN subnetの双方向広告とは区別する。両IP exit HTTPS/UDP DNS、node identityと再起動後の通信も確認する。public controlとcontrol hostname bootstrap DNSは未受入。

WAN captureは最大8MB、tailnet captureも最大8MBへ制限する。PD captureは限定filterと実行時間で抑える。ownerのprivate mount/namespace/key/lease/state/captureは終了時に撤去する。`UPLINK_SECONDS=7800`など自然T1へ達する時間で実行すれば同じnative profileの更新も観測できるが、短時間gateの合格で代用しない。

## 別に確認済みの範囲

- 生成profile、build`7c1cb3619acb`で自然PD T1 Renew/Replyをcaptureした。client546→server547、HGWのReplyは49153→546。DUID/IAID=1/prefixが同じで期限更新、通常DAD、両clientの自然DHCPv4更新と676通信roundを確認。native全体の自然T1には流用しない。raw stageは削除済み。
- 15-resource native profile、build`8ea44d776dc3`でWAN blackhole時の両IP失敗と解除後0.79秒の復旧を確認。今回の追加機能で同じ遮断試験を繰り返す理由にはしない。
- 実supervisor/control/network/Healthと独立二WAN/Linux peerのTCP/UDP・packetを使うHealth→PBR gateで、障害復帰、worker/supervisor restart、古い通知、期限切れ、confirmed rollbackを確認した。対象二回線/HAの資格ではない。
- 既存の通常Alpine/PID1隔離PVE gateで、独立PPPoE/ISC DHCPv6/Linux clientのDHCP/PD/Renew/SLAAC・両IP通信、署名更新、明示/自動rollbackを確認済み。VMは撤去済み。今回は追加実行しない。

## 残作業の実行順と合格条件

1. **homert単体の全経路を接続する。** 両node設定の110 resourcesを機能/caller/kernel effectへ対応させ、複数DNS suffix/upstream/learnedDNS、4 DS-Lite経路とbackup WAN、BGP2 peers、残るLAN scope/identity/telemetry・運用機能を埋める。converterや旧routerd2 API維持を目的にしない。
2. **同一単体profileでcold startから通常利用・復旧まで受け入れる。** WAN DHCPv4/RA/PD/Informationと自然更新、DUID/IAID、LAN DHCPv4のdynamic/OUI/MAC/予約、stateful DHCPv6が必要な範囲、RA/SLAAC/RDNSS/DNSSL、DNS各経路と両IPInternet、NAT/DS-Lite、MTU/MSS/DF/PTB、PD変更/期限・worker/control/daemon restart・通常停止後の収束を確認する。新規flowと継続TCP/UDPを区別する。
3. **二台HAを通す。** VRRP readiness/VIP、GARP/NA、DHCP/PD/DUID/IAID・lease同期、conntrack/NAT、既存TCP socket/UDP mapping、新規端末、DNS/BGP/NTP/Tailscaleを両方向停止・復帰で確認する。単体や短いnamespace合格を実homert二台へ読み替えない。
4. **残るwork packageを閉じる。** SYS（OS/capability/time/privilege/watchdog）、NET（PPPoE/link/VRF/VXLAN/DHCPv6 stateful/relay/DNS/BGP/BFD/WireGuard）、CORE/OPS（transaction/lifecycle/event/dynamic config/plugin/API/CLI/Web/logging/retention/DPI）、HA/EXT（同期/IPsec/Tailscale）、CLOUD（provider/federation/SAM）、DIST（signed runtime/boot/更新/A-B/実機/arm64/cloud image/Kubernetes）を[各packageの受入条件](implementation-plan.json)で実peerへ接続する。一つの待機・不具合で独立作業を止めない。
5. **QUALを確定する。** 全field/default/lifecycle/status/eventと明示的置換、各配布先の通信・障害復旧・性能・更新rollbackを照合する。対象ONU/NICやproviderを使えない場合は最小の不足setupと未受入範囲を残し、全体完成は宣言しない。本番変更・課金は実際のauthorization内で行う。
