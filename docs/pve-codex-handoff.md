# routerd2 開発引き継ぎ

更新: 2026-11-03。全体ゴールは継続中。

## 単体全構成の実HGW再確認とBGP受理経路の観測

main `986246c`・通常build `986246cb28dc`の45-resource単体例を実HGWで起動し、独立LAN端末のDHCPv4/SLAAC/Information・両IP Internet/DNS、4 DS-Lite、BGP2 peer、NTP、Tailscale、管理、native nDPI/履歴・syslog/OTLP、設定rollbackと全daemon再起動が終了0。PD SIGKILL1.97秒/通常再起動6.09秒、全daemon後の全項目再確認111.63秒。111.63秒は転送断時間ではない。2巡回/自然PD更新0回。WAN遮断・追加PBR調査は行っていない。

旧routerdの`pkg/controller/bgp/controller.go`のstatePeer/観測を比較した。受信数は既存ListPeerのAdj-RIB-In値を利用し、受理数は既に取得しているglobal import policy後の全local-RIB経路からpeer別に数える。ListPeerのAccepted値はglobal policy前の段階なので、未許可経路を含む実広告で違いを確認した。FIB選択、追加RIB query、別counter cache、独立pollerは増やしていない。最初の試験側の固定受信数は、FRRの実広告一覧を使う比較へ変更した。

基準main `986246c` + BGP観測変更、通常build `986246cb28dc`で独立FRR二peerの手順が終了0。各peerの受信数をFRRの実advertised-routesと照合し、global import policy後の受理数は各2件。許可経路のwithdrawで対象peerの受理数が0となり、未許可の広告は残る。BGPRouterのacceptedPrefixes参照（合計4との比較）は撤去で解除、再取得とdaemon再起動後に再assert。既存のTCP/UDP・LAN広告・GR保持/再加入・設定撤去も通過した。

関連Race/vet・make fmt-check/test-tools・通常3実行物のstatic linkが通過。再実行は`python3 scripts/daemon-tests.py bgp-frr --test TestDaemonBGPTwoFRRPeers`、実HGWは[試験計画](test-plan.md)の単体手順（WAN_OUTAGE=0）。

実HGWの単体例と独立FRRの部分受入であり、全110-resource意図、長時間/同一TCP継続、GR全期限、全BGPPeer操作、HA/実機・ONU/NICと全34 packageの完成は継続中。全構成の実HGW確認はBGP観測変更前の986246cであり、観測変更後の受入は独立FRR手順。

## PDからアドレス配布・DNS・上流通信までの接続

実通信で二つの原因を修正した。IPv6 `::`がIPv4 socketも開いて明示IPv4 listenerと衝突していたため、既存のtyped addressからudp4/tcp4とudp6/tcp6を選ぶ。もう一つは、DNS待受の短い確認期限で割当観測まで失効し、実割当と元のPDが有効でもAAAAが欠ける循環だった。Inputの`Authority`は元のresource/PD権限を保持し、`DNSReadyUntil`は両server経路の配布可否に使う。lease観測の期限は前者へ制約し、DNS待受確認の短い窓を割当所有権へ流用しない。producer/consumerを同じ現在契約へ更新し、互換readerや期限延長・retryは加えていない。旧routerdのDNS listener、dnsmasq stateful/both constructorとcallerも照合した。

基準main `e711f54` + DNSのfamily指定/配布権限と待受確認期限の分離、通常Makefile build `e711f545970f`で、独立ISC DHCP 4.4.3-P1・dhcpcd 10.0.6・Linux kernel・実product processによる`dhcp6-server`が終了0。PD /60からのLAN /64、DHCPv4/SLAAC/DHCPv6の自動設定、配布DNSへの両IP UDP/TCP A・AAAA・PTR、名前変更、worker SIGKILL/全daemon再起動後の同一割当/DUIDと実TCP/UDPを確認した。製品設定を変えずISCのprefixを変更し、自然更新で新LAN prefix・配布DNS・AAAAと通信が追従。LAN leaseの自然失効ではAAAAを撤去してIPv4 Aを保持。ISC停止によるPD自然失効では製品LAN addressとAAAAを撤去し、IPv4 DNSを維持した。ISC再開後のPD再取得・DNS・独立上流peerとのIPv6 TCP/UDPも復旧した。

