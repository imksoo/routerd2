# routerd2 実通信・製品受入計画

更新: 2026-11-03 12:28 UTC。全体範囲は[実装計画](implementation-plan.md)と[34 work package](implementation-plan.json)。実行済み版・結果・限界は[引き継ぎ](pve-codex-handoff.md)へ残す。独立端末を接続しただけで通常通信が成立し、障害・再起動・更新から復旧する製品を完成させる。

## 実行原則

- 毎回の実通信直前にこの計画全体を読み、対象・peer・実行版・観測点・隔離・復旧を確認する。変更対象の旧routerd実装、caller、運用修正と保存中homert02/03の実設定を参照する。秘密は公開しない。
- 実product processとkernel、独立したpeer/clientを使う。DHCP/PD/RA、DNS、NATを個別に動かしただけで受入にせず、設定から端末のInternetまで通す。補助テスト数やcoverageを完成の代わりにしない。
- 動的address/prefix/DNS/identityは実応答から読み、IPv6はparseして比較する。固定値比較は明示した予約・管理addressなどに限る。packet、端末、kernel、workerを必要な範囲で照合し、製品・設定・peer・runnerの失敗を分けて原因を直す。
- 一つの待機で独立作業を止めない。関連チェックが通った単位でcommit/pushし、CIの完了を待たない。現在はPVE/QEMUと追加PBR調査を行わない。
- 残すのは再実行手順と簡潔な結果・限界。終了後にraw log/pcap/key/state/namespace/重複build outputを撤去する。長時間試験のlive ownerと最小の未解決資料は終了まで保護する。

## 現在の結果と限界

基準main `1fa7cf0` + 候補設定の観測保持修正、通常build `1fa7cf099ae0`で単体39-resourceの実HGW手順が終了0。30秒のconfirmed commitでcoreのAレコードとLAN cache設定を変更し、両IP/UDP/TCPの候補回答と公開通信、無確認期限切れとcontrol SIGKILL後の元intent全体・回答・公開通信への復帰を確認した。候補疎通確認後から元の疎通を再確認するまで33.42秒/9.30秒で、転送断時間ではない。DNS編集と期限切れrollbackではWAN DHCPv4/RA/PDのPIDを維持した。PD SIGKILL3.21秒/通常再起動6.72秒、全daemon後の全項目再確認103.04秒も通過した。 `make check`（Race Detector/vet/補助36件）、最終のruntimeplan Race、通常Makefile buildと3実行物のstatic linkが通過。これは設定transactionと実通信の受入であり、署名runtimeの更新/boot、HA中の設定変更、全homert private peer、全34 packageの完成には数えない。

基準main `9fe72fe` + 二段DNS設定、製品Go不変の通常build `df490351c3ff`で単体39-resource実HGW手順が終了0。LAN resolver（cache1000・queryLogなし）からcore resolver（cache10000・queryLogあり）へloopback UDP53で転送し、coreがzone/条件付き/学習DNS/DoHを所有する。実端末からの新しいqueryと同じIDのloopback往復・回答をcaptureで照合した。両IPのDNS/Internet、条件付き上流の停止/cache/SERVFAIL/復旧、coreのcache hit/履歴API/Web、DHCP各scope・sticky、BGP/NTP/Tailscale/運用記録が通過。PD SIGKILL3.16秒/通常再起動7.06秒、全daemon再起動後の全項目再確認103.15秒。後者は抑制/samplingを含み転送断時間ではない。自然PD更新0回・通信round2の短時間実行であり、長時間安定性や全homert private peerの受入には数えない。

終了0（2026-11-03 07:42〜09:46 UTC）: source/build `df49035`/`df490351c3ff`、単体38-resource profileで実HGWとの自然Renew/Reply1組と期限更新、623巡回の両IP HTTPS・UDP/TCP DNSを確認した。自然更新後の619巡回時点まで全28 worker generation1/Ready=trueを観測。PD SIGKILL2.90秒/通常再起動5.92秒、全daemon後の全項目再確認105.73秒も通過し、identityと両端末/Tailscale通信、停止時のRA撤去を確認した。105.73秒は抑制/samplingを含む全項目時間で、転送断時間ではない。owner/process/namespace/stageは撤去済み。二段DNS・候補設定修正の別受入、既存socket継続、全homert/実機資格とは範囲を区別する。

同じ二段DNSの各48-resource二台profileも実HGWで終了0。両方向のDNS/DoH・履歴API/Web、DHCP更新/Release/再取得・sticky/fingerprint、PD identity、BGP/NTP/Tailscale/exitと運用記録を確認。同一TCPはSYN1回・209応答・最大応答間隔10.89秒、UDP239応答・19期限超過・最大応答間隔10.52秒（1秒返信予算）。全項目確認58.91/73.57秒はsampling/再取得を含み、転送断時間ではない。二台のprocess/namespace/stageは撤去済み。短時間の両方向受入であり、長時間安定性・全private DNS peer・実機資格は残る。 先行する47-resource profileは両方向の同一TCP/UDP、DHCP scope/sticky/fingerprint、PD identity、DNS/DoH/BGP/NTP/Tailscale、管理/運用記録が終了0。停止中heartbeat、共通IPC3秒打切り、AFTR入力/通知の並列所有、古いsupervisor状態cacheを修正後、観測した停止理由でAFTR409/BGP/VRRP timeoutは未再発。長時間安定性は引き続き確認する。

保存中homert02/03は各110 resourcesで、全意図と34 work packageは未完了。公開DoHの片側障害・復旧は下記の独立node手順で受入済み。全private DNS peer、残るBGP/運用/保持/配布と実機資格は残る。ens18/ens20のvirtio試験をNTT ONU直結・対象NIC・性能・arm64・cloud/Kubernetesへ読み替えない。

