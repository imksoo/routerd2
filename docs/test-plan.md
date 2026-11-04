# routerd2 実通信・製品受入計画

更新: 2026-11-04。全体範囲は[実装計画](implementation-plan.md)と[34 work package](implementation-plan.json)。実行済み版・結果・限界は[引き継ぎ](pve-codex-handoff.md)へ残す。独立端末を接続しただけで通常通信が成立し、障害・再起動・更新から復旧する製品を完成させる。

## WireGuard hostname endpointとroaming

`make check-wireguard DOCKER='sudo -n docker'`は実supervisor/control/networkとLinux WireGuard、独立wireguard-tools・dnsmasq・LAN端末を専用network/mount namespaceで動かす。試験内の`/etc/resolv.conf`だけを独立DNSへ向け、hostのresolver・経路・時計を保護する。hostnameからの接続とLANの実TCP/UDP、認証済み送信元portのroamingとDNS変更の優先関係、peer停止中の自然な135秒経過、NXDOMAINによる現endpointの保持、DNS復旧と移動先への接続、daemon再起動・peer撤去を確認する。待ち時間はwireguard-tools公式reresolve-dnsの条件を使い、時計・handshake時刻を注入しない。全体上限300秒、captureは既存の有界手順を使い、終了時に鍵・process・namespace・raw出力を撤去する。IPv6 underlay、SAM由来peer集合、全NET-08と物理機器の受入を継続する。

AllowedIPsにはホスト部付きIPv4/IPv6 CIDRと展開表記を指定し、実kernelの正規化されたprefixを初回・移動先復旧・daemon再起動後に照合する。初回とpeer復帰では実handshakeを確認してからLAN TCP/UDPを測定する。peerのlink停止でLinuxが撤去したLANへの戻り経路は、独立peerの起動設定として再適用する。接続準備中の短いTCP期限やpeer設定の欠落を製品の通信結果へ読み替えない。

## SAM登録から端末通信・撤去まで

通常build後、`python3 scripts/daemon-tests.py sam-transport --old-routerd /absolute/path/to/routerd`を使う。参照版`1329ef54f296`の独立routerd RR、FRR、wireguard-toolsとLAN端末を専用namespaceへ隔離する。IPIPとWireGuard経由それぞれで、実登録・claim所有/32の広告・kernel経路・NATなしTCP/UDP、明示revokeと元の登録期限経過による広告/tunnel/FIB撤去を確認する。既存の4手順を使用し、原本期限・時計・DBを書き換えない。peer停止後の期限を待ち、受信packetと両方向の端末通信を照合する。通常runnerは終了時にbinary copy・秘密・capture・namespaceを撤去する。これは単一RR/claimの受入であり、複数leaf間の移動・provider操作・direct mesh・key/PSK rotationは残る。

## 実行原則

- 毎回の実通信直前にこの計画全体を読み、対象・peer・実行版・観測点・隔離・復旧を確認する。変更対象の旧routerd実装、caller、運用修正と保存中homert02/03の実設定を参照する。秘密は公開しない。
- 実product processとkernel、独立したpeer/clientを使う。DHCP/PD/RA、DNS、NATを個別に動かしただけで受入にせず、設定から端末のInternetまで通す。補助テスト数やcoverageを完成の代わりにしない。
- 動的address/prefix/DNS/identityは実応答から読み、IPv6はparseして比較する。固定値比較は明示した予約・管理addressなどに限る。packet、端末、kernel、workerを必要な範囲で照合し、製品・設定・peer・runnerの失敗を分けて原因を直す。
- 一つの待機で独立作業を止めない。関連チェックが通った単位でcommit/pushし、CIの完了を待たない。現在はPVE/QEMUと追加PBR調査を行わない。
- 残すのは再実行手順と簡潔な結果・限界。終了後にraw log/pcap/key/state/namespace/重複build outputを撤去する。長時間試験のlive ownerと最小の未解決資料は終了まで保護する。

## 現在の結果と限界

main `e58d695`までの単体45-resource・二台各54-resource実HGW手順は、通常通信、再起動/両方向引継ぎ、同一公開/固定WAN TCP/UDP、逆引きとarchive障害/復旧を確認済み。実行版・測定値・未受入範囲は[引き継ぎ](pve-codex-handoff.md)と実装計画JSONへ集約する。以下は再実行手順であり、古いcheckpointの合格を現在版へ転用しない。全110-resource意図、全34 work package、対象実機/性能と長時間安定性は継続中。

## 再実行と隔離

```sh
make check-uplink DOCKER='sudo -n docker' \
  UPLINK_INTERFACE=ens18 UPLINK_WAN_MAC=02:72:6f:75:74:03 \
  UPLINK_CONFIG=examples/single-router-dslite-dhcp-dns.yaml \
  UPLINK_SECONDS=20 UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1
```

必要setup: 許可済み実HGW uplink、sudo/root、iproute2、BusyBox、dhcpcd、dnsmasq、curl/dig/tcpdump、OpenSSL/SSH、FRR8.4.4/Zebra/vtysh、公式otelcol-contrib0.151.0（`/usr/local/bin/otelcol-contrib`）。Makefileが通常buildとchecksum-pinned Headscale/Tailscale1.104.2 peerを準備する。Makefile build済み実行物でrunnerだけを再実行する場合は同じ引数を`python3 scripts/uplink-smoke.py`へ渡し、実行build IDを記録する。

専用macvlan MACとnamespaceを使い、hostの管理address/default route/時計/trust storeを変更しない。通常LAN198.18.0.0/16・機器用LAN198.19.0.0/16、管理10.255.255.0/24、WAN共有BGP192.0.2.0/24、tailnet underlay198.51.100.0/24へ隔離する。router側のlink/address/forward/NAT/serviceはnative設定が所有する。保存設定のens20（homert02）/eth2（homert03）は管理interfaceであり、追加WANではない。上流は設定された候補の優先順位と疎通状態で選び、NIC追加を条件にしない。低優先候補は既存LAN上のgatewayであり、別WANのDHCPではない。保存設定の実際の順序はDS-Lite 3本（weight120）→別DS-Lite（80）→LAN静的gateway（60）で、PPPoE候補はない。

