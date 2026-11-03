# routerd2 実通信・製品受入計画

2026-11-03 05:13 UTC: 通常Makefile build ID `6ca951d3082a` + 本変更で、各47-resource profileを実HGWと独立BusyBox端末へ接続した両方向HA試験が終了0。現用daemon停止a→bは16.65秒、b→aは9.10秒でIPv4/IPv6 HTTPS・UDP/TCP DNS・広告されたIPv6 DNS・VIP NTPが復旧した（各1回の途中probe失敗）。同一公開HTTPS TCP接続は再接続なし、captureのSYNは1回、27応答・最大応答間隔7.96秒。同一UDP socketは31応答・13欠落・最大応答間隔7.51秒（1秒返信予算）。両方向でconntrack NAT tuple/mark、PD DUID/IAID/prefix、実DHCP Renew/ACKを維持し、旧現用の再加入・同期・非preemptを確認した。無停止・無損失の結果ではない。

実障害から修正したのは、PD同期counterの所有者ごとの比較、IPv6専用macvlanにも必要なARP identity設定、元現用不在時にDHCPを停止し続けるmarker/fenceの撤去。設定はRA DNS寿命1800秒、固定B4 SNAT、静的backupの共有VIP SNAT（転送client interfaceに限定）へ修正した。旧routerdのVMAC ARP設定・PD/lease同期経路も比較した。Race Detector・vet、通常build、3実行物のstatic link、Python補助検査36件が通過。追加の`--peer-loss`独立手順は今回未実行。

未受入: 二台profileの特別scope、条件付きDNS/DoH・BGP・Tailscale・履歴/telemetry全経路と長時間安定性、全homert/対象ONU/NIC。非同期DHCPの未受信割当を失った場合の重複回避は保証しない。自然PD T1は別の旧build/profileで観測継続中であり、本変更全体の資格には転用しない。旧buildの長時間観測でBGP等のworker再起動が増えており、effect ACK timeoutの原因は未解決。終了したHAのnamespace/process/key/stageは撤去済み。


更新: 2026-11-03 05:13 UTC。全体範囲は[実装計画](implementation-plan.md)と[34 work package](implementation-plan.json)。独立端末を接続しただけで通常通信が成立し、障害・再起動・更新から復旧する製品を完成させる。

## 実行原則

- 毎回の実通信直前にこの計画全体を読み、対象・peer・実行版・観測点・隔離・復旧を確認する。変更対象の旧routerd実装、caller、運用修正と保存中homert02/03の実設定を参照する。秘密は公開しない。
- 実product processとkernel、独立したpeer/clientを使う。DHCP/PD/RA、DNS、NATを個別に動かしただけで受入にせず、設定から端末のInternetまで通す。補助テスト数やcoverageを完成の代わりにしない。
- 動的address/prefix/DNS/identityは実応答から読み、IPv6はparseして比較する。固定値比較は明示した予約・管理addressなどに限る。packet、端末、kernel、workerを必要な範囲で照合し、製品・設定・peer・runnerの失敗を分けて原因を直す。
- 一つの待機で独立作業を止めない。関連チェックが通った単位でcommit/pushし、CIの完了を待たない。現在はPVE/QEMUと追加PBR調査を行わない。
- 残すのは再実行手順と簡潔な結果・限界。終了後にraw log/pcap/key/state/namespace/重複build outputを撤去する。長時間試験のlive ownerと最小の未解決資料は終了まで保護する。

## 現在の結果と限界

38-resource native profile、基準main `b03debf` + LAN配布設定、build ID `b03debf15982`で実HGW統合gateが終了0。通常・予約・OUI・特定MACの独立端末5台で、通常gateway/DNS .1と特別gateway/DNS .253を実ACKから設定し、公開HTTPS/UDP DNSが往復した。通常/予約端末はNTP2アドレスとdomain-searchも取得。実Releaseで3日分の保持期限が保存され、端末IP撤去後も別MACの同アドレス要求には別leaseを配り、元MACには保持アドレスを再配布した。全daemon restart後も特別端末3台のRenew/ACK・通信とfingerprint APIが復旧した。LAN RAのhigh優先度/MTU1500/valid最大7200秒/preferred最大3600秒を実packetで確認。上流選択、4 DS-Lite、DNS/DoH、nDPI/計数/SNI、fresh syslog/OTLP、BGP/NTP/Tailscaleも通過。PD worker SIGKILL/通常再起動後の復旧2.96/5.85秒、全daemon restartから全確認102.76秒（60秒抑制と30秒sampling等を含む）。2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。自然T1・既存socket継続・3日/7日経過削除・全homert/HA/実機資格は今回の範囲外。