## 再実行と隔離

```sh
make check-uplink DOCKER='sudo -n docker' \
  UPLINK_INTERFACE=ens18 UPLINK_WAN_MAC=02:72:6f:75:74:03 \
  UPLINK_CONFIG=examples/single-router-dslite-dhcp-dns.yaml \
  UPLINK_SECONDS=20 UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1
```

必要setup: 許可済み実HGW uplink、sudo/root、iproute2、BusyBox、dhcpcd、dnsmasq、curl/dig/tcpdump、OpenSSL/SSH、FRR8.4.4/Zebra/vtysh、公式otelcol-contrib0.151.0（`/usr/local/bin/otelcol-contrib`）。Makefileが通常buildとchecksum-pinned Headscale/Tailscale1.104.2 peerを準備する。Makefile build済み実行物でrunnerだけを再実行する場合は同じ引数を`python3 scripts/uplink-smoke.py`へ渡し、実行build IDを記録する。

専用macvlan MACとnamespaceを使い、hostの管理address/default route/時計/trust storeを変更しない。LAN198.18.0.0/24、管理198.19.0.0/24、BGP192.0.2.0/24、tailnet underlay198.51.100.0/24へ隔離する。router側のlink/address/forward/NAT/serviceはnative設定が所有する。保存設定のens20（homert02）/eth2（homert03）は管理interfaceであり、追加WANではない。上流は設定された候補の優先順位と疎通状態で選び、NIC追加を条件にしない。低優先候補は既存LAN上のgatewayであり、別WANのDHCPではない。保存設定の実際の順序はDS-Lite 3本（weight120）→別DS-Lite（80）→LAN静的gateway（60）で、PPPoE候補はない。

WAN/tailnet captureは各最大8MB、PD captureはudp546と実行時間で限定する。ownerは全peer/process/namespaceを撤去する。長時間用syslog受信は直近8MB、公式Collector file exporterは8MB×現行/backup1へ制限する（公式`validate`通過）。受信資料はraw archiveとして残さない。

## 同一profileで確認する経路

1. **WANと通常LAN。** 実HGWのDHCPv4、通常DADのRA/SLAAC、PD /60、DUID/IAID=1、委譲LANを確認する。LANの予約BusyBox端末と通常poolのdhcpcd端末がgateway/DNS、NTP2アドレス/domain-search、SLAAC/RDNSS/DNSSL/RA-triggered DHCPv6 Informationを取得し、両IPのInternetを使う。保存中homertのDHCPv6Serverはstatelessであり、不要なstateful poolを新設しない。
2. **特別scopeと保持。** 実homertの3 selectorsは同じprofileを参照するため、一つのpool ownerへOUIと6 MACをまとめる。通常pool64..191/予約はgateway/DNS .1、特別pool32..63は .253。OUI端末1台と特定MAC端末2台を独立namespaceへ接続し、配布値の設定だけで特別gatewayの実DNS/HTTPSを使う。特別profileに通常poolのNTP/optionsを勝手に継承しない。3日sticky/7日daily fingerprint保持を設定し、実Releaseと端末IP撤去、保存期限、別MACの同IP要求、元MACの再取得を確認する。DBはread-onlyで観測し、stateを捏造しない。全daemon restart後にも特別端末のRenew/ACK・通信・fingerprint APIを確認する。3日/7日経過削除は別の受入。
3. **LAN RA。** high preference、MTU1500、valid最大7200秒/preferred最大3600秒をPD所有期限で制約する。DNS広告寿命1800秒を設定し、0秒以外のRDNSS/DNSSLとそのIPv6 DNSへの実queryを確認する。LAN送信元link-localの実packet、端末の通常DAD/SLAAC/DNS、正常停止時のzero-lifetime RAとkernel撤去を確認する。
4. **DNS。** homertと同じLAN→loopback coreの二段構成を使い、LAN/core別のcacheとcoreの履歴を確認する。宣言順のsource評価を旧routerdから維持し、子suffixを親より先に置く。独立dnsmasq5353/5354で異なるAを返し、通常回答、片側停止の切替、両側停止中のcacheと未取得名SERVFAIL、復旧を両IPで確認する。複数suffix/共有upstream、WAN PD学習DNS、Cloudflare/Google両DoHの実probe、local/lease A/AAAA、UDP/TCP、loopback/管理listener、履歴API/Webも確認する。cache比較だけ`+nocookie`でquery keyを揃える。peerの起動は実queryへの応答まで確認する。全private suffix実peerは残作業。個々の公開DoH停止・復旧は下記の独立node手順で確認する。
5. **4 DS-Liteと上流選択。** 同じWANの別委譲/128（suffix ::21/::22/::13/::23）からAFTR ::100/::101/::102/::100へ接続し、各tunnelにbindした実TCP/HTTPS・UDP/DNSを確認する。内側192.0.0.2/32へ明示SNAT（リンク停止で消えるmasqueradeを使わない）、MTU/MSS1454/DF処理、IPv6 host route table120とsource bypassを設定する。LAN新規通信は既存EgressPolicyのHealth/owner/markとweight120（3本）→80（4本目）→60（LAN static gateway）、60秒hysteresisで選ぶ。metric200..203はrouter起点の個別疎通用。試験専用nft tableで外側sourceを順に遮断し、Health・kernel規則・実conntrack mark/next hop・両通常端末の新規TCP/UDP、解除後の復帰を確認する。Linux static gatewayは同じHGWへ別MACでDHCPv4接続して転送/NAT/DNSを提供し、物理独立二回線の資格とは分ける。遮断tableはfinallyで撤去する。実設定にないPPPoE終端や別NICを新設しない。
6. **BGP・NTP。** この二台profileでは、現用権を持つnodeだけでBGP serviceを有効にし、両FRRの戻り経路がそのnodeを指すことを確認する。両nodeから固定LANを常時広告した旧試験設定では、待機側でSYN/ACKがINVALIDとして破棄され、port hash次第で通信が失敗した。 独立FRR/Zebra2台でLAN198.18.0.0/24とpeer203.0.113.0/24を相互広告し、受信RIB・両kernel・product best pathでLAN2端末のNATなしTCP/UDPを確認する。各bgpdを一台ずつ停止/再加入し、残るpeerの通信と復旧を確認する。特定peerの初期選択や切戻しを要求しない。長時間GR期限・両peer同時断・既存socket無損失継続は別の受入。NTPは実WAN学習源と独立chronyd2 clientの応答を確認し、`-x`/`-Q`でhost時計を変更しない。fallbackとappliance時計補正は別の受入。
7. **管理・Tailscale。** 一時鍵で管理SSH `show status`、Web履歴、LANからWebへの拒否、初回management generation1/restarts0、再起動後の鍵/履歴を確認する。独立Headscale/TailscaleへIPv4 LAN/exitを広告し、acceptDNS/acceptRoutes=falseとupstream既定のsubnet SNATを維持する。OS標準CAと試験CAはownerのprivate mount内で結合し、auth key/control DBをprivate stageへ置く。LAN→peer両IP、peer→LAN IPv4、両IP exit HTTPS/UDP DNSとnode identity保持を確認する。LAN→tailnet IPv6は明示NATであり、delegated IPv6 subnetの双方向広告ではない。public control/hostname bootstrapは別の受入。
8. **運用と復旧。** INFO syslog/OTLP、logs/metrics/traces、group1 NFLOG、group2 native nDPI/flow history、7日/daily/vacuumを既存ownerへ接続する。独立BGP peer上にsyslog UDP5514と公式Collector gRPC4317をdaemonより先に待受させる。実公開通信の双方向計数・native由来・TLS SNI、実管理拒否のhistory、観測開始後の新しいHealth syslog/log/metric/spanを照合する。samplingは旧既定30秒で、短flow全件保存・lossless deliveryを主張しない。PD worker SIGKILL/通常再起動と全daemon再起動後に上記の通信・identity・履歴・更新を確認し、正常停止時には上流Releaseなしでprefixを撤去する。