単体と二台のfixtureは同じ実uplinkのL2上で試験用BGPアドレスを共有するため、実通信は直列に実行する。並行して行う作業は補助check・読取照合・独立する実装とする。

WAN/tailnet captureは各最大8MB、PD captureはudp546と実行時間で限定する。ownerは全peer/process/namespaceを撤去する。長時間用syslog受信は直近8MB、公式Collector file exporterは8MB×現行/backup1へ制限する（公式`validate`通過）。受信資料はraw archiveとして残さない。

## 同一profileで確認する経路

1. **WANと通常LAN。** 実HGWのDHCPv4、通常DADのRA/SLAAC、PD /60、DUID/IAID=1、委譲LANを確認する。LANの予約BusyBox端末と通常poolのdhcpcd端末がgateway/DNS、NTP2アドレス/domain-search、SLAAC/RDNSS/DNSSL/RA-triggered DHCPv6 Informationを取得し、両IPのInternetを使う。保存中homertのDHCPv6Serverはstatelessであり、不要なstateful poolを新設しない。
2. **特別scopeと保持。** 実homertの3 selectorsは同じprofileを参照するため、一つのpool ownerへOUIと6 MACをまとめる。通常pool64..191/予約はgateway/DNS .1、特別pool32..63は .253。OUI端末1台と特定MAC端末2台を独立namespaceへ接続し、配布値の設定だけで特別gatewayの実DNS/HTTPSを使う。特別profileに通常poolのNTP/optionsを勝手に継承しない。3日sticky/7日daily fingerprint保持を設定し、実Releaseと端末IP撤去、保存期限、別MACの同IP要求、元MACの再取得を確認する。DBはread-onlyで観測し、stateを捏造しない。全daemon restart後にも特別端末のRenew/ACK・通信・fingerprint APIを確認する。3日/7日経過削除は別の受入。
3. **LAN RA。** high preference、MTU1500、valid最大7200秒/preferred最大3600秒をPD所有期限で制約する。DNS広告寿命1800秒を設定し、0秒以外のRDNSS/DNSSLとそのIPv6 DNSへの実queryを確認する。LAN送信元link-localの実packet、端末の通常DAD/SLAAC/DNS、正常停止時のzero-lifetime RAとkernel撤去を確認する。
4. **DNS。** homertと同じLAN→loopback coreの二段構成を使い、LAN/core別のcacheとcoreの履歴を確認する。宣言順のsource評価を旧routerdから維持し、子suffixを親より先に置く。独立dnsmasq5353/5354で異なるAを返し、通常回答、片側停止の切替、両側停止中のcacheと未取得名SERVFAIL、復旧を両IPで確認する。複数suffix/共有upstream、WAN PD学習DNS、Cloudflare/Google両DoHの実probe、local/lease A/AAAA、UDP/TCP、loopback/管理listener、履歴API/Webも確認する。cache比較だけ`+nocookie`でquery keyを揃える。peerの起動は実queryへの応答まで確認する。全private suffix実peerは残作業。個々の公開DoH停止・復旧は下記の独立node手順で確認する。
5. **4 DS-Liteと上流選択。** 同じWANの別委譲/128（suffix ::21/::22/::13/::23）からAFTR ::100/::101/::102/::100へ接続し、各tunnelにbindした実TCP/HTTPS・UDP/DNSを確認する。内側IPv4は保存homertどおり各tunnelの192.0.0.2/29・.3/29・.4/29・.5/29とし、それぞれのアドレスへ明示SNAT（リンク停止で消えるmasqueradeを使わない）、MTU/MSS1454/DF処理、IPv6 host route table120とsource bypassを設定する。LAN新規通信は既存EgressPolicyのHealth/owner/markとweight120（3本）→80（4本目）→60（LAN static gateway）、60秒hysteresisで選ぶ。metric200..203はrouter起点の個別疎通用。試験専用nft tableで外側sourceを順に遮断し、Health・kernel規則・実conntrack mark/next hop・両通常端末の新規TCP/UDP、解除後の復帰を確認する。Linux static gatewayは同じHGWへ別MACでDHCPv4接続して転送/NAT/DNSを提供し、物理独立二回線の資格とは分ける。遮断tableはfinallyで撤去する。実設定にないPPPoE終端や別NICを新設しない。
6. **BGP・NTP。** この二台profileでは、現用権を持つnodeだけでBGP serviceを有効にし、両FRRの戻り経路がそのnodeを指すことを確認する。両nodeから固定LANを常時広告した旧試験設定では、待機側でSYN/ACKがINVALIDとして破棄され、port hash次第で通信が失敗した。 独立FRR/Zebra2台でLAN198.18.0.0/24とpeer203.0.113.0/24を相互広告し、受信RIB・両kernel・product best pathでLAN2端末のNATなしTCP/UDPを確認する。各bgpdを一台ずつ停止/再加入し、残るpeerの通信と復旧を確認する。特定peerの初期選択や切戻しを要求しない。長時間GR期限・両peer同時断・既存socket無損失継続は別の受入。NTPは保存設定の独立した二つのsource宣言を一つのclock engineへ接続する。auto clientはWAN学習源と公開2hostnameのfallback、static serverは固定peerと同じ公開2hostnameを常時使い、名前解決はcore DNSへ向ける。実WAN学習源・固定源の同時設定とpacket往復、独立chronyd2 clientの応答を確認し、`-x`/`-Q`でhost時計を変更しない。全時刻源の停止・復旧とappliance時計補正は別の受入。
7. **管理・Tailscale。** 一時鍵で管理SSH `show status`、Web履歴、LANからWebへの拒否、初回management generation1/restarts0、再起動後の鍵/履歴を確認する。独立Headscale/TailscaleへIPv4 LAN/exitを広告し、acceptDNS/acceptRoutes=falseとupstream既定のsubnet SNATを維持する。OS標準CAと試験CAはownerのprivate mount内で結合し、auth key/control DBをprivate stageへ置く。LAN→peer両IP、peer→LAN IPv4、両IP exit HTTPS/UDP DNSとnode identity保持を確認する。LAN→tailnet IPv6は明示NATであり、delegated IPv6 subnetの双方向広告ではない。単体・二台例は保存homertと同じ私設IPv4の3範囲と、隔離環境の両LANを含む198.18.0.0/15を広告する。独立peerでexitを解除し、設定された全subnetのPrimaryRoutesとkernel経路を確認してから、既存のBGP・静的経路・旧LAN・管理機器へTCP/UDPを通す。その後にexitを選び、両IP Internetを確認する。制御URLは`https://control.tailnet.test:8443`、bootstrap DNSは独立peerの`198.51.100.1`とする。独立dnsmasqへの製品underlayアドレスからのA照会を確認し、証明書のDNS SANを検証する通常engineで接続する。単体再起動と二台の各現用nodeでも名前解決・同じ端末通信を確認する。public controlと実サイトidentityは別の受入。
8. **運用と復旧。** INFO syslog/OTLP、logs/metrics/traces、group1 NFLOG、group2 native nDPI/flow history、7日/daily/vacuumを既存ownerへ接続する。独立BGP peer上にsyslog UDP5514と公式Collector gRPC4317をdaemonより先に待受させる。実公開通信の双方向計数・native由来・TLS SNI、実管理拒否のhistory、観測開始後の新しいHealth syslog/log/metric/spanを照合する。samplingは旧既定30秒で、短flow全件保存・lossless deliveryを主張しない。保存homertのuntrust→self既定dropとlogDeny/dropSampleRate=1に合わせ、inputと転送chainの終端dropをNFLOG group1へ接続する。既存FRR peerからルーターWANの明示static addressへICMP echoを送り、拒否と新しいinput-denyの履歴・OTLPを確認する。通常DAD/RA、DHCP、DS-Lite、BGP、Tailscale、LAN/管理通信も同じprofileで確認する。既存の独立FRR側から未要求のLAN宛TCP/UDPを送り、実socketの拒否と、同じ送信元/宛先/port・入出interface・新しい時刻のdrop履歴を確認する。単体の全daemon再起動後と二台の各引継ぎ後にも同じ実packetを確認する。PD worker SIGKILL/通常再起動と全daemon再起動後に上記の通信・identity・履歴・更新を確認し、正常停止時には上流Releaseなしでprefixを撤去する。