製品Goは`b1a3e48`から不変。今回の`make race vet test-tools`（Python36件）、通常Makefile build、設定検証が終了0。3実行物は`readelf`でdynamic dependencyなし。nDPI source/license noticesも確認した。前checkpointの`make check`は補助根拠であり実通信の代替ではない。

保存中homert02/03は各110 resources。native単体例は38 resourcesで、件数一致を目標にせず実際の意図を照合する。全意図の受入と34 work package（in_progress22/planned12）は未完了。ens18/ens20はvirtioであり、NTT ONU直結・対象NIC・実機性能・arm64・HA・cloud/Kubernetesの資格へ読み替えない。

## 再実行と隔離

```sh
make check-uplink DOCKER='sudo -n docker' \
  UPLINK_INTERFACE=ens18 UPLINK_WAN_MAC=02:72:6f:75:74:03 \
  UPLINK_CONFIG=examples/single-router-dslite-dhcp-dns.yaml \
  UPLINK_SECONDS=20 UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1
```

必要setup: 許可済み実HGW uplink、sudo/root、iproute2、BusyBox、dhcpcd、dnsmasq、curl/dig/tcpdump、OpenSSL/SSH、FRR8.4.4/Zebra/vtysh、公式otelcol-contrib0.151.0（`/usr/local/bin/otelcol-contrib`）。Makefileが通常buildとchecksum-pinned Headscale/Tailscale1.104.2 peerを準備する。Makefile build済み実行物でrunnerだけを再実行する場合は同じ引数を`python3 scripts/uplink-smoke.py`へ渡し、実行build IDを記録する。

専用macvlan MACとnamespaceを使い、hostの管理address/default route/時計/trust storeを変更しない。LAN198.18.0.0/24、管理198.19.0.0/24、BGP192.0.2.0/24、tailnet underlay198.51.100.0/24へ隔離する。router側のlink/address/forward/NAT/serviceはnative設定が所有する。ens20や新規site IPは前提にしない。低優先候補は既存LAN上のgatewayであり、別WANのDHCPではない。

WAN/tailnet captureは各最大8MB、PD captureはudp546と実行時間で限定する。ownerは全peer/process/namespaceを撤去する。長時間用syslog受信は直近8MB、公式Collector file exporterは8MB×現行/backup1へ制限する（公式`validate`通過）。受信資料はraw archiveとして残さない。

## 同一profileで確認する経路

