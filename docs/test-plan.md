# routerd2 実通信・製品受入計画

更新: 2026-11-03 09:54 UTC。全体範囲は[実装計画](implementation-plan.md)と[34 work package](implementation-plan.json)。実行済み版・結果・限界は[引き継ぎ](pve-codex-handoff.md)へ残す。独立端末を接続しただけで通常通信が成立し、障害・再起動・更新から復旧する製品を完成させる。

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