9. **設定変更とconfirmed rollback。** 同じ単体profileの現在のintent/revisionをcontrol APIから取得し、coreの`router.home.arpa` AレコードとLAN cache無効化を一つの候補へまとめる。30秒の確認期限付きcommitを実APIへ送り、両IPのUDP/TCPで候補回答と公開通信を確認する。確認を送らず、期限切れによる元のintent全体・revision・回答・公開通信への復帰を確認する。もう一度候補を適用し、control workerをSIGKILLして再起動による復帰も確認する。イベントでtimeout/restartの理由を区別し、DNSだけの変更と期限切れrollbackではWAN DHCPv4/RA/PD workerを再起動しないことを確認する。control再起動では現在の明示的な再取得手順を検証する。保存DBや設定ファイルの手修復は使わない。署名runtimeの更新・boot・rollbackは別の受入であり、この設定transactionの結果へまとめない。

二段DNSのpacket確認: 上記ownerの`location.json`と実leaseを読み、同じprivate mount内のrouter namespaceで`tcpdump -l -n -vv -i lo -c 8 'udp port 53'`を最大5秒捕捉する。client namespaceから実leaseへbindした`dig -b <実lease> @198.18.0.1 observed-two-stage.split.test A +nocookie +time=3 +tries=1`を送る。同じ名前/IDのLAN回答と127.0.0.1:53のquery/replyを照合してcaptureを止める。新しい名前を用い、LAN cacheだけの回答でcore転送を確認したことにしない。raw captureは保存しない。

## HA基盤の再実行

`make check-ha-state DOCKER='sudo -n docker'`は独立Linux namespace/PID1の二台、VRRP/共有WAN・LAN identity、conntrackd FTFW、相互認証TLSのDHCP同期を確認する既存手順。追加PVE/QEMUやPBR/二WANflagsは使わない。現在は全二台profileの実通信を優先し、基盤だけの合格を全体へ転用しない。TCPは再接続しないsocket、UDPも同一socketで継続し、DHCP端末は実割当/更新を確認する。TCP SYN、NAT tuple、DHCP Request/ACK、同期交通を照合する。明示したliberal modeを適用し、packet欠落と復旧を実測する。短い返信期限を全環境の性能条件へ読み替えない。

## 実HGWへ接続した二台profileの統合

次の実行対象は`examples/homert02-native.yaml`と`examples/homert03-native.yaml`の全機能二台profile。各54 resourcesへ二段DNSを接続し、下記の初期通信・両方向の切替/再加入を実行済み。従来の14-resource例やHA基盤gateの合格を転用しない。既存のDS-Lite群120→別DS-Lite80→LAN静的gateway60をそのまま用い、PPPoEやens20を新設しない。

二台の非公開DNS設定を使う場合は`UPLINK_HA_CONFIG_A=/private/node-a.json UPLINK_HA_CONFIG_B=/private/node-b.json`を指定する。直接runnerを実行する場合は`--config-a`・`--config-b`を使う。設定は既存の隔離interface/benchmarkアドレスを維持し、private suffix・upstream・NextDNSのURLだけを保存設定から接続する。試験は設定内のlocal zoneと全HTTPS上流を読み、DHCP検索domain・PTR・管理照会・新規端末の名前解決と各DoHのHealthyを確認する。

`make check-uplink-ha DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18`で通常buildと設定検証後に実行する。まず実HGWのRA/PD、共有WAN/LAN IPv6 identity、VRRPの準備完了後公開、実DHCP端末の両IP HTTPS・UDP/TCP DNS、現用daemon停止後の引継ぎを通す。FRR2台、DNS、chronyd、Headscale、syslog/Collectorを独立peerとして配置する。独立peerの配置だけでは各経路の受入としない。