1. **WANと通常LAN。** 実HGWのDHCPv4、通常DADのRA/SLAAC、PD /60、DUID/IAID=1、委譲LANを確認する。LANの予約BusyBox端末と通常poolのdhcpcd端末がgateway/DNS、NTP2アドレス/domain-search、SLAAC/RDNSS/DNSSL/RA-triggered DHCPv6 Informationを取得し、両IPのInternetを使う。保存中homertのDHCPv6Serverはstatelessであり、不要なstateful poolを新設しない。
2. **特別scopeと保持。** 実homertの3 selectorsは同じprofileを参照するため、一つのpool ownerへOUIと6 MACをまとめる。通常pool64..191/予約はgateway/DNS .1、特別pool32..63は .253。OUI端末1台と特定MAC端末2台を独立namespaceへ接続し、配布値の設定だけで特別gatewayの実DNS/HTTPSを使う。特別profileに通常poolのNTP/optionsを勝手に継承しない。3日sticky/7日daily fingerprint保持を設定し、実Releaseと端末IP撤去、保存期限、別MACの同IP要求、元MACの再取得を確認する。DBはread-onlyで観測し、stateを捏造しない。全daemon restart後にも特別端末のRenew/ACK・通信・fingerprint APIを確認する。3日/7日経過削除は別の受入。
3. **LAN RA。** high preference、MTU1500、valid最大7200秒/preferred最大3600秒をPD所有期限で制約する。DNS広告寿命1800秒を設定し、0秒以外のRDNSS/DNSSLとそのIPv6 DNSへの実queryを確認する。LAN送信元link-localの実packet、端末の通常DAD/SLAAC/DNS、正常停止時のzero-lifetime RAとkernel撤去を確認する。
4. **DNS。** 宣言順のsource評価を旧routerdから維持し、子suffixを親より先に置く。独立dnsmasq5353/5354で異なるAを返し、通常回答、片側停止の切替、両側停止中のcacheと未取得名SERVFAIL、復旧を両IPで確認する。複数suffix/共有upstream、WAN PD学習DNS、Cloudflare/Google両DoHの実probe、local/lease A/AAAA、UDP/TCP、loopback/管理listener、履歴API/Webも確認する。cache比較だけ`+nocookie`でquery keyを揃える。peerの起動は実queryへの応答まで確認する。全private suffix実peerと個々のDoH停止切替は別の受入。
5. **4 DS-Liteと上流選択。** 同じWANの別委譲/128（suffix ::21/::22/::13/::23）からAFTR ::100/::101/::102/::100へ接続し、各tunnelにbindした実TCP/HTTPS・UDP/DNSを確認する。内側192.0.0.2/32へ明示SNAT（リンク停止で消えるmasqueradeを使わない）、MTU/MSS1454/DF処理、IPv6 host route table120とsource bypassを設定する。LAN新規通信は既存EgressPolicyのHealth/owner/markとweight120（3本）→80（4本目）→60（LAN static gateway）、60秒hysteresisで選ぶ。metric200..203はrouter起点の個別疎通用。試験専用nft tableで外側sourceを順に遮断し、Health・kernel規則・実conntrack mark/next hop・両通常端末の新規TCP/UDP、解除後の復帰を確認する。Linux static gatewayは同じHGWへ別MACでDHCPv4接続して転送/NAT/DNSを提供し、物理独立二回線の資格とは分ける。遮断tableはfinallyで撤去する。実設定にないPPPoE終端や別NICを新設しない。
6. **BGP・NTP。** 独立FRR/Zebra2台でLAN198.18.0.0/24とpeer203.0.113.0/24を相互広告し、受信RIB・両kernel・product best pathでLAN2端末のNATなしTCP/UDPを確認する。各bgpdを一台ずつ停止/再加入し、残るpeerの通信と復旧を確認する。特定peerの初期選択や切戻しを要求しない。長時間GR期限・両peer同時断・既存socket無損失継続は別の受入。NTPは実WAN学習源と独立chronyd2 clientの応答を確認し、`-x`/`-Q`でhost時計を変更しない。fallbackとappliance時計補正は別の受入。
7. **管理・Tailscale。** 一時鍵で管理SSH `show status`、Web履歴、LANからWebへの拒否、初回management generation1/restarts0、再起動後の鍵/履歴を確認する。独立Headscale/TailscaleへIPv4 LAN/exitを広告し、acceptDNS/acceptRoutes=falseとupstream既定のsubnet SNATを維持する。OS標準CAと試験CAはownerのprivate mount内で結合し、auth key/control DBをprivate stageへ置く。LAN→peer両IP、peer→LAN IPv4、両IP exit HTTPS/UDP DNSとnode identity保持を確認する。LAN→tailnet IPv6は明示NATであり、delegated IPv6 subnetの双方向広告ではない。public control/hostname bootstrapは別の受入。
8. **運用と復旧。** INFO syslog/OTLP、logs/metrics/traces、group1 NFLOG、group2 native nDPI/flow history、7日/daily/vacuumを既存ownerへ接続する。独立BGP peer上にsyslog UDP5514と公式Collector gRPC4317をdaemonより先に待受させる。実公開通信の双方向計数・native由来・TLS SNI、実管理拒否のhistory、観測開始後の新しいHealth syslog/log/metric/spanを照合する。samplingは旧既定30秒で、短flow全件保存・lossless deliveryを主張しない。PD worker SIGKILL/通常再起動と全daemon再起動後に上記の通信・identity・履歴・更新を確認し、正常停止時には上流Releaseなしでprefixを撤去する。

## 自然更新と並行するHA基盤の通信継続

基準main `9fb64f4`の通常実行物、build ID `9fb64f4267e9`で`make check-ha-state`が終了0。独立Linux namespace/PID1の2 product processとclient/serverで、A→B/B→AのVRRP/NAT引継ぎ、同一TCP socketとUDP mapping、同期済みDHCP leaseの実Renew/ACK、worker/conntrack engine再起動を確認した。TCP再接続なし（captureのSYNは1回）、最大応答待ち1.665秒。UDPは50ms送信/200ms返信予算で3009送信・2942受信・67期限超過、最大無応答1.110秒（このfixture条件での測定）。同期断中の独立割当/更新と復帰、未設定証明書の拒否、実lease期限での失効、3日sticky期限を変えない再起動/引継ぎ、保持中の別client割当と元client再取得も通過。設定削除で同期workerを回収し、元のconntrack sysctlへ復元した。実HGWの38-resource全体を二台化した結果や、14-resource例・実homert二台の資格とは分ける。