9. **設定変更とconfirmed rollback。** 同じ単体profileの現在のintent/revisionをcontrol APIから取得し、coreの`router.home.arpa` AレコードとLAN cache無効化を一つの候補へまとめる。30秒の確認期限付きcommitを実APIへ送り、両IPのUDP/TCPで候補回答と公開通信を確認する。確認を送らず、期限切れによる元のintent全体・revision・回答・公開通信への復帰を確認する。もう一度候補を適用し、control workerをSIGKILLして再起動による復帰も確認する。イベントでtimeout/restartの理由を区別し、DNSだけの変更と期限切れrollbackではWAN DHCPv4/RA/PD workerを再起動しないことを確認する。control再起動では現在の明示的な再取得手順を検証する。保存DBや設定ファイルの手修復は使わない。署名runtimeの更新・boot・rollbackは別の受入であり、この設定transactionの結果へまとめない。

二段DNSのpacket確認: 上記ownerの`location.json`と実leaseを読み、同じprivate mount内のrouter namespaceで`tcpdump -l -n -vv -i lo -c 8 'udp port 53'`を最大5秒捕捉する。client namespaceから実leaseへbindした`dig -b <実lease> @198.18.0.1 observed-two-stage.split.test A +nocookie +time=3 +tries=1`を送る。同じ名前/IDのLAN回答と127.0.0.1:53のquery/replyを照合してcaptureを止める。新しい名前を用い、LAN cacheだけの回答でcore転送を確認したことにしない。raw captureは保存しない。

## HA基盤の再実行

`make check-ha-state DOCKER='sudo -n docker'`は独立Linux namespace/PID1の二台、VRRP/共有WAN・LAN identity、conntrackd FTFW、相互認証TLSのDHCP同期を確認する既存手順。追加PVE/QEMUやPBR/二WANflagsは使わない。現在は全二台profileの実通信を優先し、基盤だけの合格を全体へ転用しない。TCPは再接続しないsocket、UDPも同一socketで継続し、DHCP端末は実割当/更新を確認する。TCP SYN、NAT tuple、DHCP Request/ACK、同期交通を照合する。明示したliberal modeを適用し、packet欠落と復旧を実測する。短い返信期限を全環境の性能条件へ読み替えない。

## 実HGWへ接続した二台profileの統合

次の実行対象は`examples/homert02-native.yaml`と`examples/homert03-native.yaml`の全機能二台profile。各48 resourcesへ二段DNSを接続し、下記の初期通信・両方向の切替/再加入を実行済み。従来の14-resource例やHA基盤gateの合格を転用しない。既存のDS-Lite群120→別DS-Lite80→LAN静的gateway60をそのまま用い、PPPoEやens20を新設しない。

`make check-uplink-ha DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18`で通常buildと設定検証後に実行する。まず実HGWのRA/PD、共有WAN/LAN IPv6 identity、VRRPの準備完了後公開、実DHCP端末の両IP HTTPS・UDP/TCP DNS、現用daemon停止後の引継ぎを通す。FRR2台、DNS、chronyd、Headscale、syslog/Collectorを独立peerとして配置する。独立peerの配置だけでは各経路の受入としない。