既存stateless PDのISC/radvd/BusyBox/Linux手順（期限切れ・confirmed rollback・委譲DNS）と`make check`（Race/vet/Python36件）、通常static3実行物、設定例検証も通過。再実行は`make check-dhcp6-server DOCKER='sudo -n docker' DHCP6_SERVER=/usr/sbin/dhcpd`。対象だけは`python3 scripts/daemon-tests.py dhcp6-server --test TestDaemonDHCP6StatefulIndependentClient`。新しい例は[dhcp6-pd-stateful.yaml](../examples/dhcp6-pd-stateful.yaml)。手順・隔離は[試験計画](test-plan.md#アドレス配布型dhcpv6)。

隔離Linuxのrouted IPv6 WAN・独立peerでの受入。保存homertはstatelessのまま。全homert構成のInternet、stateful lease同期/HA、client Reconfigure、ONU/NIC/性能と全34 packageの完成は継続中。

## DHCPv6再番号付け時の古いDNS登録を修正

実端末試験で、dnsmasqが旧addressにVL=0を返した後も元期限付きのlease履歴を保存し、DNSが旧・新AAAAを両方返す問題を再現した。engine lease fileは変更せず、観測時に固定poolのprefix、またはconstructorの現在のinterface prefixに属する割当だけを公開する。旧routerdのlease readerとdnsmasqのconstructor/lease実装を比較した。新しいallocator、lease DB、再試行層は追加していない。

基準main `aabd595` + 本修正、通常Makefile build `aabd5951463d`、独立dhcpcd・実supervisor/control/network/worker・bundled dnsmasqで`dhcp6-server`が終了0。設定APIでLAN /64・RA・配布DNS/listenerを変更し、端末の自然更新から新prefix取得、旧AAAA撤去、新割当の両DNS/UDP/TCP A・AAAA・PTRとTCP/UDP、同じDUIDを確認した。端末をReleaseなしで停止し、実leaseの保存期限（120秒）後にAAAAが消え、600秒leaseのIPv4 Aは残った。再開後は端末の実割当とDAD完了を読み直し、DNSとTCP/UDPが復旧した。

これは隔離Linuxでの設定によるLAN再番号付けとlease自然失効の受入。上流PDの変更/失効から同じstateful LANへ連鎖する全経路、stateful lease同期/HA、全構成Internet・実機/ONU/NIC/性能と全34 packageは継続中。 関連Race/vet、make fmt-check/test-tools、通常3実行物のstatic linkも通過。再実行は`make check-dhcp6-server DOCKER='sudo -n docker'`。先行runnerでは期限待ちのDNS連続照会がcapture上限を超え、再開直後の旧address前提も失敗した。実保存期限を待ち、再取得後の実kernel address/DADを観測する形へ直し、capture上限と意図した通信確認を維持した最終手順が合格した。

## DHCPv6 leaseのDNS登録と両familyの自動設定

旧routerdのdnsmasq lease読取とzone回答を基に、engineが所有する実IPv6 leaseを有限の観測としてDNSへ接続した。DHCPv4/v6は同じtyped receive viewを使い、別のlease DBは作らない。同名レコードを丸ごと置き換える処理を修正し、A/AAAAを集約して各addressの元lease期限/producer期限を保持する。lease読取・通知はengine停止を妨げない所有goroutineで行い、報告失敗で配布自体を止めない。

基準main `e1d2b91` + DHCPv6/DNS変更、通常Makefile build `e1d2b91a96b5`で、独立dhcpcd端末が製品RA・DHCPv4/v6から両familyのaddress/routeを自動設定した。実配布された両DNSへUDP/TCPで同名A/AAAA・IPv4/v6 PTRを照会し、実割当と一致。hostname変更後の更新と旧名撤去、実Renew/Reply、割当からのTCP/UDP、worker SIGKILL/全daemon再起動後の割当・DUID・DNS復旧も通過した。Information-requestと既存DHCPv4の7手順も終了0。

隔離Linuxの実product process/kernelと独立端末での受入。保存homertのstateless構成にはpoolを追加していない。stateful lease同期、prefix変更/自然失効、HA、全構成Internet/署名起動・更新、ONU/NIC/性能と全34 packageは継続中。 再実行は`make check-dhcp6-server DOCKER='sudo -n docker'`と`python3 scripts/daemon-tests.py dhcp-server`。`make check`、最終関連Race/vet・通常static3実行物・設定例検証も通過。dhcpcdの設定再読込は一時的にaddressを外してConfirmするため、再取得後に通常Renewを行う手順へ修正した。固定IPや手動経路で補っていない。

## DHCPv6アドレス配布を既存engineへ接続

旧routerdの`pkg/render/dnsmasq.go`、dnsmasq2.92rel2のconstructor/option/lease-DUID処理と現行callerを参照した。`dhcp6_server.pool`で既存bundled dnsmasqにアドレス配布を任せ、Goの新しいallocatorは作っていない。RAは既存serviceへ分離し、managed/autonomous指定でstateful/bothを構成する。情報配布型の既存Go経路はそのまま使う。DUIDを既存の原子的保存からengine leaseへ引き継ぎ、割当・更新・解放・lease保存はdnsmasqが所有する。学習options/配布権限は既存の元期限で更新・停止する。

基準main `e58d695` + 本変更、通常build `e58d695dbac6`で、独立BusyBoxクライアントのDHCPv6割当/実Renew、配布DNS、割当アドレスからのTCP/UDP、worker SIGKILL/全daemon再起動後の同一割当とDUID保持を確認した。captureの実Advertise/Replyのserver identifierも保存DUIDと一致。既存Information-requestの実通信、共通engine起動処理を使うDHCPv4の7手順も終了0。`make check`（Race Detector/vet/Python36件）・通常static3実行物・設定例検証が通過。

通常署名bundleを実publisherで生成し、receiverのstage/verifyが終了0。25 worker roles、6 engine bindings、15 artifactsで、DHCPv4/v6は同一dnsmasq engine/sourceを参照し二重収録しない。初回のbundle実行はローカルのPPPoE source archive不足で未生成となり、既存Makefileのengine準備後に最終確認を通した。

今回のアドレス配布は隔離Linux kernelと独立クライアントでの受入。保存homertはstatelessのままで、そこへpoolは新設していない。stateful leaseのDNS連携/同期、prefix変更と期限、HA、全構成Internet/署名起動・更新、ONU/NIC/性能、全34 packageは残る。 再実行は`make check-dhcp6-server DOCKER='sudo -n docker'`、共通engineのDHCPv4は`python3 scripts/daemon-tests.py dhcp-server`。利用例は[dhcp6-stateful.yaml](../examples/dhcp6-stateful.yaml)、設定/engine bindingは[service説明](../internal/protocol/dhcp6server/README.md)。

## ローカル逆引きと保持障害の接続

保存homertのlocal-zoneのmatchと旧routerdのsource選択→zone回答を読み直し、native単体/二台設定へ通常LANの逆引きsuffixを追加した。既存のPTR/実lease処理を使い、製品Goの別経路や受信制限は加えていない。上流候補も変更していない。保存設定の順序はDS-Lite群120→別DS-Lite80→既存LAN静的gateway60、PPPoE候補なし、ens20は管理用。

通常Makefile build `7649acd44e52`、単体45-resource実HGW手順が終了0。独立端末から静的名と実DHCP名のPTRを両IP UDP/TCPで確認し、全daemon再起動後の通常通信・identity保持も通過した。全項目再確認111.63秒は転送断時間ではない。その後のarchive障害中もDNS/両IP HTTPSと元の4 datasetを保持し、復旧後はDNS10件・終了flow10件・firewall4件・fingerprint5件の元ID/時刻を受信して期限削除、元の保持intentと通信へ復帰した。2巡回/自然PD更新0回。実行snapshotにはPTRの再起動後再照会がなく、その項目は未実行。

再実行手順と限界は[試験計画](test-plan.md)末尾。共有archive確認は送信先logを明示引数で受け取り、二台でも新しい現用機が実取得した記録を使う。期限削除後の管理履歴は新しい実queryで確認する。設定確定前には同じ候補revisionの実healthyを待ち、確認期限30秒と製品の拒否条件を維持する。先行二台実行は候補通信・期限切れ復元後、状態劣化によるconfirm422で終了し、同時刻にevent保存deadline超過を観測した。DNS回答だけを確定可能状態の証明にせず、同じ構成の最終再実行は終了0。

2026-11-03、通常build `7649acd44e52`（製品Go不変）、各54-resource実HGW二台手順が終了0。両方向で静的/実DHCP PTRの両IP UDP/TCP、設定期限切れ復元/confirm、通常通信・identity保持、archive障害/復旧と元の保持intentへの復帰を確認した。元ID/時刻の受信と期限削除はbでDNS/終了flow/firewall/fingerprint=10/10/4/3件、aで10/10/8/4件。公開TCP500応答/最大応答間隔15.77秒、UDP543応答/17期限超過/最大10.52秒、固定WAN TCP543応答/最大13.60秒、UDP549応答/11期限超過/最大9.51秒（UDP返信予算1秒）。TCPはいずれもSYN1回、同一socket/NAT tupleを維持。全項目128.03/82.84秒はNTP/保持確認を含み転送断時間ではない。 短時間のLinux namespace/kernel・実HGW・独立peerでの受入であり、全110-resource intent、全private DNS、長時間安定性、物理ONU/NIC・性能と全34 packageの完成は残る。

補助検査は`make test-tools race vet`、`make fmt-check`、通常Makefile build/3実行物static linkと単体/二台設定検証が通過した。製品Goは変更せず、逆引きは既存の通信実装を設定へ正しく接続した。

## 固定共有WAN IPv4と固定SNAT

保存homertのVirtualAddress/wan-nat-v4とその参照SNATを再読し、native例のDHCPアドレスによるWAN内masqueradeを削除した。既存WANのbenchmark subnetに固定`192.0.2.100/32`と宛先`192.0.2.0/24`へのSNATを設定し、独立peer `192.0.2.2`で確認する。単体はwan0、二台は既存の共有MACを持つwan-haで現用側だけが固定アドレスを所有する。アドレスとSNATは既存Address/NATの依存・条件・単一kernel writerへ接続し、新しいworkerや状態は追加していない。サイトの稼働アドレスは流用していない。

2026-11-03、基準main `d92b0b2` + 設定/fixture変更、通常Makefile build `d92b0b2a894e`（製品Go不変）で、単体45-resourceと各54-resourceの二台手順が両方終了0。単体の起動時・全daemon再起動後に、peer観測の固定sourceとTCP/UDP、管理NAT/私設網の非NAT・通常Internet/DNSを確認。全項目再確認111.76秒は転送断時間ではない。二台では各方向の停止/再加入とDNS編集/rollback/confirm後にも、現用側だけの固定/32、peerが学習した共有MAC、同じ固定sourceが成立した。packetでもLANの元source→wan0の固定source→wan-haへの応答→LANを照合した。

固定WANへの同一TCPは484応答・最大応答間隔13.91秒、同一UDP socketは491応答・11期限超過・最大間隔11.01秒（返信予算1秒）。NAT tupleを両方向で保持し、公開回線への既存TCP/UDP、PD identity、DHCP・管理・運用記録・Tailscaleも通過。両TCPのcaptureはそれぞれSYN1 packet/初期sequence1個だった。確認側はSYN再送を許容してsequenceを照合する。全項目113.76/68.79秒はNTP品質待ちと再取得等を含み、転送断時間ではない。echo TCPの行形式と受信側を統一し、到達可能peerに対して拒否/撤去確認が誤合格しないことも実通信で確認した。make test-tools race vet・fmt-check、通常buildと3実行物static linkが通過。

再実行は[試験計画](test-plan.md)末尾。これは実HGW接続下の隔離Linuxでの固定SNAT/共有アドレス受入であり、物理HGW管理機能、本番回線の既存session、ONU/NIC/性能、全private DNS、HA保持障害、全34 packageの完成は残る。次は保存DNSZoneの逆引き等、残るDNS/保持の実運用差へ進める。

## 無関係な設定変更でHAの現用権を失わない

基準main `b9d4d14`の実HGW二台試験で、DNSのAレコード/cacheだけを変更するとVRRP workerがHTTP409で停止し、待機機へ切り替わる問題を再現した。ApplyConfigが設定更新時に、変更していないworkerのカーネル反映待ちも一律にErrStaleで終了していた。反映待ちは実際のworker identityと原通知に結び付け、不要な設定epochを削除した。新しい設定でも同一のworker/通知を保持する要求だけを継続し、新しいprojectionの実適用後にACKする。worker交換、古い通知、元の期限とカーネル反映の検査は維持する。

2026-11-03、基準main `b9d4d14` + 本修正、通常Makefile build ID `b9d4d14bc85b`で、各52-resourceの最終HA手順が終了0。各現用機で両IP UDP/TCPの候補DNS回答と公開HTTPS、30秒候補の自然期限切れ・元intent全体/回答への復帰、再適用/明示confirm/元設定への通常commitが通過。期限切れは32.46/32.61秒、confirmから復元を含む操作は3.57/3.90秒。いずれも現用権とWAN DHCPv4/RA/PD・VRRP・BGPのPIDを維持した。確認側のfalse省略による比較誤りは、APIが返した適用済み/確定済みの設定同士を照合するよう修正した。

同じTCP/UDPを設定変更前から両方向のdaemon停止/再加入まで継続し、SYN1回・TCP333応答/最大間隔16.10秒、UDP372応答/16期限超過/最大間隔10.52秒（返信予算1秒）。同じNAT tuple/mark・共有PD identity、実DHCP更新/再取得、通常通信/運用記録/Tailscale exitも通過。全項目56.31/66.94秒は転送断時間ではない。make test-tools race vet、関連runtimeplan Race、fmt-check、通常build/3実行物static linkが通過。再実行は[試験計画](test-plan.md)末尾。node間の設定自動同期、pending中のcontrol交換、保持障害、物理機器と全34 packageは未受入。

次の実装差: 保存homertのWAN固定共有IPv4（/32）とそれを使う固定SNATに対し、現在の二台例はWAN DHCPアドレスのmasqueradeを使っている。この二つを同等の受入として扱わず、既存WAN上の共有アドレス/固定SNATを次に接続する。サイトの稼働IPは試験へ流用しない。

## 二つのLAN・経路・転送機能の二台HA受入

2026-11-03、基準main `adbd41f` + 設定/確認側変更、通常build `adbd41ffa4be`（製品Go不変）で、各52-resourceの実HGW二台手順が終了0。単体で受け入れた二つのLAN /16とDHCP profile、RFC1918 NAT除外、管理/WAN内SNAT、個別静的経路、WAN共有BGPの4私設prefix、公開DNS/NTP転送を二台例へ接続した。両方向のdaemon停止/再加入後にも実TCP/UDP、peer観測source、DHCP mask/gateway/DNS・Renew/Release/再取得・sticky/fingerprint、PD identity、DNS/DoH/NTP、管理/Tailscale/運用記録が通過した。既存captureでも両nodeのwan0で私設アプリ通信のIn/Outを確認した。

同一TCPはSYN1回・229応答・最大応答間隔15.13秒。転送集合外の公開DNSへの同一UDP socketは255応答・15期限超過・最大応答間隔8.51秒（返信予算1秒）。NAT tuple/markと共有DUID/IAID/prefixを保持し、再加入は非preempt。全項目63.12/72.55秒はsampling/再取得を含み、転送断時間ではない。初回はWANへ移したBGP経路のforward許可漏れを修正。別の実行は、継続UDPの旧宛先がローカル転送対象であると判明して中止し、集合外9.9.9.10へ変更した最終実行だけを上流UDP継続の受入とした。

上流選択は保存設定のDS-Lite主群120→別DS-Lite80→LAN静的gateway60と既存hysteresisを維持し、追加WAN/PPPoE/専用BGP NICは導入していない。`make test-tools race vet`、通常build、3実行物static link、両設定検証が通過。再実行は[試験計画](test-plan.md)末尾。これは実HGW接続下の隔離Linux二台であり、実homert二台、物理ONU/NIC、全private DNS、HA中の設定更新/保持障害、全34 packageの完成は残る。旧LAN gateway宛ての既存session継続は今回測定していない。

## BGPを既存WANへ接続

2026-11-03、基準main `ac0b1a0` + native設定/fixture変更、通常build `ac0b1a0980ed`（製品Go不変）で、BGPを保存設定と同じ既存WAN共有へ接続した単体43-resource実HGW手順が終了0。専用bgp0と相互forwardルールを削除し、BGP用benchmark addressをwan0へ設定。独立FRR2台も同じ既存uplink上の隔離peerへ接続した。4つの私設prefix（/24・/12・/16・/24）の実kernel gateway/interface、peerの通常LAN /16受信、両LAN clientの各prefixへのNATなしTCP/UDPを照合した。10/8 blackholeと192.168/16 on-linkより具体的なBGP routeが使われることも確認。

各peer停止/再加入後に残るpeerで全prefixの通信が成功し、再確認まで0.88秒/1.13秒。全daemon再起動後も全経路と通信を再確認。既存captureのwan0 In/OutでBGP179と私設アプリTCP/UDPを照合した。通常DHCP/DNS/NTP/Internet、管理、NAT/非NAT、Tailscale/運用記録も通過。confirmed rollback期限切れ29.90秒/control再起動6.26秒、PD SIGKILL2.96秒/通常再起動5.92秒、全daemon後の全項目再確認111.26秒。全項目時間は転送断時間ではない。20秒の2巡回・自然PD更新0回。通常build・3実行物static link、make test-tools race vet・py_compileが通過。

検証peerは独立FRRであり、実サイトのBGP peerへの接続ではない。WANには試験用192.0.2/24の副アドレスを使い、実サイトIPとの競合を避けた。物理NIC、BGP大量経路/長時間GR、二台の同構成は未受入。新規router WAN・host route・PVE/QEMU・追加PBR調査は行っていない。 再実行は[試験計画](test-plan.md)の「WAN共有のBGP」。

## 二つのLAN subnetと保存NAT除外の接続

2026-11-03、基準main `69ecf22` + native設定/fixture変更、通常build `69ecf22aa39d`（製品Go不変）で、通常/機器用LANを別の/16にした単体44-resource実HGW手順が終了0。通常LANは198.18.0.0/16、機器用は198.19.0.0/16、管理は重複しない10.255.255.0/24。DHCP/BusyBox hookは実応答のnetmaskを使い、予約/通常/OUI/個別MACの割当・実prefix・gateway/DNS・Internet・Release/sticky再取得を確認した。通常LANから実機器用DHCP client2台の名前解決も確認。保存設定にあったRFC1918宛先のSNAT除外を反映し、benchmarkの両LANも内部宛先として除外。私設/機器用宛てには元のLAN sourceが届き、管理/WAN内宛ての明示SNATとInternet向けSNATは維持した。

全daemon再起動後にも両subnetのDHCP/prefix/通信、NAT/非NAT、経路/blackhole、DNS/NTP/管理/BGP/Tailscaleが通過。confirmed rollback期限切れ30.72秒/control再起動6.41秒、PD SIGKILL2.60秒/通常再起動5.33秒、全daemon後の全項目再確認111.50秒。これは転送断時間ではない。20秒の2巡回・自然PD更新0回。通常build・3実行物static link、make test-tools race vet・py_compileが通過。NTPの提供許可は保存設定どおり通常LANのみで、機器用LANへのNTP提供とは数えない。

通常機能確認から毎回の上流障害注入を外し、既存の明示的なUPLINK_WAN_OUTAGE=1へまとめた。今回の最終版は健全な現用上流での実UDP/TCPを確認し、全段切替の再受入には数えない。追加の先行確認で0秒を指定し自然更新待ちに入った実行は明示終了し、合格に流用していない。最終版は20秒・全daemon再起動ありで終了0。二台設定への同構成の接続、全保存intent/実機資格と全34 packageは継続中。 再実行は[試験計画](test-plan.md)の「二つのLAN subnetとDHCP profile」。

## 保存IPv4経路とblackholeの単体通信

2026-11-03、基準main `0fbb5c3` + native設定/fixture変更、通常build `0fbb5c31fd34`（製品Go不変）で、4つの保存IPv4経路を接続した単体43-resource実HGW手順が終了0。LAN gateway経由の個別host/私設網、WAN on-linkで独立clientのTCP/UDPが届き、選択interfaceとpeerの実観測sourceを照合した。10/8 blackholeはTCP/UDP timeout、実kernel routeと短時間captureのLAN ingressあり/egressなしで確認。追加経路と管理側通信は基本DHCP/DNSの直後に先行確認し、全daemon再起動後にも再確認した。

通常Internet/DNS/NTP、DHCP、管理、BGP、運用記録、Tailscaleも通過。confirmed rollback期限切れ30.25秒/control再起動6.05秒、PD SIGKILL2.29秒/通常再起動4.98秒、全daemon後の全項目再確認111.44秒。全項目時間は転送断時間ではない。20秒の2巡回・自然PD更新0回。通常build・3実行物static link、make test-tools race vet・py_compileが通過。実Starlink/IX2215・物理NIC資格ではない。保存設定は異なる二つのLAN IPv4 subnetを使うが、単体例のDHCP profileはまだ同一subnetの別poolであり、次に実構成との差を解消する。 再実行は[試験計画](test-plan.md)の「保存homertの個別IPv4経路とblackhole」。既存namespace内のpeer設定だけを行い、host route・PVE/QEMU・追加PBR調査は増やしていない。

## 管理側・WAN内機器への単体転送/NAT

2026-11-03、基準main `96d0112` + native設定/fixture変更、通常build `96d01127742c`（製品Go不変）で、LAN→管理側/WAN内機器の単体39-resource実HGW手順が終了0。保存homertのRFC1918宛先除外、管理側forward/SNATを単体例へ接続し、単体WAN内宛ては現用DHCPアドレスを使うmasqueradeで表した。既存管理peerと既存WAN gateway namespaceの実DHCPアドレスへ、別namespaceの自動LAN clientからTCP/UDPで通信し、peerがsocketで観測した送信元が管理アドレス/実WAN leaseであることを確認。全daemon再起動後にも同じ通信が通過した。管理peer自身の自己宛て通信は使っていない。

管理SSH/Web、LANからの管理console拒否、公開DNS/NTP転送・Internet、DHCP/BGP/運用記録/Tailscaleも通過。confirmed rollback期限切れ30.23秒/control再起動5.68秒、PD SIGKILL2.27秒/通常再起動6.00秒、全daemon後の全項目再確認111.47秒。全項目時間は転送断時間ではない。20秒の2巡回・自然PD更新0回。通常build・3実行物static link、make test-tools race vet・py_compileが通過。これは実HGW接続下の隔離Linux peerでのNAT受入であり、物理HGW管理UI、HAのWAN VIPによる固定SNAT・全homert intentの受入ではない。 再実行は[試験計画](test-plan.md)の「LANから管理側・WAN内機器への送信元NAT」。既存peer/インターフェースを使い、host route・PVE/QEMU・追加PBR調査は増やしていない。

## 保存homertのDNS/NTP転送を単体構成へ接続

2026-11-03、基準main `efe9845` + native設定/fixture変更、通常build `efe984588553`（製品Go不変）で、保存homertの公開DNS/NTPの名前集合とLocalServiceRedirectを組み込んだ単体39-resource実HGW手順が終了0。両LAN端末から、実membershipの公開DNSアドレスを直接指定しても両IP UDP/TCPでローカルzoneの回答を得た。独立chronydの公開NTP宛て照会とconntrackのローカル転送も、保存設定と同じIPv4 allow範囲で確認。両方を全daemon再起動後にも再確認した。

同じ実行で通常の両IP Internet/DNS、DHCP scopes/sticky、BGP、NTP、Tailscale/管理/運用記録を確認。confirmed rollbackは期限切れ30.50秒/control再起動5.78秒、PD SIGKILL2.05秒/通常再起動7.01秒、全daemon後の全項目再確認111.77秒。各時間は定義した再確認までの時間で転送断時間ではない。DUID/IAIDと両端末/Tailscale通信、停止時のRA撤去/上流Releaseなしも通過。20秒の2巡回・自然PD更新0回。通常build・3実行物static link、make test-tools race vet・py_compileが通過。

最初の停止は、NTP clientの送信元をDHCPアドレスだとした照合側の誤り。端末の実routeが別の保有アドレスを選ぶことを観測し、そのprefsrcを使って再実行した。WAN用UDP/DNSは実転送集合外の宛先を使い、ローカルDNS処理をWAN疎通と混同しない。二台用設定への転送機能の統合、IPv6 NTP提供、全homert intent・物理NIC/ONU資格は未受入。PVE/QEMUと新規PBR調査は行っていない。 再実行は[試験計画](test-plan.md)の同名項目。

## 部分DNS失敗でlocal redirectの設定確定を止めない

2026-11-03、基準main `fa246ed` + 部分DNS失敗の準備判定修正、通常Makefile build `fa246ede2a5e`で実daemon/独立dnsmasqのlocal redirect手順が終了0（外側5.12秒）。旧routerdのresolveIPAddressSetは成功した名前のアドレスを使い、独立した失敗をstatusへ残す。native networkも部分membershipを適用していたが、coordinatorのCheckReadyが一件の解決errorで準備/確認を拒否していた。修正前は正常回答とREFUSEDの混在でhealth準備が10秒後も失敗することを再現。修正後はusable membershipを準備済みとし、REFUSEDをnetwork statusへ残したまま、両IP UDP/TCPのローカル転送、選外宛先のWAN転送、実DNS更新による旧転送先の解除/新転送先の適用、confirmed commit/confirm、daemon再起動、転送設定撤去を確認した。WAN captureで対象DNSの漏出なしも確認した。

これは実Linux kernel/実product processと独立dnsmasqの通信であり、実HGW全profileへ保存homertの2つのIPAddressSetとDNS/NTP LocalServiceRedirectを接続した受入は次の残作業。NTP redirect・HA・物理NIC資格には数えない。元fixtureに欠けていたICMPv6入力許可とLAN→WAN転送を明示し、確認応答は別の空structへdecodeするよう修正した。製品側に暗黙許可・DAD回避・待機延長は追加していない。通常build・3実行物static link、make race vetが通過。

再実行: `make check-local-redirect DOCKER='sudo -n docker'`。通常build後に最終fixtureを同じdaemon runnerで再実行した。

## 実private DNSとNextDNSの単体通信

2026-11-03、基準main `c371753`、通常build `6e0f020bf76c`（製品Go不変）で、保存homertの実private DNS/NextDNS設定を接続した単体39-resource実HGW手順が終了0。実NextDNS/Cloudflareの設定済みDoH上流すべてのHealthyを確認。安定した5転送先について独立LAN端末から両IP・UDP/TCPの20照会を行い、3先のSOA、2先のNXDOMAINとcore履歴の選択sourceを照合した。残る1先は事前UDP/TCP無応答から、試験中UDPのSERVFAILと後続NOERRORへ変化し、WAN captureで指定peer自身のNOERROR/NODATA応答を確認した。この先の固定SERVFAIL期待は試験側の誤りで、安定した4方式の受入には数えない。試験用local zoneは実suffixを隠さない名前へ変更し、DHCP/search/照会側も合わせた。新規WAN・host経路は追加していない。

同じ実行で通常の両IP Internet/DNS、DHCP scopes/sticky、BGP、NTP（host時計変更なし）、Tailscale/管理/運用記録、confirmed rollbackが通過。11巡回/自然PD更新0回、PD SIGKILL2.01秒・通常再起動4.87秒、停止時RA撤去/上流Releaseなし。全daemon再起動、HAでの実private DNS、全110-resource intent・実機資格は未受入。NextDNSは実endpointのprobe疎通で、全LAN照会がNextDNSを選んだという主張ではない。

再実行手順は[試験計画](test-plan.md)の「実設定のprivate DNS peerとNextDNS」。site intent/case/NextDNS URLは公開しない。通常Makefile build・3実行物のstatic link、Race Detector/vet・Python補助36件とpy_compileが通過。

## 単体構成のarchive接続と実履歴の期限切れ

2026-11-03、基準main `6e0f020` + native設定/fixture変更、実行build `799377d12628`（製品Go不変）で、archiveを接続した単体39-resource実HGW/Tailscale手順が終了0。保存homertのLogRetention sinksを再確認し、event/DNS/flow/firewall/DHCP fingerprintの保持へ既存syslog peerを接続した。通常通信・confirmed rollback・PD復旧の後、同じowner内で一時intentの期限2秒/間隔1秒を直列実行。archive専用の到達不能先で4 datasetの元記録を保持したまま、独立端末のDNS UDP/TCPとIPv4/IPv6 HTTPSが成功した。正常な受信先へ戻し、DNS query10件、終了済みflow10件、NFLOG拒否2件、DHCP fingerprint5件について元の識別子/時刻のsyslog受信と期限削除を照合した。元の7日/daily/vacuum intentへ戻した後も通信が成功した。

同じ実行で通常/特別DHCP scope・sticky、二段/条件付きDNS/両DoH、BGP peer障害復旧、nDPI/計数/SNI、NTP、syslog/OTLP、Tailscale/exit、両IP公開通信が通過。PD worker SIGKILL/正常再起動から再確認まで2.01/4.99秒、停止時のzero-lifetime RA/kernel撤去・上流Releaseなしも通過した。短時間2巡回・自然PD更新0回、全daemon再起動は今回未実行。通常Makefile build（後続build ID `6e0f020bf76c`）・3 static実行物、`make race vet`、Python36件・py_compile、3設定例のschema検証が通過。

再実行: `make check-uplink-retention DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18 UPLINK_WAN_MAC=02:72:6f:75:74:03 UPLINK_CONFIG=examples/single-router-dslite-dhcp-dns.yaml UPLINK_SECONDS=20 UPLINK_TAILNET=1`。通常巡回と設定変更を並列に競合させず、同じownerが最後に保持試験を実行する。DB行/時刻やhost時計は変更しない。

これは明示した短いpolicyの自然期限・送信error/復旧と27件の受信確認であり、7日経過、lossless UDP配送、全signal/物理機器の資格ではない。変更した二台設定例はschema検証済みで、このarchive条件のHA実通信は未実行。

## 失敗した署名候補からの実通信復旧

2026-11-03、通常build `30054e56fdf6` / `799377d12628`の全factory署名bundleと独立RP-PPPoE/ISC/Linux LANで、更新→明示的復帰→失敗候補からの自動選択復旧が終了0。確認済み版へ再度rollbackする候補の起動時に、router namespace内の独立UDP processでDNS port53を占有した。実DNS workerのEADDRINUSEを観測し、11:03:26 UTC開始の既定120秒trialが11:05:26 UTCに失敗して製品自身がrebootを要求した。読取専用CLIで失敗digest、以前のactive、pending/attempt解消を確認。競合process終了後、保存選択/設定を手修復せずPID 1を再起動し、旧runtimeとpaired startupのrecovered記録、revisionの採用、更新後に追加したDNS回答、DUID、DNS UDP/TCPと両IP TCP/UDPが復旧した。high watermark=2、正常poweroff、captureの上流PD Releaseなしも通過。

同sequenceのprepare再送はhigh watermarkで拒否される。確認済み保存版の選択には既存のrollback APIを使い、境界を緩めない。DNS競合時の最初の期限処理ではrelease store busyも記録され、その後の停止処理で失敗状態が保存された。上記の実保存状態と復旧を確認しており、busyを候補正常確認へ読み替えていない。製品Goの変更なし。通常Makefile build/3 static実行物、`make race vet`とPython補助36件が通過。再実行は試験計画の`make check-runtime-update`。

これはOS待受競合による候補失敗と、外部競合解消後の復旧の確認。両buildの製品Goは同一で、同じkernelのPID namespaceを再起動した。実装/ABI変更を跨ぐ更新、kernel/bootloader再起動・停電、競合が続いた状態でのDNS提供、全homert profile/ONU/NIC資格は残る。PVE/QEMUと追加PBR調査、共有host時計の変更は行っていない。

## 通常runtimeの実通信更新・復帰

2026-11-03、通常署名bundleを実control APIでupload/stage/prepareし、実PID 1のrebootと再起動で `b3a0053f24c6 → 30054e56fdf6 → b3a0053f24c6` の更新・明示的rollbackが終了0。独立RP-PPPoE/ISC DHCPv6/Linux LANを使用し、全workerのrelease ID、実health watcherの自動confirm、現在の設定/DUID保持、DNS UDP/TCPと両IP TCP/UDP、high watermark=2の保持を確認した。候補でDNSレコードを追加してから旧runtimeへ戻し、その追加設定の両transportでの実回答も維持した。startup採用時は設計どおりrevisionが進む。正常poweroffとcaptureの上流PD Releaseなしも通過。

再実行は`make check-runtime-update UPDATE_BASELINE_DIR=<以前の通常build> DOCKER='sudo -n docker'`。候補はsource `30054e5`の通常Makefile buildで3実行物のstatic linkを確認した。試験側の固定revision要求を除き、実startup採用記録とrevisionの対応へ修正した。大きなbundleの送信はstreaming uploadを使う。補助`make test-tools`36件、`make race vet`、通常Makefile buildと3実行物のstatic linkが通過。製品Goは変更せず、既存の更新/復旧処理を実peerへ接続した。

製品Goは両buildで同一で、異なる通常release IDの実行物を選択・execする経路の確認。PID namespaceを同じkernel上で再起動した試験であり、kernel/bootloader再起動、停電、失敗候補の自動fallback、異なるABI/実装間の更新、全homert profile/ONU/NIC資格は残る。全構成のappliance NTPには独立kernel/時計の実行機が必要で、共有host時計は変更していない。PVE/QEMUは実行していない。

## 通常署名bundleの実PID 1・PPP/PD受入

2026-11-03、基準main `b3a0053` + 本変更の通常build `b3a0053f24c6`で、全25役割・5 engineの署名bundleを実receiverでstage/initializeし、Linux namespaceの実PID 1から起動した。独立RP-PPPoE/ISC DHCPv6/Linux LAN端末とのPAP・CHAPが両方終了0。DNS UDP/TCP、両IP TCP/UDP、自然Renew、server停止によるPD期限切れ/再取得、worker交換/DUID保持/古い通知拒否、confirmed rollback、PD/RA削除復元、PPP MTU変更、手動/control再起動、正常poweroffが通過。captureでMTU変更・手動/control再起動のRA zero-lifetime通知がPADTより前であり、上流Releaseなしと確認した。

実通信で検出した2件を修正した。停止指示前のReady状態で消滅したPPP linkへ経路を追加する競合は、停止指示後に次のowner巡回で現在状態を取得してから適用する。手動再起動の一斉停止は既存の設定変更用dependency drainへ統一し、PD/RAをPPPより先に撤去する。待ち時間延長・追加retry・新しい状態は入れていない。試験専用catalog/不正bundle生成を削除し、通常配布入口を使用する。旧「通知期限で生きたPPP routeを消す」要求と、PD削除時に参照RAを残す試験設定も修正した。

最終`make check`（Go/Race Detector/vet/Python36件）、変更後のruntimeplan Race、通常Makefile buildと3実行物のstatic linkが通過。試験用process/namespace/key/展開storeは終了済み。次は同じ通常配布経路で全homert profileの更新・復旧を確認する。

実行物SHA256: `routerd`=`01b1ec3c511766b0ead2a363856901a17f8ad775364bfe3ea79e1f4c244f7271`、`routerd-modules`=`0fd2f8e8cf0c71825179bd79f48de37f861dfa9cef2807de1a8b64f279b6fb93`。再実行は試験計画の「通常署名bundleのPID 1起動とPPPoE/PD」。これは実kernel/PID 1と独立peerでの配布boot・PPP/PD復旧の受入。ISPのPPPoE、ONU/NIC実機、全homert profileの署名更新/rollback、全34 packageの完成は残る。homert上流候補へPPPoEや追加NICは導入していない。

終了0（2026-11-03 07:42〜09:46 UTC）: source/build `df49035`/`df490351c3ff`、単体38-resource profileで実HGWとの自然Renew/Reply1組と期限更新、623巡回の両IP HTTPS・UDP/TCP DNSを確認した。自然更新後の619巡回時点まで全28 worker generation1/Ready=trueを観測。PD SIGKILL2.90秒/通常再起動5.92秒、全daemon後の全項目再確認105.73秒も通過し、identityと両端末/Tailscale通信、停止時のRA撤去を確認した。105.73秒は抑制/samplingを含む全項目時間で、転送断時間ではない。owner/process/namespace/stageは撤去済み。二段DNS・候補設定修正の別受入、既存socket継続、全homert/実機資格とは範囲を区別する。

通常build `842c47e32300`で新しい`make runtime-bundle`が終了0。25役割・5 engine・15 artifacts、527,315,456 byteの署名配布物へsourceと303 license資料を同梱した。実receiverでstage/verify/initialize/statusと後続候補のprepare/cancelが通過し、元のactiveと増加済みwatermarkを維持。独立OpenSSLも署名を検証した。入力はprivate snapshotへ固定し、nDPIのsourceコピーをproduction/build入力へ限定してroot private probeとsite boot treeの非収録を確認した。再実行は[配布手順](runtime-bundle.md)。これは配布物と保存選択の受入であり、このbundleの実機起動・全profile更新は残る。

基準main `1fa7cf0` + 候補設定の観測保持修正、通常build `1fa7cf099ae0`で単体39-resourceの実HGW手順が終了0。30秒のconfirmed commitでcoreのAレコードとLAN cache設定を変更し、両IP/UDP/TCPの候補回答と公開通信、無確認期限切れとcontrol SIGKILL後の元intent全体・回答・公開通信への復帰を確認した。候補疎通確認後から元の疎通を再確認するまで33.42秒/9.30秒で、転送断時間ではない。DNS編集と期限切れrollbackではWAN DHCPv4/RA/PDのPIDを維持した。PD SIGKILL3.21秒/通常再起動6.72秒、全daemon後の全項目再確認103.04秒も通過した。

候補準備で全ての観測を空にしていた処理を除き、同じworker設定のproducerと、同じ設定・依存関係の適用済みresourceだけを元の有限期限のまま評価する。準備段階は現在の条件履歴・イベント/derived ownerを進めない。小さな回帰確認では旧関数が無関係なMTU編集で稼働中consumerを停止することを再現し、修正後は維持、WAN自体の変更では旧観測を流用しないことを確認した。世代番号は他の退役workerからも引き継ぐため、番号だけで個々の再起動回数を推定しない。 `make check`（Race Detector/vet/補助36件）、最終のruntimeplan Race、通常Makefile buildと3実行物のstatic linkが通過。これは設定transactionと実通信の受入であり、署名runtimeの更新/boot、HA中の設定変更、全homert private peer、全34 packageの完成には数えない。

今回の補助確認: `make test-tools race vet`、3設定例の検証、通常`make build DOCKER='sudo -n docker'`が終了0。後続build IDは`9fe72fede476`で、3実行物のstatic linkを確認した。実通信で実行したbuild IDは上記`df490351c3ff`であり、製品Go sourceは同一。

基準main `9fe72fe` + 二段DNS設定、製品Go不変の通常build `df490351c3ff`で単体39-resource実HGW手順が終了0。LAN resolver（cache1000・queryLogなし）からcore resolver（cache10000・queryLogあり）へloopback UDP53で転送し、coreがzone/条件付き/学習DNS/DoHを所有する。実端末からの新しいqueryと同じIDのloopback往復・回答をcaptureで照合した。両IPのDNS/Internet、条件付き上流の停止/cache/SERVFAIL/復旧、coreのcache hit/履歴API/Web、DHCP各scope・sticky、BGP/NTP/Tailscale/運用記録が通過。PD SIGKILL3.16秒/通常再起動7.06秒、全daemon再起動後の全項目再確認103.15秒。後者は抑制/samplingを含み転送断時間ではない。自然PD更新0回・通信round2の短時間実行であり、長時間安定性や全homert private peerの受入には数えない。 同じ二段DNSの各48-resource二台profileも実HGWで終了0。両方向のDNS/DoH・履歴API/Web、DHCP更新/Release/再取得・sticky/fingerprint、PD identity、BGP/NTP/Tailscale/exitと運用記録を確認。同一TCPはSYN1回・209応答・最大応答間隔10.89秒、UDP239応答・19期限超過・最大応答間隔10.52秒（1秒返信予算）。全項目確認58.91/73.57秒はsampling/再取得を含み、転送断時間ではない。二台のprocess/namespace/stageは撤去済み。短時間の両方向受入であり、長時間安定性・全private DNS peer・実機資格は残る。

2026-11-03、製品code `df49035`・通常build `df490351c3ff`で `make check-doh-outage` が終了0。実Cloudflare/GoogleへTLS通信し、片側ずつTCP443を遮断して各16packetのdropとDown、解除後の両Healthyを確認した。cache無効の独立dig端末は全段階でUDP/TCPのNOERROR/A回答を取得。Cloudflare停止直後の初回回答1.043秒、以後の確認回答0.025〜0.063秒。新DNS node/client/process/stageは撤去済み。元の38-resource自然PD試験への障害注入は行っていない。公開DoH IPv4 transportの独立受入であり、DS-Lite/HA全体の障害受入には転用しない。 再実行条件は[試験計画](test-plan.md#公開dohの片側停止復旧)に記載。Go製品codeは変更していない。今回の補助チェック `make test-tools` は36件通過。

同じ稼働中の実DHCP割当を読み、その送信元へbindしてMTU/MSSを確認した。IPv4合計1500byte・DF=1の実pingが公開peerと往復し、WANでAFTRからの同一IDのIPv6断片2個（offset0/752、payload752/748）を観測した。TCPは同じ実tuple/sequenceでLAN SYN MSS1460→DS-Lite送信1414、受信SYN/ACK1420→LAN1414となり、HTTPSが成功。ここで確認したのは選択されたDS-Lite経路の往復・受信側外側IPv6再構成と双方向MSS調整。B4側送信断片化、実IPv6 PTB/外側MTU縮小の全条件や性能は未受入。[RFC6333 section5.3](https://www.rfc-editor.org/rfc/rfc6333.html#section-5.3)に照らし、1500byte DFを一律のICMP拒否条件にしない。

最新修正（基準main `a0ec3ac` + 本変更）: BGP/VRRPの元の期限より前に送信を打ち切る共通IPCの3秒制限を外した。AFTRは一つのループでtyped Input取得→snapshot通知を順に行い、入力世代を並列更新するgoroutine/channelを削除した。周期/取得budgetと元の依存期限は維持する。旧routerdのDS-Lite単一reconcileとcallerを照合した。supervisorの状態APIも管理ループへ要求して応答し、登録より古い状態を返すcache/mutexを削除した。producer未登録とAFTR入力世代不一致には、元のErrStaleを保ったまま原因を示すcontextを付けた。互換経路・retry・新しいepoch/stateは追加していない。

最終通常build `a0ec3ac167e8` + 本変更で実HGW二台47-resource手順が終了0。同一TCPはSYN1回・245応答・最大応答間隔11.54秒、UDPは260応答・19期限超過・最大応答間隔10.53秒（1秒返信予算）。全項目確認61.87/76.33秒はsampling/再取得を含む。両方向のDHCP更新/Release/再取得/sticky/fingerprint、PD identity、DNS/DoH、BGP/NTP/Tailscale/exit、管理、fresh nDPI/履歴/syslog/OTLPが通過。最終の再加入前まで両nodeのworker停止理由を照合し、AFTR409/BGP/VRRP timeoutは未観測。短時間の未再発であり、自然更新を含む長時間安定性はこれから確認する。

先行単体38-resource実行（状態API変更前）も実HGWで終了0。通常/予約/特別端末、4 DS-Lite、DNS/DoH、BGP片側停止、NTP/Tailscale/運用経路、PD SIGKILL2.07秒/通常再起動6.02秒、全daemon復旧102.79秒を確認した。ただし初期AFTR409は残り、登録観測の修正につなげた。この先行結果を最終状態API変更後の単体長時間受入へ流用しない。

AFTRの実ソケット/独立DNS試験と専用daemon手順が終了0。前者の500ms入力は1秒の取得周期より前に失効し、利用不可となった。fixtureは取得後のliteral選択と元の期限での撤去を確認できる2秒の明示grantへ更新し、製品の期限は変更していない。更新された入力callbackと既存検証を併せて確認した。最終`make check`（Race/vet/Python36件を含む）と通常static buildが通過。補助成功を通信/安定性の代用にしない。

以下は前checkpointの結果。

今回の修正: supervisorが停止中workerの期限切れheartbeatを観測全体の期限へ混ぜ、正常な兄弟workerまで失効させる処理を除いた。停止対象のReady=false、正常workerの元のheartbeat期限、全体の未準備状態は維持する。新しい状態・タイマー・再試行は追加していない。実fork/execのworker一つをSIGSTOPし、heartbeat timeoutから停止中へ入ると正常な兄弟を含むExpiresNanos=ObservedNanosになることを旧コードで再現し、修正後は同じ手順と既存heartbeat検出が通過した。Race Detector/vet/Python補助36件、通常Makefile build、3実行物のstatic linkも通過。

修正後の実通信: 基準main `85259c1` + 本変更、通常Makefile build ID `85259c1a892c`で `python3 scripts/uplink-ha.py --uplink ens18` が終了0。実HGWと独立端末/peerを使う二台47-resource profileで、初期・両方向の停止/再加入後の通常/特別DHCP更新・Release/再取得・sticky割当/fingerprint、両IP公開通信、条件付きDNS/両DoH、BGP、NTP、Tailscale/exit、管理、fresh nDPI/履歴/syslog/OTLPが通過。同一TCPはSYN1回・228応答・最大応答間隔11.50秒、UDPは246応答・17期限超過・最大応答間隔9.52秒。全項目確認60.75/73.57秒はsampling/再取得を含む。cold startでAFTR HTTP409、WAN停止、BGP/VRRP適用timeoutを再観測したため、安定起動/長時間安定性は未受入。

起動診断では、通常kernel適用0.2～0.4秒、負荷/停止時には1.7秒程度まで伸び、依存worker停止中の全体適用見送りも観測した。ただしBGP/VRRP effect timeoutはまだ再発しており、上記の巻き込み修正だけで原因全体を解消したとは扱わない。時間計測コードはすべて除去済み。別の先行実行は特別端末→独立gateway DNSの接続拒否、別実行は初期公開TCPのNo route to hostで終了した。DNS拒否の正確な原因は未確定で、次の実行では独立gatewayの待受/実応答を確認し、障害時のpeerログと100packetに限定したcaptureを既存owner内で取得する。これは合格への読み替えではない。

以下は前checkpointの結果。

最新実行: 基準main `c144132` + 本変更、通常Makefile build ID `c14413250ebd`、各47-resource profileで `python3 scripts/uplink-ha.py --uplink ens18` が終了0。実HGW、独立BusyBox端末4台、FRR2台、chronyd、Headscale/Tailscale、syslog受信機、公式Collectorを使用。両方向の停止・再加入で、通常/特別scopeの実Renew/ACK・DNS/公開HTTPS、特別3端末のRelease/再取得とsticky同一割当、再取得した現用機のfingerprintを確認した。新規公開通信の双方向nDPI計数/TLS SNI、実管理拒否の履歴、元の記録時刻とnode識別を照合したfresh syslog/OTLP logs/metrics/tracesも初期・両方向の切替後に通過した。

同一TCPはSYN1回、249応答、最大応答間隔10.82秒。同一UDPは287応答・15期限超過・最大応答間隔8.52秒（1秒返信予算）。両方向のNAT tuple/mark、DUID/IAID/prefix、再加入/同期/非preempt、DNS/DoH/BGP/NTP/Tailscale/管理を維持した。全項目確認まで80.40/73.30秒には30秒sampling待ちと特別端末のRelease/再取得を含み、転送断時間とは区別する。

VRRPの余分な2秒適用待ち設定を削除し、元の選挙期限の残り時間へ統一した。期限切れ判定とwithdrawalは維持する。ただし今回のcold startでもBGP/VRRPの適用待ちtimeoutと依存workerの再起動を観測したため、これだけで起動安定性が解決したとは扱わない。次はcoordinatorの実適用待ちと依存worker停止の時間関係を観測し、待ち時間を延ばす対処ではなく原因を直す。

切替後fingerprint待ちの先行失敗はrunnerの前提誤り。配布用dnsmasq2.92_p2と独立BusyBoxの実割当→再起動→Renewで、ACK後も`DNSMASQ_DATA_MISSING=1`・要求optionsなしの通知を再現した。upstream helper/lease/rfc2131実装も照合し、通常Renewには取得時metadataがないことを確認した。データのない通知からfresh記録を捏造せず、実Renew/通信と実Release/再取得/fingerprintを別に確認する。fingerprintはlease同期の対象ではない。保存設定も再読し、ens20（homert02）/eth2（homert03）は管理用、上流順序はDS-Lite群120→別DS-Lite80→LAN静的gateway60、PPPoE候補なしと確認した。

通常Makefile build ID `875dfa6f1f84` + 本変更で、実HGWへ接続した二台47-resource profileの拡張手順が終了0。条件付きDNS/DoH・BGP・NTP・両IP Internetに加え、独立Headscale/Tailscale端末とのLAN→peer IPv4/IPv6、peer→LAN IPv4のTCP/UDPを両方向の現用停止・再加入後に確認した。LAN subnet routeは独立端末で手動変更せず切り替わった。最後に現用nodeを明示選択したIPv4/IPv6 exit HTTPS/UDP DNSも通過（同一exit node IDの自動引継ぎの受入ではない）。両nodeへのSSH鍵認証・保持host key、現用側のloopback/管理DNS・Webの実DNS履歴、LANからの管理拒否も通過した。

全項目の復旧はa→b36.82秒（途中probe失敗1回）、b→a36.13秒（3回）。同一公開TCPはSYN1回、117応答、最大応答間隔11.68秒。同一UDPは126応答・17欠落・最大応答間隔9.52秒（1秒返信予算）。両方向でNAT tuple/mark、DUID/IAID/prefix、実DHCP更新、再加入/同期/非preemptを維持した。切替断の短縮と長時間安定性は引き続き必要。

TailscaleのLAN通信は、両側で常時広告していた旧二台例で実際に失敗した。現用からtailscale0へ出るSYNをpacketで確認し、広告を現用権に合わせた設定へ修正した。独立制御サーバーの明示policyは試験userのLAN/exit広告だけを事前許可し、切替後の手動承認を合格条件にしない。各nodeの保持鍵・名称は別のまま再利用する。

BGPは観測の元の5秒期限より短い2秒で適用待ちを切っていた。余分な`effect_timeout_milliseconds`設定をBGPのproducer/consumerから削除し、RIB走査に使った時間も差し引いた元の残り期限で待つ。期限を更新して待ち直す処理は追加しない。Race Detector・vet・Python補助検査36件・通常build・3実行物のstatic linkが通過。短時間の二台通信は確認済みだが、単体の長時間BGP安定性は再受入が必要。VRRPの適用待ち再起動と初期AFTR HTTP409も残る。

今回の補助検査: `make check`（Race Detector/vetを含む）、通常Makefile build、3実行物のstatic link確認が通過。最初の同時実行ではstorage guestの150ms診断待ちが一度失敗し、実通信終了後の無変更再実行は終了0。期限や期待値は変更していない。終了した実通信ownerはnamespace/process/key/stageを撤去済み。

## 今回の自然更新とHA修正

自然PD更新の長時間試験は終了0。通常build `b03debf15982`、単体38-resource profile/runner `5861b7e`で、実HGW source port49153から05:24:16 UTCのRenew/Replyと期限更新、678回の両IP HTTPS/DNS確認を通過した。PD SIGKILL後2.22秒、通常再起動後7.05秒、全daemon再起動後112.21秒で復旧し、DUID/IAID/prefix・両端末/Tailscale通信を維持した。正常停止のzero-lifetime RA、kernel撤去、上流Releaseなしも確認した。BGPはgeneration17まで再起動したため、これは自然更新と復旧の受入であり長時間安定性の合格ではない。最新のRA寿命/SNAT/HA修正を含む全体の資格にも転用しない。stage/process/namespaceは撤去済み。

二台用設定の両側常時LAN広告では、独立FRRの戻りが待機側へ入りSYN/ACKをINVALIDとして破棄する実障害を確認した。保存中homertのBGPのexport/connected intentと旧routerdのrender/route同期を参照し、このstateful二台profileのBGPは現用権で有効化する。新しい通信手順は両FRRの実経路が現用側だけを向くこと、端末TCP/UDPの非NAT往復、条件付きDNS/子suffixと両DoH上流を切替前後に確認する。

製品の修正は、経路適用に不要なkernel route dumpのsort削除と、VIPが未公開の待機/準備通知を既に適用したVIP不在で応答する処理。これにより、準備を進める通知がその準備の完了を待ってtimeoutする依存を外す。準備条件が満たされた現用化とVIP撤去は実適用を待つ。診断用のstack timerは除去し、通常Makefile buildで上記の最終確認を実施した。

## 前checkpointの単体結果

38-resource native profile、基準main `b03debf` + LAN配布設定、build ID `b03debf15982`で実HGW統合gateが終了0。通常・予約・OUI・特定MACの独立端末5台で、通常gateway/DNS .1と特別gateway/DNS .253を実ACKから設定し、公開HTTPS/UDP DNSが往復した。通常/予約端末はNTP2アドレスとdomain-searchも取得。実Releaseで3日分の保持期限が保存され、端末IP撤去後も別MACの同アドレス要求には別leaseを配り、元MACには保持アドレスを再配布した。全daemon restart後も特別端末3台のRenew/ACK・通信とfingerprint APIが復旧した。LAN RAのhigh優先度/MTU1500/valid最大7200秒/preferred最大3600秒を実packetで確認。上流選択、4 DS-Lite、DNS/DoH、nDPI/計数/SNI、fresh syslog/OTLP、BGP/NTP/Tailscaleも通過。PD worker SIGKILL/通常再起動後の復旧2.96/5.85秒、全daemon restartから全確認102.76秒（60秒抑制と30秒sampling等を含む）。2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。自然T1・既存socket継続・3日/7日経過削除・全homert/HA/実機資格は今回の範囲外。

- 製品Goは`b1a3e48`から不変。今回Race/vet、Python checks/static build/設定検証が通過。7日/daily/vacuumをoperational event・DNS/flow/firewallへ設定した。全datasetの保持/期限削除を今回実証したことにはしない。
- backupをDHCPとした誤った追加は削除済み。実homertは静的IPv4。実設定の低優先候補は既存LAN上の静的gateway。ens20追加WANと新規static IPを前提にした依頼は撤回済み。4 DS-Liteの個別疎通に続き、既存優先順位の選択と両LAN端末の通信も受入済み。
- 旧routerdのBGP広告callerとTailscaleのrender/chainを参照した。専用CA必須という制限を除き、OS標準CAを専用rootへ渡す。upstream既定のsubnet SNATを維持する。
- homert02/03の実設定は`chatty.lain.local:~/homert02/hosts/{homert02,homert03}/router.yaml`に各110 resources。秘密は転記しない。native単体例38 resources、各homert例47 resourcesへ拡張し、実HGWで二台の初期通信/片方向復旧を確認。全設定受入ではない。
- 自然PD T1、WAN遮断、Health→PBR、appliance/updateの別gateと未受入範囲は[試験計画](test-plan.md)に簡潔に残した。raw log/pcapは保持しない。

HA基盤の最新確認: 基準main `9fb64f4`の通常実行物、build ID `9fb64f4267e9`で`make check-ha-state`が終了0。独立Linux namespace/PID1の2 product processとclient/serverで、A→B/B→AのVRRP/NAT引継ぎ、同一TCP socketとUDP mapping、同期済みDHCP leaseの実Renew/ACK、worker/conntrack engine再起動を確認した。TCP再接続なし（captureのSYNは1回）、最大応答待ち1.665秒。UDPは50ms送信/200ms返信予算で3009送信・2942受信・67期限超過、最大無応答1.110秒（このfixture条件での測定）。同期断中の独立割当/更新と復帰、未設定証明書の拒否、実lease期限での失効、3日sticky期限を変えない再起動/引継ぎ、保持中の別client割当と元client再取得も通過。設定削除で同期workerを回収し、元のconntrack sysctlへ復元した。実HGWの38-resource全体を二台化した結果や、14-resource例・実homert二台の資格とは分ける。

## 二台profileの初期通信

2026-11-03 04:12 UTC: 通常Makefile build ID `a3444d519fb0`（製品Goはwildcard NTP listener修正`19682da`を含む）と各47-resource二台profileで、実HGWの共有WAN RA/PD、共有LAN IPv6 identity、VRRP準備後公開、実DHCP端末のIPv4/IPv6 HTTPS・UDP/TCP DNS・独立chronyd `-Q`によるVIP NTP応答が通過した。現用aの正常停止後、同じ端末設定のままbで全項目が復旧し、停止開始から再確認まで17.01秒、途中の試行失敗3回。RA/NA packetとkernelも観測した。無停止・既存socket継続・逆方向復帰・全機能安定性の受入ではない。初期のVIP DNS拒否は`accept_mode: true`で修正した。単発失敗で試験を止める条件は廃し、断と復旧を記録する。

NTPのワイルドカード待受を許可する製品修正`19682da`はmainへpush済み。Race/vet・通常build・3実行物のstatic linkとPython checksが通過。二台profileの全サービス/両方向復帰は残作業。

## 次の作業

1. cold startで再現したBGP/VRRP適用待ち・依存worker再起動の原因を直し、同じ単体/二台profileで起動と長時間安定性を受け入れる。初期AFTRのHTTP409/restartと切替断も残る。
2. 今回通過した二台の特別scope・nDPI/履歴・syslog/OTLPに続き、未確認の保持期限・archive障害/復旧と長時間動作を実peerで受け入れる。短時間通信の通過と安定性を分ける。
3. 保存homert全意図と保持dataset、MTU/期限/更新/配布、残る34 work packageを完了する。対象ONU/NIC・実機性能・arm64・cloud等は未受入。各実通信の直前に[試験計画](test-plan.md)全体を読む。PVE/QEMUと追加PBR調査は実行しない。

全体開始から9日20時間6分、記録済み期限から4日14時間38分経過（07:35 UTC）。全体ゴールは継続中。終了したstage/key/capture/log/重複build outputを撤去し、次の観測で必要な実行物・pinned peerだけを残す。
