# homert設定のnative対応表

## 対象と範囲

2026-11-03にhomert02/03設定を再読し、resource種別・参照関係と利用する通信経路を確認した。各設定は110 resourcesで、全kind countと抽出できた型付き依存関係は一致した。以前の109件からの差はDNSForwarderが1件増えたことだった。両方にIPv6 host-traffic policy route（table 120）がある。設定値、アドレス、identity、credential、鍵はこのリポジトリへ保存していない。

[homert02の例](../examples/homert02-native.yaml)と[homert03の例](../examples/homert03-native.yaml)は、文書用アドレスを使ったResourceProgramの例である。各54 resourcesで、DHCP、DNS、PD、RA、DS-Lite、BGP/NTP/Tailscale、管理・履歴とVRRP/lease replicationを含む。実HGW・隔離Linux二台・独立端末との両方向引継ぎは受入済みで、実行版と範囲は[引き継ぎ](pve-codex-handoff.md)へ集約する。実サイトの110 resourcesすべての意図と物理機器の受入は継続する。旧routerdの[`DHCPv6ServerLeaseSync`契約](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/docs/how-to/dhcp-lease-sync.md)はstateful DHCPv6 address poolのlease state同期用である。保存設定のLAN DHCPv6 serverはstatelessでpoolを持たないため、この同期機能は現行homertの単体・HA動作に必要なstateを表していない。

## 機能対応

件数は両方の保存設定から読み取ったresource種別の数である。対応欄は現在のResourceProgramの表現と実行経路である。resource件数と、利用者が必要とする機能・設定・実通信の受入範囲を分けて確認する。

| 保存設定のresource種別（件数） | routerd2のnative表現 | 状態・未確定事項 |
| --- | --- | --- |
| `Interface` (10), `IPv4StaticAddress` (7), `IPv4Route` (4) | `interface`、`address`、`route`、`host_route`、`egress_route` | 個別経路・on-link・blackhole、管理/WAN/LANの分離を単体・二台例で適用し、kernel経路・独立端末のTCP/UDPとpeer観測sourceを確認済み。サイト固有の全interface/経路と対象NICは残る。 |
| `IPv6DelegatedAddress` (5), `VirtualAddress` (2) | `service.dhcp6.delegations`、`service.vrrp` のaddressとVIP interface | PD由来のLAN addressとB4 /128、共有VIPを単体・二台例へ接続。実HGWの委譲と両方向引継ぎで所有・撤去・端末通信を確認済み。全サイト値と物理二台での受入は継続する。 |
| `DHCPv4Server` (1), `DHCPv4Reservation` (2), `DHCPv4ServerLeaseSync` (1) | `service.dhcp_server` のscope/reservationと `ha` TLS replication | 通常/機器用の二つの/16、予約、MAC/OUI selector、3日sticky、fingerprintを接続済み。実割当/更新/Release/再取得と、同期済み・新規端末のHA引継ぎを確認済み。3日/7日経過、同期未受信の割当消失とpartitionは残る。 |
| `DHCPv6PrefixDelegation` (1), `DHCPv6PrefixDelegationLeaseSync` (1), `DHCPv6Information` (1) | `service.dhcp6`、`dhcp6.ha`、delegation、RA client factと各consumer | PD/InformationとRAの学習情報を型付き期限で渡す。45-resource単体例は実HGWの自然Renew/Replyとworker/daemon再起動、二台例は共有DUID/IAID/prefixと引継ぎを確認済み。保存済みサイトidentityの本番引継ぎと物理機器は残る。 |
| `DHCPv6Server` (1), `DHCPv6ServerLeaseSync` (1), `IPv6RouterAdvertisement` (1) | `service.dhcp6_server`、`service.ra` | 保存設定はpoolのないstateless DHCPv6である。旧routerdのlease-syncはstateful address pool用なので、このprofileでは同期すべきserver lease stateがない。native server DUIDは各nodeで永続化され、対になるサービスでは同じ明示DUIDを使える。WAN DHCPv6 clientのDUID/IAIDとPD lease同期は別の機能である。 |
| `DNSResolver` (2), `DNSForwarder` (11), `DNSUpstream` (13), `DNSZone` (1) | `service.dns` のlistener、suffix source、upstream、zone、DHCP由来record | 二段resolver、条件付き転送、学習DNS、DoH、静的/lease A・AAAA/PTR、cache/historyを単体・二台例で接続済み。保存private DNS/NextDNSの実通信条件と残るpeerは[引き継ぎ](pve-codex-handoff.md)へ集約する。全private peerの二台受入は継続する。 |
| `DSLiteTunnel` (4), `EgressRoutePolicy` (3), `HealthCheck` (6) | `service.ds_lite`、`host_route`、`egress_policy`/`egress_route`、`service.health` | 保存設定のDS-Lite主群120→別DS-Lite80→既存LAN静的gateway60と60秒hysteresisをnative設定へ接続し、Health/owner・kernelと端末通信を確認済み。単体の自然PD更新も通過。追加WAN/PPPoE/ens20を条件にしない。 |
| `NAT44Rule` (7), `NAT44SessionSync` (1) | `nat` resource、`service.vrrp.conntrack_sync` | 固定共有WAN /32、管理SNAT、内部宛てのNAT除外を接続し、peer観測sourceを確認済み。公開/固定WANの既存TCP/UDPとNAT tupleは両方向HAで維持。同期未受信・大規模state・物理機器の受入は残る。 |
| `FirewallZone` (3), `FirewallPolicy` (1), `FirewallRule` (2), `IPAddressSet` (2), `LocalServiceRedirect` (1), `FirewallEventLog` (1) | `firewall` rule、address set、local redirect、`service.firewall_log` | native chain/ruleとaddress setへ意図を表現。管理アクセス制限、DNS/NTP redirect、NFLOG履歴と部分DNS失敗時の有効membershipを実通信で確認済み。保存zone/policyの全field照合は継続する。 |
| `BGPRouter` (1), `BGPPeer` (1) | `service.bgp` | 既存WANを共有し、独立FRR二peerとLAN広告・学習経路・撤去/再加入・NATなしTCP/UDPを確認済み。受信数とglobal import policy後の受理数も実広告と照合。実サイトpeer、大量経路/GR全期限は残る。 |
| `NTPClient` (1), `NTPServer` (1) | 一つの`service.ntp`のsource group、`listen`、`allow` | auto学習/fallbackと常時固定3源を一つのchronydへ接続し、単体・二台の同期とLAN応答を確認済み。共有hostでは時計補正を無効にした。全源停止/長時間品質、独立kernelのappliance時計補正は残る。 |
| `TailscaleNode` (1) | `service.tailscale` | 保存設定の私設IPv4の3範囲と文書用両LANを広告し、exit未選択の独立Headscale/Tailscale peerからBGP・静的経路・旧LAN・管理へTCP/UDPを確認済み。subnet/exit、LAN・peer双方向通信、両IP Internet、保持identity、単体再起動と両方向HA引継ぎも通過。実サイトのcontrol/identityとpublic bootstrapは別に受け入れる。 |
| `LogSink` (2), `Telemetry` (1), `LogRetention` (1), `TrafficFlowLog` (1), `WebConsole` (1) | `service.otlp_logs`/`syslog`、retention設定、`service.traffic_flow`、`service.web_console` | 実syslog/公式Collectorへ新しいlogs/metrics/tracesを送信し、native nDPI計数/SNIと履歴を確認済み。単体・二台でarchive失敗時の保持/通信と復旧後の送信/期限削除を確認。7日実経過・lossless deliveryは残る。 |
| `ManagementAccess` (1) | explicit listener/firewall設定とconfirmed commitによる管理経路維持 | 明示listener/firewallとconfirmed commitへ接続し、管理SSH鍵認証・保持host key、Web履歴、LANからの拒否、期限切れ/control再起動時の元intentと通信への復帰を確認済み。実サイトの管理経路・物理機器は別の受入。 |
| `Package` (1) | native `service.vrrp.conntrack_sync` | 保存設定のconntrackd依存は固定版のbundled engine/source/licenseへ収録する。VRRP workerがprivate chrootのconntrackd FTFWを所有し、commit完了後にVIPを公開する。実HAの既存socket/NAT tupleを確認済み。FTFW transportの認証・全partition条件は残る。 |
| `Sysctl` (1) | `forwarding.conntrack_tcp_liberal` | 実設定は`net.netfilter.nf_conntrack_tcp_be_liberal=1`。typed fieldへ対応し、設定journalとboot再適用で維持する。single profileへ追加し、実HGW gateでrouter namespaceの実kernel値1を確認した。 |