`make check-ha-state DOCKER='sudo -n docker'`で、既存の独立Linux namespace/PID1の2 product process、VRRP/共有WAN・LAN identity、conntrackd FTFW、相互認証TLSのDHCP同期を確認する。追加PVE/QEMUやPBR/二WAN切替のflagsは使わない。自然更新ownerのMAC・namespace・プロセス・時計へ触れない。署名bundleは通常Makefile build/publisherとpinned engineから作る。

独立peerのTCPは再接続しないsocket、UDPも同一socketで送受信し、両方向takeover、worker/engine再起動、同期断中のDHCP更新、同期復帰、lease期限/削除/保持を観測する。packetでTCP SYN回数、UDP NAT tuple、DHCP Request/ACK、同期交通を照合する。TCP/UDPの継続clientは明示した静的LANアドレスを使い、DHCP clientは別に実割当/更新を確認する。fixtureのtcp_loose=0はmidstreamの拾い直しをstate同期と誤認しないための限定したkernel条件で、製品が明示するliberal modeはhomertどおり適用する。UDPは50ms送信・200ms返信予算を観測条件として記録し、これ自体を全環境の製品性能条件にしない。

このgateはnative HA/NAT/DHCP基盤の受入であり、現在の38-resource実HGW単体全体の二台化や実homert二台の資格とは分ける。14-resource homert examplesのWAN /128 B4 source、NTT向けMTU1454/inner IPv4、RA/保持を単体確認済み契約に合わせるが、設定検証だけでそのHA例の全通信を資格済みとはしない。private stage/log/pcapはgate終了後の簡潔な記録/統合を終えて撤去する。

## 実HGWへ接続した二台profileの統合

次の実行対象は`examples/homert02-native.yaml`と`examples/homert03-native.yaml`の全機能二台profile。各47 resourcesへ拡張し、下記の初期通信を実行済み。従来の14-resource例やHA基盤gateの合格を転用しない。既存のDS-Lite群120→別DS-Lite80→LAN静的gateway60をそのまま用い、PPPoEやens20を新設しない。

`make check-uplink-ha DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18`で通常buildと設定検証後に実行する。まず実HGWのRA/PD、共有WAN/LAN IPv6 identity、VRRPの準備完了後公開、実DHCP端末の両IP HTTPS・UDP/TCP DNS、現用daemon停止後の引継ぎを通す。FRR2台、DNS、chronyd、Headscale、syslog/Collectorを独立peerとして配置する。全機能の個別受入、逆方向復帰、既存socket継続はこの最初のbring-upだけでは合格にしない。

隔離: private mountと専用namespace/bridgeを用い、ens18へ専用macvlanで接続する。共有WAN MACは`02:00:5e:00:01:43`、静的gateway用HGW接続は`02:72:6f:75:74:06`。自然更新ownerのMAC/protocol/stateへ触れない。hostの管理address/default route/時計は変更しない。各実行は最大600秒、peer/productのstdoutと状態はprivate stageのみとし、終了時にprocess/namespace/key/stageを撤去する。失敗の原因と必要な修正は簡潔に残す。

2026-11-03 04:12 UTC: 通常Makefile build ID `a3444d519fb0`（製品Goはwildcard NTP listener修正`19682da`を含む）と各47-resource二台profileで、実HGWの共有WAN RA/PD、共有LAN IPv6 identity、VRRP準備後公開、実DHCP端末のIPv4/IPv6 HTTPS・UDP/TCP DNS・独立chronyd `-Q`によるVIP NTP応答が通過した。現用aの正常停止後、同じ端末設定のままbで全項目が復旧し、停止開始から再確認まで17.01秒、途中の試行失敗3回。RA/NA packetとkernelも観測した。無停止・既存socket継続・逆方向復帰・全機能安定性の受入ではない。初期のVIP DNS拒否は`accept_mode: true`で修正した。単発失敗で試験を止める条件は廃し、断と復旧を記録する。