二台Tailscaleは現用権に合わせて有効化し、nodeごとの保持鍵を維持する。独立Headscaleの明示policyで試験userのLAN/exit広告を事前許可する（[公式route設定](https://headscale.net/stable/ref/routes/)）。独立端末はLAN経路を手動で切り替えず、制御サーバーが選んだPrimaryRoutesと実TCP/UDPを照合する。両方向の停止/再加入後もLAN→peer両IP・peer→LAN IPv4を確認する。exitは最後に現用nodeを明示選択して両IP HTTPS/UDP DNSを確認し、同一exit node IDの自動切替とは扱わない。両nodeへのSSH鍵認証/保持host key、現用側のloopback/管理DNS・Web履歴とLANからの管理拒否も確認する。

隔離: private mountと専用namespace/bridgeを用い、ens18へ専用macvlanで接続する。共有WAN MACは`02:00:5e:00:01:43`、静的gateway用HGW接続は`02:72:6f:75:74:06`。自然更新ownerのMAC/protocol/stateへ触れない。hostの管理address/default route/時計は変更しない。各実行は最大600秒、peer/productのstdoutと状態はprivate stageのみとし、終了時にprocess/namespace/key/stageを撤去する。失敗の原因と必要な修正は簡潔に残す。

再実行手順: 同じ設定で現用停止→待機側への復旧→旧現用の再加入を両方向へ進める。独立Python標準ライブラリのclientから公開HTTPSへTCP接続を一度だけ張り、公開DNSへ同じUDP socketで送信し続ける。TCPの再接続を禁止し、両nodeの実conntrack reply tupleとclient captureのSYNを照合する。UDPは1秒の返信期限で欠落と最大応答間隔を記録する。これは無損失条件ではない。DUID/IAID/prefixと同期済みDHCP leaseの実Renew/ACK、再加入後の非preemptも確認する。 OUI端末1台と特定MAC端末2台も別namespaceで実割当し、配布された特別gateway/DNSだけで公開通信を行う。通常・特別端末のlease全てが待機側へ同期した後に停止し、両方向の引継ぎで実Renew/ACK・同一割当・通信を確認する。続いて実Release/再取得でsticky割当の保持と新しい現用機のfingerprintを確認する。dnsmasqの通常Renew通知には要求options等がなく、fingerprint同期もないため、Renewだけで新しい現用機に識別記録ができることを前提にしない。新しい公開通信のnDPI計数/SNI、管理拒否履歴、現用node識別付きのfresh syslog/OTLP logs/metrics/tracesは記録時刻で過去の結果と分ける。

DHCPの現用権はVRRPが決める。元現用が停止しても、同期済みの保存leaseから配布を再開して実Renew/ACKを返す。同期は旧routerdと同じ非同期のため、未受信の新規割当を失った場合の重複回避までは保証しない。`--peer-loss`の既存namespace手順も、元現用不在中の既知lease更新と、同期復帰後の新規割当を確認する契約へ更新する。

各方向の引継ぎ後、元現用daemonを停止したまま、保存leaseも手動address/routeも持たない新しい独立LAN端末を接続する。同じHA手順内でBusyBoxのDHCPv4・DHCPv6 Information、kernelの通常DAD/SLAAC/default route、配布されたDNS/searchを取得し、新規leaseの重複なし・自身のDHCP名・公開名の両IP UDP/TCP解決・両IP HTTPSを確認する。端末ごとに最大48packetのDHCP/ICMPv6 captureを取り、旧現用の再加入後には新規leaseも同期完了の対象に含める。DHCP名の非同期な公開は実queryで待ち、確認開始から回答までの時間を記録する。

残作業: 今回通過した特別scope・nDPI/履歴・syslog/OTLPに続き、保持期限/archive障害・cold start/再起動後の安定性を同じprofileで受け入れる。複数回、BGP/VRRP effect ACK待ちのtimeoutでworker再起動を観測したため、全機能時の適用/停止順序も次の確認対象とする。

## 公開DoHの片側停止・復旧

通常build済みの実行物と、稼働中の上記uplink fixtureを使う。`location.json`のowner PID/client namespaceを読み、ownerの生存を確認してから実行する。

```sh
make check-doh-outage DOH_OWNER_PID=<owner_pid> DOH_PARENT_NAMESPACE=<client_namespace>
```

別の実supervisor/control/network/DNS nodeと独立dig端末を、既存client0 LANへmacvlanで接続する。専用addressは198.18.0.200/.201、既存の独立gatewayは.253。追加WAN/NIC、hostのroute/clock/trust変更は不要。既存ルーターの設定・遮断規則は変更しない。Makefileは既存buildを使い、実行中の版を勝手に差し替えない。

公開Cloudflare/GoogleのDoHを両方Healthyまで確認し、DNS cacheを無効にして実queryを要求する。新しいDNS node内だけで片側の実A応答に含まれるTCP443宛先を遮断し、独立端末のUDP/TCP A回答、遮断packet counter、対象のDownと他方のHealthyを照合する。解除後は両方のHealthyと端末回答を確認して、もう一方にも同じ操作を行う。固定の回答IPは要求しない。全体上限300秒で、新node/client・process・一時binary/state/logを終了時に撤去する。これは公開DoHのIPv4 transportの片側障害であり、全homert private suffix、両DoH同時断、IPv6 transport、DS-Lite/HA統合の代用にはしない。

## MTU/MSSの実通信手順

自然更新ownerを同じprofileで稼働させ、初期の上流遮断/復旧確認が終わって通常通信roundへ入ってから行う。表示されたprivate stageの`location.json`からowner PID/router/client namespaceを読み、`client-lease-events`の最後の実割当を送信元に使う。hostの経路/MTUやproduct設定は変更しない。

rootで`nsenter --mount=/proc/<owner_pid>/ns/mnt ip netns exec <router>`内のWANを`tcpdump -n -v -i wan0 -c 8 'ip6 protochain 44'`で最大数秒捕捉する。同じprivate mountのclientから`ping -4 -n -I <実割当> -M do -s 1472 -c 1 -W 2 1.0.0.1`を送る。IPv4合計1500byte/DFの往復と、AFTRからの同一IPv6 fragment ID/offsetを照合する。`protochain`はkernel filter非対応時のuserspace filterでも観測可能で、filter警告を通信失敗へ読み替えない。captureは数秒で止め、raw出力を保存しない。

同じrouterの`any`で`tcp and host 1.1.1.1 and port 443 and tcp[tcpflags] & tcp-syn != 0`を上限8packet/数秒捕捉し、clientから実割当にbindした公開HTTPSを実行する。curlの`%{local_ip}:%{local_port}`とpacketのsequenceで同じ接続を照合する。他の周期通信を混ぜず、LAN→tunnelとtunnel→LANのMSSを確認する。実行例は通常の`--resolve one.one.one.one:443:1.1.1.1`を使う。

## 自然更新と残る全体受入

自然PD T1は同じprofileで`UPLINK_SECONDS=7800`など実際の`renew_at`へ達する時間を選び、packetのRenew/Reply、DUID/IAID/prefixと更新期限、端末の通信・lease更新を確認する。短時間gateで代用しない。既存TCP socket/UDP mappingの継続は新規接続と別に観測する。自然更新待ちには独立する実装を進める。

別checkpointの自然更新、WAN遮断、Health→PBR、Alpine/PID1・更新rollbackは引き継ぎを参照する。現在の全profileや実機資格には転用せず、今回は追加実行しない。

1. **homert単体の全意図を閉じる。** 保存中の両110-resource設定を機能/caller/kernel effectへ照合する。残るprivate DNS peer、BGP契約、LAN/運用/保持dataset、全設定のcold start・自然更新・MTU/MSS/DF/PTB・PD変更/期限・通信継続・worker/control/daemon復旧を受け入れる。converterや旧prototype API維持を目的にしない。
2. **二台HA。** VRRP readiness/VIP/GARP/NA、DHCP/PD/DUID/IAID・lease同期、conntrack/NAT、既存TCP/UDP mapping、新規端末、DNS/BGP/NTP/Tailscaleを両方向停止/復帰で確認する。単体namespaceの成功を実homert二台へ読み替えない。
3. **全34 work package。** SYS（OS/capability/time/worker障害/watchdog）、NET（PPPoE/link/VRF/VXLAN/DHCPv6 stateful/relay/DNS/BGP/BFD/WireGuard）、CORE/OPS（transaction/lifecycle/event/dynamic config/plugin/API/CLI/Web/logging/retention/DPI）、HA/EXT（同期/IPsec/Tailscale）、CLOUD（provider/federation/SAM）、DIST（signed runtime/boot/更新/A-B/実機/arm64/cloud image/Kubernetes）を各packageの受入条件で実peerへ接続する。一つの不具合で独立作業を止めない。
4. **QUAL。** 全field/default/lifecycle/status/eventと明示的置換、各配布先の通信・障害復旧・性能・更新rollbackを照合する。対象ONU/NIC/providerが使えなければ最小の不足setupと未受入範囲を残す。本番変更/課金は実際のauthorization内で行い、全条件が実証されるまで全体完成を宣言しない。

## 通常署名bundleのPID 1起動とPPPoE/PD

`make check-pppoe-pd DOCKER='sudo -n docker'`は通常の3実行物・全5 engineとsource/licenseを
`build-runtime-bundle.py`で署名し、実receiverでstage/initializeしたcatalogからPID 1を起動する。
独立RP-PPPoEのPAP/CHAP、ISC DHCPv6とLinux LAN端末でDNS/TCP/UDP、PD更新・失効・再取得、
worker/control再起動、設定rollback、正常poweroffを確認する。専用network/mount/PID namespace内で実行し、
hostの時計・管理経路は変えない。これはNET-04と配布boot経路の受入であり、homertの上流候補へPPPoEを追加しない。
PPP routeはleaseではなく接続所有であり、通知停止だけで存続中の経路撤去を要求しない。
接続消滅時の撤去とPD自身の実期限は別々に確認する。ログ/captureは終了後に簡潔な結果へまとめて撤去する。

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

失敗候補の受入: 上記更新・明示的復帰に続き、previousにある確認済み版へ戻す実rollback APIで次の候補を準備する。
同じsequenceのprepare再送はhigh watermarkで拒否されるため、既存版の選択には専用rollback操作を使う。
旧runtime終了後、隔離router namespaceだけで独立UDP processがDNS port53を占有する。
候補のDNS起動失敗から製品が失敗を保存してrebootを要求するまで待つ（製品の既定120秒判定は変更しない）。
期限監視は失敗を返し、PID 1がworkerを停止して設定writerのrelease store lockを解放してから失敗を保存する。
保存状態は読取専用CLIで確認し、失敗対象digest・元のreadiness期限理由・以前のactive・pending/attemptの解消を照合する。
独立processを停止して外部競合を解消し、同じ保存状態からPID 1を再起動する。
以前のruntime、paired startupのrecovered記録、採用revision、更新後に追加したDNS設定/DUIDと
両IP TCP/UDP/DNSの実回答を確認する。保存選択・設定・署名物の手修復は行わない。
これはOSの待受競合を原因とする候補失敗と復旧の確認であり、競合が続いたままDNSを提供できるという主張ではない。

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
二台では`SITE_DNS_STAGE`を現用nodeの`<HA stage>/a`または`/b`、clientを共通LAN namespaceへ向け、初期状態と両方向引継ぎ後に同じ照会を行う。ケース件数は質問数であり、一つのsourceが複数質問を持つ場合もsource数へ重複計上しない。
事前の無応答を以後の固定SERVFAIL期待にしない。応答が変動するpeerは同時点のpacketと実応答を照合し、安定した相互接続の成功と区別する。新規WAN/NICやhost経路は追加しない。

## LocalServiceRedirectと部分DNS失敗

`make check-local-redirect DOCKER='sudo -n docker'`。独立dnsmasqのhostsと未設定の別名を同じAddressSetへ指定し、正常回答とREFUSEDを受ける。入力拒否policyにはICMPv6/サービスportを明示し、通常WAN転送も許可する。両IP UDP/TCPで、対象のローカル回答・WAN漏出なし、選外のpeer回答、peerのSIGHUP/実DNS更新によるmembership変更、confirmed commit/confirm、daemon再起動、撤去後のpeer回答を確認する。network snapshotにはusable membershipとREFUSEDが併存することを確認する。

## 保存homertのDNS/NTP転送を単体構成へ接続

`examples/single-router-dslite-dhcp-dns.yaml`のfirewallは、保存homertの公開DNS/NTPの名前一覧をcore DNSで解決し、LANからのTCP/UDP53とUDP123をローカルへ転送する。
`make check-uplink DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18 UPLINK_WAN_MAC=02:72:6f:75:74:03 UPLINK_CONFIG=examples/single-router-dslite-dhcp-dns.yaml UPLINK_SECONDS=20 UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1`で実HGW・独立LAN端末・FRR/chronyd/Headscaleの通常通信と一緒に確認する。
両LAN端末が実membershipの公開DNSアドレスを直接指定しても、IPv4/IPv6・UDP/TCPで設定済みローカルzoneの回答を得ることを確認する。NTPは保存homertのIPv4 allow範囲に合わせ、独立chronydの照会成功とconntrackのローカル転送を照合する。送信元は端末の実経路から読み、DHCPのアドレスと決めつけない。WAN確認用UDP/DNSは転送集合外の宛先を使い、実membershipに含まれないことも確認する。
同じ転送確認をdaemon再起動後にも行う。短時間の巡回は自然PD/DHCPv4更新や物理NIC・ONU資格に数えず、HA用設定への統合は別に受け入れる。

## LANから管理側・WAN内機器への送信元NAT

現行の固定WANアドレス/SNATは末尾の「固定共有WAN IPv4とSNAT」を使う。以下は旧39-resourceのDHCP/masqueradeによる受入記録であり、現行の固定SNATの合格へ流用しない。
前項と同じ`make check-uplink`（`UPLINK_FULL_RESTART=1`）で、既存管理peerと既存WAN gateway namespaceの実DHCPアドレスに汎用TCP/UDP peerを起動する。管理peerのnamespaceとは別の自動LAN clientから通信し、peerがOS socketから得た送信元を、設定した管理アドレス/実WAN leaseと比較する。管理peer自身からの自己宛て通信で合格にしない。
通常の管理SSH/WebとLANからの管理console拒否、公開Internet/DNS/NTP、設定rollbackを維持し、全daemon再起動後にも両peerのTCP/UDP・観測sourceを再確認する。物理HGWの管理UIやHAのWAN VIP切替を検証したことにはしない。新規WAN/NIC・host routeは追加しない。

## 保存homertの個別IPv4経路とblackhole

単体例には、LAN側gateway経由の`192.168.100.1/32`（metric10）、WANへのon-link `192.168.0.0/16`（metric20）、LAN側gateway経由の`172.16.0.0/12`（metric20）、`10.0.0.0/8` blackhole（metric20）を宣言する。LAN gatewayのbenchmarkアドレスは`198.19.0.253`で、実サイト値を公開しない。
前項と同じ`make check-uplink`で、独立gateway namespaceに対象の受信アドレスとsocket peerを設定する。WAN側peerには実WAN lease経由で試験clientへの戻りhost routeを明示する。個別経路/私設網/WAN on-linkのTCP/UDP、kernelが選んだinterfaceとpeerの観測sourceを照合する。
blackholeは実clientからTCP/UDPを送り、timeout、kernel blackhole route、短時間・最大20 packetのcaptureでLAN ingressあり/egressなしを確認する。管理側・追加経路の確認は基本DHCP/DNSが使えた直後に行い、他の既存項目の待機を先行させない。全daemon再起動後にも同じ確認を行う。
このscopeは単体Linuxの経路適用と転送であり、実Starlink/IX2215の資格ではない。保存設定にある二つのLAN IPv4サブネットと異なるDHCP profileの構成差は別途解消し、同一サブネットのpool確認で代用しない。

## 二つのLAN subnetとDHCP profile

単体例は通常LAN `198.18.0.0/16`、機器用LAN `198.19.0.0/16`を同じ`lan0`へ設定し、管理は`10.255.255.0/24`へ分離する。機器用gateway/DNSは独立peer `198.19.0.253`。peerには通常LANへの戻り経路を明示し、WAN側照会中のhost routeだけを使い終わったら撤去する。
前項と同じ`make check-uplink ... UPLINK_SECONDS=20 UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1`で、通常/予約/機器用割当、実netmask、MAC/OUI、gateway/DNS、公開DNS/HTTPS、Release/3日sticky、DHCP名のDNS回答を実peerと照合する。fixtureはDHCP ACKのmaskをそのまま設定し、固定/24で補わない。割当範囲も実configから読む。
機器用DHCP名の追加照合は、live ownerのmount namespace内で`r2auto-<owner>`から`dig @198.18.0.1 special-0.home.arpa A`と`special-1.home.arpa`を照会し、各`<stage>/specialN-leases`の最新割当IPとparseして比較する。
保存RFC1918とbenchmark内部宛先にはSNATしない。私設/機器用peerが元の通常LAN sourceを見ることと、管理/WAN内peerが明示した変換後sourceを見ることを、実TCP/UDPで確認する。通常LANの/16へ揃えたBGP/Tailscale広告と管理SSH/Web、通常LANのIPv6/DNS/NTPも再確認する。
通常機能の実行は健全な現用上流を使い、上流の全段障害注入は明示した`UPLINK_WAN_OUTAGE=1`だけで行う。自然更新を再測定しない短時間確認には正の秒数を指定する。`--seconds 0`は自然PD更新まで待つ指定である。

## WAN共有のBGP

保存設定のBGP peerは既存WAN側にあり、専用BGP NICを条件にしない。単体例は試験用192.0.2/24をwan0の副subnetとして使う。独立FRR2台は既存uplinkの隔離peer interfaceで接続し、siteの稼働IPは使わない。
前項の`make check-uplink ... UPLINK_SECONDS=20 UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1`で、4つのimport prefixごとに実gatewayとwan0を確認し、両clientのTCP/UDPとpeer観測sourceを照合する。LAN /16の広告は両FRRのRIBとkernelで確認する。各peerを停止/再加入し、残るpeerの全prefixの通信と復帰、全daemon再起動後を確認する。WANからの新規LAN接続を広く許可するルールは追加しない。
既存のbounded captureを、live ownerのmount/network namespaceでtcpdumpのstdinへ読み、wan0 In/OutのBGP179と私設TCP18080/UDP18081を照合する。単に広いon-link routeで届いたことを合格にせず、具体的なBGP next hopも確認する。

## 二台構成への単体受入機能の接続

`make check-uplink-ha DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18`を使う。各54-resourceの二台例には、通常/機器用LANの二つの/16、保存NAT除外、管理/WAN内SNAT、個別静的経路、WAN共有BGP、公開DNS/NTP転送を接続する。専用BGP NICや追加WANは使わない。上流の既存順位120→80→60と60秒hysteresisは変更しない。
独立FRR2台の4私設prefixをwan0の具体的なnext hopで転送し、peer観測sourceとTCP/UDPを照合する。管理peerはLAN clientと別namespaceへ置き、製品を迂回した自己通信でNATを判定しない。公開DNS/NTPは実membershipの宛先でローカル回答を確認する。
現用daemon停止/再加入を両方向に行い、上記通信、両LANのDHCP mask/gateway/DNS・Renew/Release/再取得、共有PD identity、管理/Tailscale/運用記録を確認する。継続TCPは1.1.1.1:443、UDPは転送集合外の9.9.9.10:53を用い、同一socketとNAT tuple/mark、TCP SYN、応答間隔を照合する。UDP宛先が実membership外であることを確認し、ローカルDNS転送を上流UDP継続と数えない。build済み同一版で確認側を修正した場合は`python3 scripts/uplink-ha.py --uplink ens18`で再実行できる。

二台運転中の設定変更は同じHA手順の各引継ぎ前に現用機で行う。実control APIから現行intent/revisionを読み、coreのrouter AとLAN cacheを一つの30秒候補へ変更する。独立LAN端末から両IP UDP/TCPの候補回答と公開HTTPSを確認し、無確認の自然期限切れ、元intent全体と回答への復帰、rolled_back_timeout eventを照合する。再適用した候補を実confirm APIで確定し、通常commitで元のintentへ戻す。両caseで現用機とWAN DHCPv4/RA/PD・VRRP・BGPのPIDを維持する。既存TCP/UDPは設定変更中から両方向のdaemon停止/再加入まで継続させる。これは各nodeへの設定変更であり、node間の設定自動同期や更新中のcontrol交換を合格に含めない。

## 固定共有WAN IPv4とSNAT

単体/二台例の`address/wan-nat`はbenchmark `192.0.2.100/32`、`wan-local-nat`は通常LANから同じWAN subnet `192.0.2.0/24`への固定SNATを設定する。保存homertのWAN固定VIP/宛先subnetをこの隔離subnetへ対応づけ、サイトの稼働IPは使わない。単体はwan0、二台は既存wan-haの共通MACへ固定アドレスを置く。DHCPアドレスを使う旧WAN内masqueradeは削除する。
上記の単体`make check-uplink ... UPLINK_SECONDS=20 UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1`で、既存WAN上の独立peer `192.0.2.2`へのTCP/UDP、peer観測source、固定/32と再起動後を確認する。HGW subnetの既存peerはNAT選外となるため、peerのnamespace内にだけ対称な戻りhost routeを一時設定して元のLAN sourceを確認し、finallyで撤去する。hostの経路は変更しない。
同じbuildで`python3 scripts/uplink-ha.py --uplink ens18`を実行し、現用側だけの固定/32、peerが学習した共有MAC、各引継ぎ後のsourceを確認する。公開TCP/UDPに加えて、固定WANへのTCP接続/UDP socketを設定変更前から維持し、両方向の停止/再加入後に同一NAT tupleと通信の再開を照合する。captureはSYNの再送を許容し、各TCP接続の初期sequenceが一つであることを確認する。固定WAN peerはPython標準socketの独立processであり、物理HGWの管理機能や本番回線の既存sessionの資格とは分ける。


## ローカル逆引きと二台構成のarchive障害

保存homertのlocal-zoneが持つ正引きdomainとIPv4逆引きsuffixを、そのままnativeのzone sourceへ接続する。benchmarkの通常LANは`18.198.in-addr.arpa`。旧routerdと同様にsourceのmatchでzoneを選び、既存の静的レコードと生きているDHCP leaseからPTRを返す。新しい受信制限や逆引き専用の別経路は加えない。
単体は上記`make check-uplink-retention ... UPLINK_FULL_RESTART=1 UPLINK_TAILNET=1`、二台は`make check-uplink-ha DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18 UPLINK_RETENTION=1`を使う。通常buildが同一でrunnerだけを修正した場合は`python3 scripts/uplink-ha.py --uplink ens18 --retention-check`で再実行できる。
独立端末から静的名と実DHCP名をIPv4/IPv6のDNS listenerへUDP/TCPで逆引きする。HA端末は取得時にhostnameを通知し、実ACKのアドレスを使う。両方向の引継ぎ後にも同じ照会を行う。
archive試験は既存の単体手順を共有し、各引継ぎ後に新しい現用機が実通信から得た4 datasetを使う。障害中の記録保持と通常DNS/Internet、復旧後の元ID/時刻の受信・期限削除、元の7日/daily設定への復帰を確認する。fingerprintは新しい現用機で実Release/再取得した記録を使い、DBへ注入しない。期限削除後の管理履歴確認には新しい実queryを発生させ、期限切れ前の履歴が残る前提を置かない。既存の公開/固定WAN TCP/UDPは全操作中継続する。


## アドレス配布型DHCPv6

`make check-dhcp6-server DOCKER='sudo -n docker' DHCP6_SERVER=/usr/sbin/dhcpd`でInformation-requestとアドレス配布を確認する。後者は独立ISCのPD /60を製品WANが取得し、製品がLAN /64・RA・DHCPv4/v6・DNSを提供する。同じ実supervisor/control/network/worker、通常dnsmasq engineと独立dhcpcdを使い、WAN/LANを専用namespaceで隔離する。WANは試験用静的IPv6と独立peerへのdefault routeをnative設定が所有する。peerは実際に委譲したprefixを製品WANへ戻す経路を持つ。hostの管理経路/時計は変えない。

端末はRA・DHCPv4/v6から自動設定し、実割当、配布DNS、両IP UDP/TCP A/AAAA/PTR、上流peerとのIPv6 TCP/UDPを照合する。RAのSLAACとDHCPv6が併存するため、配布poolの実addressを選んで比較する。dhcpcd専用mount namespaceの`/run`と`/var/lib/dhcpcd`はtmpfsとし、hostのDUID・PID・resolverを変更しない。hookは実通知を観測する。名前変更・実更新、worker SIGKILL/daemon再起動後の割当とDUID、captureのserver identifierも確認する。

製品設定は変えず、独立ISCのprefixを変更する。peerのlease DBは保持し、自然更新によるLAN prefix・配布DNS・AAAA・通信の追従を確認する。旧prefixの保存履歴をDNSへ再掲載しない。次にdhcpcdだけをReleaseなしで止め、実保存lease期限まで待ってAAAA撤去と長いIPv4 Aの保持を両DNS UDP/TCPで確認する。再開後は実address/DAD完了を読み直してDNSとTCP/UDPを照合する。最後にISCを停止し、元のPD期限で製品LAN address・AAAAが撤去されることとIPv4 DNSの維持、ISC再開後のPD再取得・DNS・上流peer通信を確認する。clientがcacheしたIA_NAの即時消滅は要求しない。時計・DB・lease fileの注入/手修復は使わない。自然待機を含むowner上限400秒、外側runner420秒。

DNSの待受は明示IPv4とIPv6 `::`を併記する。割当観測は元のresource/PD権限とlease UTC期限を保持し、DNS待受確認は送信可否に使う。短い待受確認期限を割当所有権へ流用しない。同名A/AAAAは各addressの元期限で失効する。観測の失敗でworking DHCP engineを止めず、再観測できない情報は元の有限期限で失効させる。

対象だけの再実行は`python3 scripts/daemon-tests.py dhcp6-server --test TestDaemonDHCP6StatefulIndependentClient`。必要setupはISC DHCP 4.4.3-P1・dhcpcd・BusyBox・dig・通常build/engine・sudo/root。終了時にpeer/process/state/captureを撤去する。保存homertのstateless構成にはpoolを追加しない。例は[dhcp6-pd-stateful.yaml](../examples/dhcp6-pd-stateful.yaml)。

最新結果と実行版は[引き継ぎ](pve-codex-handoff.md)へ集約する。今回の上流は独立ISCのrouted IPv6 peerであり、実homert全構成のInternetやONU/NIC資格とは範囲を分ける。


## BGP受信数・受理数とresource参照

`python3 scripts/daemon-tests.py bgp-frr --test TestDaemonBGPTwoFRRPeers`を通常build後に実行する。独立FRR/Zebra二peerが許可経路と未許可prefixを広告し、実advertised-routes一覧の件数を製品の受信数と照合する。global import policy後の全local RIBにある受理経路数、peerごとのwithdraw/復旧、`BGPRouter.status.acceptedPrefixes`の参照値によるassert解除/復帰、daemon再起動後の同じ観測とTCP/UDPを確認する。非best/GR保持とローカル生成をFIB件数へ置き換えない。peerによる再広告を考慮し、受信数を設定したnetworkの数に固定しない。

この独立手順は専用namespaceの二つのpeer subnetを使う。実HGW単体例の同一WAN上BGPは別の上記全構成手順で確認する。新規host WAN、PVE/QEMUや追加PBR調査は不要。実行版・結果・未受入範囲は[引き継ぎ](pve-codex-handoff.md)を参照する。

## kernel対応ソースを含む通常配布image

`make image DOCKER='sudo -n docker'`で通常APKのbuild/checkとkernel/initramfsの組立てを行う。kernel入力stageは署名検証されたAlpine linux-ltsのinstalled metadataからrecipe commitを取得し、公式GitLabの該当directory全体とAlpine distfilesの外部sourceを取得する。APKBUILDに記載されたSHA512を照合し、実際にインストールされたkernel configと共に`kernel-sources.tar`へ格納する。組立てはそのpackage/version/architecture/commitをkernel rootと照合し、manifestのfile/digestとSHA256SUMSへ接続する。

出力directoryで`sha256sum -c SHA256SUMS`を実行し、archive内のAPKBUILDにある全sourceのSHA512とinstalled.config、manifestのkernel package/ソースdigestを照合する。元のkernel COPYING/LICENSESは同梱したupstream archiveで確認する。再現は通常image buildを使う。kernelを起動していないbuild確認の範囲と、独立kernel/実機でのboot・時計補正・全構成更新の受入を区別する。今回もPVE/QEMUは実行しない。終了後は組立て用の一時log、確認用archive/image、task用Docker tagを撤去する。

## 通常ディスクのGPT・BIOS・UEFI配置

`make image DOCKER='sudo -n docker' IMAGE_DIR=dist/image`に続き、private mount namespace内で`make disk-image`を使う。[image手順](../images/README.md#bios-and-uefi-x86-disk)のGRUB両platform・dosfstools等を準備し、一時的なoperator公開鍵と選択したnative SSH host keyを入力する。新規32 GiB sparse imageにはGPTのBIOS boot/FAT32 EFI/ext4 boot/ext4 stateを作り、`DISK_STATE_DEVICE=/dev/vda4`（SCSIは`/dev/sda4`）へ揃える。

生成物をread-only loopとして接続し、`sfdisk --verify`と4区画のname/type/size、EFIの`EFI/BOOT/BOOTX64.EFI`に対する`grub-file --is-x86_64-efi`、BIOS coreと`grub-script-check`を確認する。boot filesystemの実UUIDが共通GRUB設定へ使われ、kernel/initramfs/boot文書のdigestがmanifestと一致すること、boot文書のstate参照/認可公開鍵と第4区画の選択host keyのbytes/modeを照合する。unmount/loop解除後、結果を集約して一時key/image/logを撤去する。

これは実block/filesystemを使う生成内容の確認である。unsigned UEFIはSecure Boot無効が必要で、BIOS/UEFIの実起動、2回以上の起動でのstate保持、対象driver/firmware、停電、署名whole-OS更新・kernel/bootloader A/Bは対象機で別に受け入れる。PVE/QEMUは実行しない。現在のimageはvirtio中心のdriver closureであり、対象NIC/storageは実機情報に合わせて接続・検証する。