## 抽出した依存関係の形

両設定から同じ依存種別を抽出した。resource identityと設定値は記載しない。

- BGP peerはBGP routerを参照する。DHCPv4 lease syncはserverを、DHCPv6 prefix syncはPD clientを、DHCPv6 server syncはDHCPv6 serverを参照する。
- DHCPv6 delegated addressはPD clientに依存する。PDと学習情報はLAN RA、DHCPv6 information、DNS upstream/bootstrap、DS-Lite source、NTP sourceへ渡る。
- DS-Lite tunnelはdelegated IPv6とローカルIPv4 addressに依存する。Egress候補はWAN/tunnel deviceとHealth条件を参照し、Firewall zoneにも依存する。
- DHCPv4 gateway/DNSとNAT source addressはvirtual addressを参照する。Local redirectはaddress setを参照する。DNS listener/recordはinterface、static、delegated、virtual addressを参照する。
- legacy参照名の一部は複数のresource kindで重複している。参照はkindとownerを含めて照合し、保存値の解決とnative設定の接続を機能単位で進める。

## 隔離環境で実行する前に置き換える値

例のinterface名、アドレス、domainは文書用である。再現可能な隔離試験では、test link名、WAN peer/AFTR動作、LAN prefixとlease pool、安定したDHCP client/server DUID、VRRP source/VIP/peer、TLS certificate pathと証明書、DNS条件付きupstream、実測したMTU/MSS policyを明示的に置き換える。例の証明書・秘密鍵は存在せず、pathはschema値だけである。文書用アドレスを実LANへ接続しない。

## 検証の範囲

単体45-resource・二台各54-resourceの実daemon/kernel、実HGWと独立端末/peerによる通信・復旧を実行済み。実行版ごとの範囲は[試験計画](test-plan.md)と[引き継ぎ](pve-codex-handoff.md)を参照する。単体の自然PD更新と二台の既存socket/新規端末の受入も分けて記録する。全110-resource意図、全34 work package、対象ONU/NIC・物理二台・性能と全配布先の完成を継続する。