二台Tailscaleは現用権に合わせて有効化し、nodeごとの保持鍵を維持する。独立Headscaleの明示policyで試験userのLAN/exit広告を事前許可する（[公式route設定](https://headscale.net/stable/ref/routes/)）。独立端末はLAN経路を手動で切り替えず、制御サーバーが選んだPrimaryRoutesと実TCP/UDPを照合する。両方向の停止/再加入後もLAN→peer両IP・peer→LAN IPv4を確認する。exitは最後に現用nodeを明示選択して両IP HTTPS/UDP DNSを確認し、同一exit node IDの自動切替とは扱わない。両nodeへのSSH鍵認証/保持host key、現用側のloopback/管理DNS・Web履歴とLANからの管理拒否も確認する。

隔離: private mountと専用namespace/bridgeを用い、ens18へ専用macvlanで接続する。共有WAN MACは`02:00:5e:00:01:43`、静的gateway用HGW接続は`02:72:6f:75:74:06`。自然更新ownerのMAC/protocol/stateへ触れない。hostの管理address/default route/時計は変更しない。各実行は最大600秒、peer/productのstdoutと状態はprivate stageのみとし、終了時にprocess/namespace/key/stageを撤去する。失敗の原因と必要な修正は簡潔に残す。

再実行手順: 同じ設定で現用停止→待機側への復旧→旧現用の再加入を両方向へ進める。独立Python標準ライブラリのclientから公開HTTPSへTCP接続を一度だけ張り、公開DNSへ同じUDP socketで送信し続ける。TCPの再接続を禁止し、両nodeの実conntrack reply tupleとclient captureのSYNを照合する。UDPは1秒の返信期限で欠落と最大応答間隔を記録する。これは無損失条件ではない。DUID/IAID/prefixと同期済みDHCP leaseの実Renew/ACK、再加入後の非preemptも確認する。 OUI端末1台と特定MAC端末2台も別namespaceで実割当し、配布された特別gateway/DNSだけで公開通信を行う。通常・特別端末のlease全てが待機側へ同期した後に停止し、両方向の引継ぎで実Renew/ACK・同一割当・通信を確認する。続いて実Release/再取得でsticky割当の保持と新しい現用機のfingerprintを確認する。dnsmasqの通常Renew通知には要求options等がなく、fingerprint同期もないため、Renewだけで新しい現用機に識別記録ができることを前提にしない。新しい公開通信のnDPI計数/SNI、管理拒否履歴、現用node識別付きのfresh syslog/OTLP logs/metrics/tracesは記録時刻で過去の結果と分ける。

DHCPの現用権はVRRPが決める。元現用が停止しても、同期済みの保存leaseから配布を再開して実Renew/ACKを返す。同期は旧routerdと同じ非同期のため、未受信の新規割当を失った場合の重複回避までは保証しない。`--peer-loss`の既存namespace手順も、元現用不在中の既知lease更新と、同期復帰後の新規割当を確認する契約へ更新する。

残作業: 今回通過した特別scope・nDPI/履歴・syslog/OTLPに続き、保持期限/archive障害・cold start/再起動後の安定性を同じprofileで受け入れる。複数回、BGP/VRRP effect ACK待ちのtimeoutでworker再起動を観測したため、全機能時の適用/停止順序も次の確認対象とする。

## 公開DoHの片側停止・復旧

通常build済みの実行物と、稼働中の上記uplink fixtureを使う。`location.json`のowner PID/client namespaceを読み、ownerの生存を確認してから実行する。

```sh
make check-doh-outage DOH_OWNER_PID=<owner_pid> DOH_PARENT_NAMESPACE=<client_namespace>
```

別の実supervisor/control/network/DNS nodeと独立dig端末を、既存client0 LANへmacvlanで接続する。専用addressは198.18.0.200/.201、既存の独立gatewayは.253。追加WAN/NIC、hostのroute/clock/trust変更は不要。既存ルーターの設定・遮断規則は変更しない。Makefileは既存buildを使い、実行中の版を勝手に差し替えない。

公開Cloudflare/GoogleのDoHを両方Healthyまで確認し、DNS cacheを無効にして実queryを要求する。新しいDNS node内だけで片側の実A応答に含まれるTCP443宛先を遮断し、独立端末のUDP/TCP A回答、遮断packet counter、対象のDownと他方のHealthyを照合する。解除後は両方のHealthyと端末回答を確認して、もう一方にも同じ操作を行う。固定の回答IPは要求しない。全体上限300秒で、新node/client・process・一時binary/state/logを終了時に撤去する。これは公開DoHのIPv4 transportの片側障害であり、全homert private suffix、両DoH同時断、IPv6 transport、DS-Lite/HA統合の代用にはしない。

2026-11-03、製品code `df49035`・通常build `df490351c3ff`で `make check-doh-outage` が終了0。実Cloudflare/GoogleへTLS通信し、片側ずつTCP443を遮断して各16packetのdropとDown、解除後の両Healthyを確認した。cache無効の独立dig端末は全段階でUDP/TCPのNOERROR/A回答を取得。Cloudflare停止直後の初回回答1.043秒、以後の確認回答0.025〜0.063秒。新DNS node/client/process/stageは撤去済み。元の38-resource自然PD試験への障害注入は行っていない。公開DoH IPv4 transportの独立受入であり、DS-Lite/HA全体の障害受入には転用しない。

## MTU/MSSの実通信手順

自然更新ownerを同じprofileで稼働させ、初期の上流遮断/復旧確認が終わって通常通信roundへ入ってから行う。表示されたprivate stageの`location.json`からowner PID/router/client namespaceを読み、`client-lease-events`の最後の実割当を送信元に使う。hostの経路/MTUやproduct設定は変更しない。

rootで`nsenter --mount=/proc/<owner_pid>/ns/mnt ip netns exec <router>`内のWANを`tcpdump -n -v -i wan0 -c 8 'ip6 protochain 44'`で最大数秒捕捉する。同じprivate mountのclientから`ping -4 -n -I <実割当> -M do -s 1472 -c 1 -W 2 1.0.0.1`を送る。IPv4合計1500byte/DFの往復と、AFTRからの同一IPv6 fragment ID/offsetを照合する。`protochain`はkernel filter非対応時のuserspace filterでも観測可能で、filter警告を通信失敗へ読み替えない。captureは数秒で止め、raw出力を保存しない。

同じrouterの`any`で`tcp and host 1.1.1.1 and port 443 and tcp[tcpflags] & tcp-syn != 0`を上限8packet/数秒捕捉し、clientから実割当にbindした公開HTTPSを実行する。curlの`%{local_ip}:%{local_port}`とpacketのsequenceで同じ接続を照合する。他の周期通信を混ぜず、LAN→tunnelとtunnel→LANのMSSを確認する。実行例は通常の`--resolve one.one.one.one:443:1.1.1.1`を使う。

2026-11-03 build `df490351c3ff`: 実DHCP端末のDF1500往復、AFTR→B4のIPv6断片2個と再構成、同一TCPのSYN MSS1460→1414とSYN/ACK1420→1414、HTTPSが通過。B4送信断片化/IPv6 PTB/外側MTU縮小と性能は未確認。[RFC6333 section5.3](https://www.rfc-editor.org/rfc/rfc6333.html#section-5.3)は外側IPv6での断片化/再構成を規定するため、内側DF1500が成功したことを不具合や試験失敗と決めつけない。

## 自然更新と残る全体受入

終了0（2026-11-03 07:42〜09:46 UTC）: source/build `df49035`/`df490351c3ff`、単体38-resource profileで実HGWとの自然Renew/Reply1組と期限更新、623巡回の両IP HTTPS・UDP/TCP DNSを確認した。自然更新後の619巡回時点まで全28 worker generation1/Ready=trueを観測。PD SIGKILL2.90秒/通常再起動5.92秒、全daemon後の全項目再確認105.73秒も通過し、identityと両端末/Tailscale通信、停止時のRA撤去を確認した。105.73秒は抑制/samplingを含む全項目時間で、転送断時間ではない。owner/process/namespace/stageは撤去済み。二段DNS・候補設定修正の別受入、既存socket継続、全homert/実機資格とは範囲を区別する。

通常build `842c47e32300`で新しい`make runtime-bundle`が終了0。25役割・5 engine・15 artifacts、527,315,456 byteの署名配布物へsourceと303 license資料を同梱した。実receiverでstage/verify/initialize/statusと後続候補のprepare/cancelが通過し、元のactiveと増加済みwatermarkを維持。独立OpenSSLも署名を検証した。入力はprivate snapshotへ固定し、nDPIのsourceコピーをproduction/build入力へ限定してroot private probeとsite boot treeの非収録を確認した。再実行は[配布手順](runtime-bundle.md)。これは配布物と保存選択の受入であり、このbundleの実機起動・全profile更新は残る。

終了0（2026-11-03 05:39 UTC、開始03:25:30 UTC）: 設定/runner `5861b7e`、通常build ID `b03debf15982`（製品Go不変）で`--seconds 7800 --full-restart --tailnet`の自然更新観測を起動した。PD bound/IAID1も実プロセス・IPCで確認した。05:24:16 UTCの自然Renew/Replyと期限更新、678通信round、PD workerと全daemon再起動後の復旧を確認し、手順は終了0。stage/process/namespaceは撤去済み。BGPはgeneration17まで再起動したため長時間安定性は未受入。

自然PD T1は同じprofileで`UPLINK_SECONDS=7800`など実際の`renew_at`へ達する時間を選び、packetのRenew/Reply、DUID/IAID/prefixと更新期限、端末の通信・lease更新を確認する。短時間gateで代用しない。既存TCP socket/UDP mappingの継続は新規接続と別に観測する。自然更新待ちには独立する実装を進める。

別checkpointの自然更新、WAN遮断、Health→PBR、Alpine/PID1・更新rollbackは引き継ぎを参照する。現在の全profileや実機資格には転用せず、今回は追加実行しない。

1. **homert単体の全意図を閉じる。** 保存中の両110-resource設定を機能/caller/kernel effectへ照合する。残るprivate DNS peer、BGP契約、LAN/運用/保持dataset、全設定のcold start・自然更新・MTU/MSS/DF/PTB・PD変更/期限・通信継続・worker/control/daemon復旧を受け入れる。converterや旧prototype API維持を目的にしない。
2. **二台HA。** VRRP readiness/VIP/GARP/NA、DHCP/PD/DUID/IAID・lease同期、conntrack/NAT、既存TCP/UDP mapping、新規端末、DNS/BGP/NTP/Tailscaleを両方向停止/復帰で確認する。単体namespaceの成功を実homert二台へ読み替えない。
3. **全34 work package。** SYS（OS/capability/time/privilege/watchdog）、NET（PPPoE/link/VRF/VXLAN/DHCPv6 stateful/relay/DNS/BGP/BFD/WireGuard）、CORE/OPS（transaction/lifecycle/event/dynamic config/plugin/API/CLI/Web/logging/retention/DPI）、HA/EXT（同期/IPsec/Tailscale）、CLOUD（provider/federation/SAM）、DIST（signed runtime/boot/更新/A-B/実機/arm64/cloud image/Kubernetes）を各packageの受入条件で実peerへ接続する。一つの不具合で独立作業を止めない。
4. **QUAL。** 全field/default/lifecycle/status/eventと明示的置換、各配布先の通信・障害復旧・性能・更新rollbackを照合する。対象ONU/NIC/providerが使えなければ最小の不足setupと未受入範囲を残す。本番変更/課金は実際のauthorization内で行い、全条件が実証されるまで全体完成を宣言しない。

## 通常署名bundleのPID 1起動とPPPoE/PD

`make check-pppoe-pd DOCKER='sudo -n docker'`は通常の3実行物・全5 engineとsource/licenseを
`build-runtime-bundle.py`で署名し、実receiverでstage/initializeしたcatalogからPID 1を起動する。
独立RP-PPPoEのPAP/CHAP、ISC DHCPv6とLinux LAN端末でDNS/TCP/UDP、PD更新・失効・再取得、
worker/control再起動、設定rollback、正常poweroffを確認する。専用network/mount/PID namespace内で実行し、
hostの時計・管理経路は変えない。これはNET-04と配布boot経路の受入であり、homertの上流候補へPPPoEを追加しない。
PPP routeはleaseではなく接続所有であり、通知停止だけで存続中の経路撤去を要求しない。
接続消滅時の撤去とPD自身の実期限は別々に確認する。ログ/captureは終了後に簡潔な結果へまとめて撤去する。

実行結果: 2026-11-03、基準main `b3a0053` + 本変更の通常build `b3a0053f24c6`で、全25役割・5 engineの署名bundleを実receiverでstage/initializeし、Linux namespaceの実PID 1から起動した。独立RP-PPPoE/ISC DHCPv6/Linux LAN端末とのPAP・CHAPが両方終了0。DNS UDP/TCP、両IP TCP/UDP、自然Renew、server停止によるPD期限切れ/再取得、worker交換/DUID保持/古い通知拒否、confirmed rollback、PD/RA削除復元、PPP MTU変更、手動/control再起動、正常poweroffが通過。captureでMTU変更・手動/control再起動のRA zero-lifetime通知がPADTより前であり、上流Releaseなしと確認した。 これは実kernel/PID 1と独立peerでの配布boot・PPP/PD復旧の受入。ISPのPPPoE、ONU/NIC実機、全homert profileの署名更新/rollback、全34 packageの完成は残る。homert上流候補へPPPoEや追加NICは導入していない。

## 通常runtimeの署名更新・復帰

異なるrelease IDを持つ通常buildの3実行物とoffline publisherをowner専用directoryへ保持し、
`make check-runtime-update DOCKER='sudo -n docker' UPDATE_BASELINE_DIR=<保持したbuild directory>`を実行する。
現在の通常buildを候補として同じfactory catalog/engine/source/licenseと署名鍵でsequence1/2を作成する。
独立RP-PPPoE/ISC/Linux LANの初期通信後、実control APIで候補をupload/stage/prepareし、実PID 1へreboot要求を送る。
PID namespaceの終了後は同じkernel上でPID 1を再起動し、選択された別entrypoint/全workerのrelease ID、
実health watcherの自動confirm、元の設定/DUIDとstartup採用によるrevisionの進行、DNSと両IP TCP/UDPを確認する。
更新後に実設定へDNSレコードを追加して回答を確認し、実rollback APIと同じ再起動経路で以前のruntimeへ戻す。追加した設定・回答・同じ通信とhigh watermark保持を確認する。
通常終了のpoweroffとPD Releaseなしもcaptureで確認する。update用のowner上限は600秒で、protocolの期限は変更しない。
これは実kernel/process/store/通信の更新受入であり、kernel再起動・bootloader・停電と全homert profileの資格ではない。
全構成のappliance NTPはCLOCK_REALTIMEを変更するため、共有時計のnamespaceでは実行せず、独立kernel/時計を持つ許可済み実行機が必要。
PVE/QEMUは実行しない。終了後は以前のbuild copy、両bundle/store、鍵、capture/logを撤去する。

実行結果: 2026-11-03、通常署名bundleを実control APIでupload/stage/prepareし、実PID 1のrebootと再起動で `b3a0053f24c6 → 30054e56fdf6 → b3a0053f24c6` の更新・明示的rollbackが終了0。独立RP-PPPoE/ISC DHCPv6/Linux LANを使用し、全workerのrelease ID、実health watcherの自動confirm、現在の設定/DUID保持、DNS UDP/TCPと両IP TCP/UDP、high watermark=2の保持を確認した。候補でDNSレコードを追加してから旧runtimeへ戻し、その追加設定の両transportでの実回答も維持した。startup採用時は設計どおりrevisionが進む。正常poweroffとcaptureの上流PD Releaseなしも通過。 製品Goは両buildで同一で、異なる通常release IDの実行物を選択・execする経路の確認。PID namespaceを同じkernel上で再起動した試験であり、kernel/bootloader再起動、停電、失敗候補の自動fallback、異なるABI/実装間の更新、全homert profile/ONU/NIC資格は残る。全構成のappliance NTPには独立kernel/時計の実行機が必要で、共有host時計は変更していない。PVE/QEMUは実行していない。

失敗候補の受入: 上記更新・明示的復帰に続き、previousにある確認済み版へ戻す実rollback APIで次の候補を準備する。
同じsequenceのprepare再送はhigh watermarkで拒否されるため、既存版の選択には専用rollback操作を使う。
旧runtime終了後、隔離router namespaceだけで独立UDP processがDNS port53を占有する。
候補のDNS起動失敗から製品が失敗を保存してrebootを要求するまで待つ（製品の既定120秒判定は変更しない）。
保存状態は読取専用CLIで確認し、失敗対象digest・以前のactive・pending/attemptの解消を照合する。
独立processを停止して外部競合を解消し、同じ保存状態からPID 1を再起動する。
以前のruntime、paired startupのrecovered記録、採用revision、更新後に追加したDNS設定/DUIDと
両IP TCP/UDP/DNSの実回答を確認する。保存選択・設定・署名物の手修復は行わない。
これはOSの待受競合を原因とする候補失敗と復旧の確認であり、競合が続いたままDNSを提供できるという主張ではない。

実行結果: 2026-11-03、通常build `30054e56fdf6` / `799377d12628`の全factory署名bundleと独立RP-PPPoE/ISC/Linux LANで、更新→明示的復帰→失敗候補からの自動選択復旧が終了0。確認済み版へ再度rollbackする候補の起動時に、router namespace内の独立UDP processでDNS port53を占有した。実DNS workerのEADDRINUSEを観測し、11:03:26 UTC開始の既定120秒trialが11:05:26 UTCに失敗して製品自身がrebootを要求した。読取専用CLIで失敗digest、以前のactive、pending/attempt解消を確認。競合process終了後、保存選択/設定を手修復せずPID 1を再起動し、旧runtimeとpaired startupのrecovered記録、revisionの採用、更新後に追加したDNS回答、DUID、DNS UDP/TCPと両IP TCP/UDPが復旧した。high watermark=2、正常poweroff、captureの上流PD Releaseなしも通過。 これはOS待受競合による候補失敗と、外部競合解消後の復旧の確認。両buildの製品Goは同一で、同じkernelのPID namespaceを再起動した。実装/ABI変更を跨ぐ更新、kernel/bootloader再起動・停電、競合が続いた状態でのDNS提供、全homert profile/ONU/NIC資格は残る。PVE/QEMUと追加PBR調査、共有host時計の変更は行っていない。

## 単体全構成の履歴archive・期限切れ

保存homertのLogRetentionに合わせ、単体/二台native例のevent/DNS/flow/firewall/DHCP fingerprint保持へ既存syslog peerを接続する。
`make check-uplink-retention`へ通常の`UPLINK_INTERFACE`/`UPLINK_CONFIG`/`UPLINK_SECONDS`等を渡す。
同じownerが通常通信・設定rollback・PD復旧の後に保持試験を直列実行する（設定変更中に別の通常巡回を競合させない）。
実際に生成済みのDNS query・終了したflow・NFLOG拒否・DHCP識別記録をread-only SQLで観測する。
一時intentの期限2秒/間隔1秒とarchive専用の到達不能先で、期限後も元の記録が保持され、DNS UDP/TCP・両IP HTTPSが続くことを確認する。
正常な受信先へ戻し、元のrecord identity/timestampのsyslog受信と期限削除を確認して元の7日/daily/vacuum intentへ戻す。
live DBや時計へ時刻/行を注入しない。専用到達不能routeはfinallyで撤去し、WAN/通常syslog/OTLP/BGP peerの経路は変更しない。
これは短い明示policyの自然期限とarchive送信error/復旧の確認であり、7日経過・lossless UDP archive・全signalの保持資格ではない。
fixtureの終了後にDB/log/captureを撤去する。

実行結果: 2026-11-03、基準main `6e0f020` + native設定/fixture変更、実行build `799377d12628`（製品Go不変）で、archiveを接続した単体39-resource実HGW/Tailscale手順が終了0。保存homertのLogRetention sinksを再確認し、event/DNS/flow/firewall/DHCP fingerprintの保持へ既存syslog peerを接続した。通常通信・confirmed rollback・PD復旧の後、同じowner内で一時intentの期限2秒/間隔1秒を直列実行。archive専用の到達不能先で4 datasetの元記録を保持したまま、独立端末のDNS UDP/TCPとIPv4/IPv6 HTTPSが成功した。正常な受信先へ戻し、DNS query10件、終了済みflow10件、NFLOG拒否2件、DHCP fingerprint5件について元の識別子/時刻のsyslog受信と期限削除を照合した。元の7日/daily/vacuum intentへ戻した後も通信が成功した。 これは明示した短いpolicyの自然期限・送信error/復旧と27件の受信確認であり、7日経過、lossless UDP配送、全signal/物理機器の資格ではない。変更した二台設定例はschema検証済みで、このarchive条件のHA実通信は未実行。

## 実設定のprivate DNS peerとNextDNS

保存homertのDNSForwarder/DNSUpstreamを読取専用で照合し、対象suffixのSOAを各既存peerへUDP/TCPで照会する。
応答なし、NOERROR/SOA、NXDOMAINを区別し、privateなcase fileへsource/name/rcode/soaを保存する。
native単体profileのcoreへ同じ宣言順の転送先を設定し、実NextDNS/CloudflareのURL・bootstrapとWAN学習参照を接続する。
試験用local zoneが実suffixを隠さない名前を使い、DHCP/search/zoneの設定と確認側を一緒に更新する。
秘密を含むsite intent/case/URLはowner専用一時directoryへ置き、公開リポジトリへ保存しない。
DoH確認は固定provider名ではなく、実configに設定されたHTTPS上流すべてのHealthyを要求する。
通常巡回のlive ownerへ、`make check-site-dns SITE_DNS_OWNER=<pid> SITE_DNS_STAGE=<stage> SITE_DNS_CLIENT=<client namespace> SITE_DNS_CASES=<private cases.json>`を実行する。
設定は変更せず、実listenerを読み、両IP UDP/TCPのSOA/negative応答とcore履歴の実sourceを照合する。
同時にWANのUDP53を短時間captureし、質問・peer応答を確認する。未知の回答IPやSOA serialを固定値にしない。
事前の無応答を以後の固定SERVFAIL期待にしない。応答が変動するpeerは同時点のpacketと実応答を照合し、安定した相互接続の成功と区別する。新規WAN/NICやhost経路は追加しない。

実行結果: 2026-11-03、基準main `c371753`、通常build `6e0f020bf76c`（製品Go不変）で、保存homertの実private DNS/NextDNS設定を接続した単体39-resource実HGW手順が終了0。実NextDNS/Cloudflareの設定済みDoH上流すべてのHealthyを確認。安定した5転送先について独立LAN端末から両IP・UDP/TCPの20照会を行い、3先のSOA、2先のNXDOMAINとcore履歴の選択sourceを照合した。残る1先は事前UDP/TCP無応答から、試験中UDPのSERVFAILと後続NOERRORへ変化し、WAN captureで指定peer自身のNOERROR/NODATA応答を確認した。この先の固定SERVFAIL期待は試験側の誤りで、安定した4方式の受入には数えない。試験用local zoneは実suffixを隠さない名前へ変更し、DHCP/search/照会側も合わせた。新規WAN・host経路は追加していない。 同じ実行で通常の両IP Internet/DNS、DHCP scopes/sticky、BGP、NTP（host時計変更なし）、Tailscale/管理/運用記録、confirmed rollbackが通過。11巡回/自然PD更新0回、PD SIGKILL2.01秒・通常再起動4.87秒、停止時RA撤去/上流Releaseなし。全daemon再起動、HAでの実private DNS、全110-resource intent・実機資格は未受入。NextDNSは実endpointのprobe疎通で、全LAN照会がNextDNSを選んだという主張ではない。

## LocalServiceRedirectと部分DNS失敗

`make check-local-redirect DOCKER='sudo -n docker'`。独立dnsmasqのhostsと未設定の別名を同じAddressSetへ指定し、正常回答とREFUSEDを受ける。入力拒否policyにはICMPv6/サービスportを明示し、通常WAN転送も許可する。両IP UDP/TCPで、対象のローカル回答・WAN漏出なし、選外のpeer回答、peerのSIGHUP/実DNS更新によるmembership変更、confirmed commit/confirm、daemon再起動、撤去後のpeer回答を確認する。network snapshotにはusable membershipとREFUSEDが併存することを確認する。

2026-11-03、基準main `fa246ed` + 部分DNS失敗の準備判定修正、通常Makefile build `fa246ede2a5e`で実daemon/独立dnsmasqのlocal redirect手順が終了0（外側5.12秒）。旧routerdのresolveIPAddressSetは成功した名前のアドレスを使い、独立した失敗をstatusへ残す。native networkも部分membershipを適用していたが、coordinatorのCheckReadyが一件の解決errorで準備/確認を拒否していた。修正前は正常回答とREFUSEDの混在でhealth準備が10秒後も失敗することを再現。修正後はusable membershipを準備済みとし、REFUSEDをnetwork statusへ残したまま、両IP UDP/TCPのローカル転送、選外宛先のWAN転送、実DNS更新による旧転送先の解除/新転送先の適用、confirmed commit/confirm、daemon再起動、転送設定撤去を確認した。WAN captureで対象DNSの漏出なしも確認した。 これは実Linux kernel/実product processと独立dnsmasqの通信であり、実HGW全profileへ保存homertの2つのIPAddressSetとDNS/NTP LocalServiceRedirectを接続した受入は次の残作業。NTP redirect・HA・物理NIC資格には数えない。元fixtureに欠けていたICMPv6入力許可とLAN→WAN転送を明示し、確認応答は別の空structへdecodeするよう修正した。製品側に暗黙許可・DAD回避・待機延長は追加していない。通常build・3実行物static link、make race vetが通過。

## 保存homertのDNS/NTP転送を単体構成へ接続

`examples/single-router-dslite-dhcp-dns.yaml`のfirewallは、保存homertの公開DNS/NTPの名前一覧をcore DNSで解決し、LANからのTCP/UDP53とUDP123をローカルへ転送する。
`make check-uplink DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18 UPLINK_WAN_MAC=02:72:6f:75:74:03 UPLINK_CONFIG=examples/single-router-dslite-dhcp-dns.yaml UPLINK_SECONDS=20 UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1`で実HGW・独立LAN端末・FRR/chronyd/Headscaleの通常通信と一緒に確認する。
両LAN端末が実membershipの公開DNSアドレスを直接指定しても、IPv4/IPv6・UDP/TCPで設定済みローカルzoneの回答を得ることを確認する。NTPは保存homertのIPv4 allow範囲に合わせ、独立chronydの照会成功とconntrackのローカル転送を照合する。送信元は端末の実経路から読み、DHCPのアドレスと決めつけない。WAN確認用UDP/DNSは転送集合外の宛先を使い、実membershipに含まれないことも確認する。
同じ転送確認をdaemon再起動後にも行う。短時間の巡回は自然PD/DHCPv4更新や物理NIC・ONU資格に数えず、HA用設定への統合は別に受け入れる。

2026-11-03の実行結果: 基準main `efe9845` + native設定/fixture変更、通常build `efe984588553`（製品Go不変）で、保存homertの公開DNS/NTPの名前集合とLocalServiceRedirectを組み込んだ単体39-resource実HGW手順が終了0。両LAN端末から、実membershipの公開DNSアドレスを直接指定しても両IP UDP/TCPでローカルzoneの回答を得た。独立chronydの公開NTP宛て照会とconntrackのローカル転送も、保存設定と同じIPv4 allow範囲で確認。両方を全daemon再起動後にも再確認した。 全daemon後の全項目再確認111.77秒は転送断時間ではない。20秒の2巡回・自然PD更新0回。詳細と未受入範囲は[引き継ぎ](pve-codex-handoff.md)を参照する。