再実行手順: 同じ設定で現用停止→待機側への復旧→旧現用の再加入を両方向へ進める。独立Python標準ライブラリのclientから公開HTTPSへTCP接続を一度だけ張り、公開DNSへ同じUDP socketで送信し続ける。TCPの再接続を禁止し、両nodeの実conntrack reply tupleとclient captureのSYNを照合する。UDPは1秒の返信期限で欠落と最大応答間隔を記録する。これは無損失条件ではない。DUID/IAID/prefixと同期済みDHCP leaseの実Renew/ACK、再加入後の非preemptも確認する。

DHCPの現用権はVRRPが決める。元現用が停止しても、同期済みの保存leaseから配布を再開して実Renew/ACKを返す。同期は旧routerdと同じ非同期のため、未受信の新規割当を失った場合の重複回避までは保証しない。`--peer-loss`の既存namespace手順も、元現用不在中の既知lease更新と、同期復帰後の新規割当を確認する契約へ更新する。

残作業: 特別scope、DNS/DoH・BGP・Tailscale・履歴/telemetry全経路と再起動後の安定性を同じ二台profileで受け入れる。初期bring-upの一度だけ、BGP/VRRP effect ACK待ちのtimeoutでworker再起動を観測したため、全機能時の適用/停止順序も次の確認対象とする。

## 自然更新と残る全体受入

進行中（2026-11-03T03:25:30Z）: 設定/runner `5861b7e`、通常build ID `b03debf15982`（製品Go不変）で`--seconds 7800 --full-restart --tailnet`の自然更新観測を起動した。live ownerと現在のPD bound/IAID1を実プロセス・IPCで確認済み。`/tmp/routerd2-natural-uplink.log`が現在の進捗。終了0/自然Renew/Replyはまだ未確認。重複起動やlive stageの削除をせず、実プロセスを再確認して観測を継続する。待機中は残る単体/HA作業を進める。

自然PD T1は同じprofileで`UPLINK_SECONDS=7800`など実際の`renew_at`へ達する時間を選び、packetのRenew/Reply、DUID/IAID/prefixと更新期限、端末の通信・lease更新を確認する。短時間gateで代用しない。既存TCP socket/UDP mappingの継続は新規接続と別に観測する。自然更新待ちには独立する実装を進める。

別checkpointの限定根拠: 生成profile/build`7c1cb3619acb`はHGW source49153からの自然PD T1、同一DUID/IAID/prefix、期限更新、通常DAD、両端末の自然DHCPv4更新と676通信roundを確認済み。15-resource native/build`8ea44d776dc3`はWAN遮断解除後0.79秒の復旧を確認済み。実supervisor/control/network/Healthと独立二WANのTCP/UDP・packetによる障害復帰/worker再起動/古い通知/期限切れ/confirmed rollback、および限定Alpine/PID1 PVEの独立PPPoE/ISC DHCPv6/通信/署名更新rollbackも別gateで確認済み。現在の全profile・HA・実機資格には流用せず、今回は追加実行しない。raw stageは撤去済み。

1. **homert単体の全意図を閉じる。** 保存中の両110-resource設定を機能/caller/kernel effectへ照合する。残るprivate DNS peer、BGP契約、LAN/運用/保持dataset、全設定のcold start・自然更新・MTU/MSS/DF/PTB・PD変更/期限・通信継続・worker/control/daemon復旧を受け入れる。converterや旧prototype API維持を目的にしない。
2. **二台HA。** VRRP readiness/VIP/GARP/NA、DHCP/PD/DUID/IAID・lease同期、conntrack/NAT、既存TCP/UDP mapping、新規端末、DNS/BGP/NTP/Tailscaleを両方向停止/復帰で確認する。単体namespaceの成功を実homert二台へ読み替えない。
3. **全34 work package。** SYS（OS/capability/time/privilege/watchdog）、NET（PPPoE/link/VRF/VXLAN/DHCPv6 stateful/relay/DNS/BGP/BFD/WireGuard）、CORE/OPS（transaction/lifecycle/event/dynamic config/plugin/API/CLI/Web/logging/retention/DPI）、HA/EXT（同期/IPsec/Tailscale）、CLOUD（provider/federation/SAM）、DIST（signed runtime/boot/更新/A-B/実機/arm64/cloud image/Kubernetes）を各packageの受入条件で実peerへ接続する。一つの不具合で独立作業を止めない。
4. **QUAL。** 全field/default/lifecycle/status/eventと明示的置換、各配布先の通信・障害復旧・性能・更新rollbackを照合する。対象ONU/NIC/providerが使えなければ最小の不足setupと未受入範囲を残す。本番変更/課金は実際のauthorization内で行い、全条件が実証されるまで全体完成を宣言しない。
