# homert設定のnative対応表

## 対象と範囲

保存済みのhomert02/03設定を読み取り専用で解析し、resource種別と参照関係の形だけを確認した。各設定は109 resourceで、種別ごとの件数と抽出できた型付き依存関係は一致した。設定値、アドレス、identity、credential、鍵はこのリポジトリへ保存していない。

[homert02の例](../examples/homert02-native.yaml)と[homert03の例](../examples/homert03-native.yaml)は、文書用アドレスを使ったResourceProgramの例である。DHCP、DNS、PD、RA、DS-Liteと、VRRP/lease replicationの相方設定を含む。保存設定の変換結果ではなく、配備可能な設定でも、HA・クライアント相互接続の合格証跡でもない。旧routerdの[`DHCPv6ServerLeaseSync`契約](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/docs/how-to/dhcp-lease-sync.md)はstateful DHCPv6 address poolのlease state同期用である。保存設定のLAN DHCPv6 serverはstatelessでpoolを持たないため、この同期機能は現行homertの単体・HA動作に必要なstateを表していない。

## 機能対応

件数は両方の保存設定から読み取ったresource種別の数である。対応欄は現在のResourceProgramで表現する場合の候補であり、自動または一対一の変換を意味しない。

| 保存設定のresource種別（件数） | routerd2のnative表現 | 状態・未確定事項 |
| --- | --- | --- |
| `Interface` (10), `IPv4StaticAddress` (7), `IPv4Route` (4) | `interface`、`address`、`route`、`host_route`、`egress_route` | 基本的な意図は表現可能。route種別、所有者、source、interfaceの実値は保存していない。 |
| `IPv6DelegatedAddress` (5), `VirtualAddress` (2) | `service.dhcp6.delegations`、`service.vrrp` のaddressとVIP interface | 例ではPD由来のLAN addressを物理LAN linkに、IPv4 VIPをVRRP所有interfaceに置く。実際のactive/standbyでの所有関係は実peerで確認が必要。 |
| `DHCPv4Server` (1), `DHCPv4Reservation` (2), `DHCPv4ServerLeaseSync` (1) | `service.dhcp_server` のscope/reservationと `ha` TLS replication | 旧routerdの[DHCPv4 renderer](https://github.com/imksoo/routerd/blob/bf123f93af59/pkg/lan/dhcpv4server/dnsmasq.go)と現行workerを確認した。単体例には通常pool、MAC/OUI別pool、2件の予約を文書用値で記述した。例では待機側でもDHCP workerを動かし、`ha.owner` が配布役を決める。実profileのlease transferとclient renewは未確認。 |
| `DHCPv6PrefixDelegation` (1), `DHCPv6PrefixDelegationLeaseSync` (1), `DHCPv6Information` (1) | `service.dhcp6`、`dhcp6.ha`、delegation、RA client factと各consumer | native PD state replicationと学習情報を表現できる。保存済みDUID/IAIDとpeer動作は転記・受入していない。 |
| `DHCPv6Server` (1), `DHCPv6ServerLeaseSync` (1), `IPv6RouterAdvertisement` (1) | `service.dhcp6_server`、`service.ra` | 保存設定はpoolのないstateless DHCPv6である。旧routerdのlease-syncはstateful address pool用なので、このprofileでは同期すべきserver lease stateがない。native server DUIDは各nodeで永続化され、対になるサービスでは同じ明示DUIDを使える。WAN DHCPv6 clientのDUID/IAIDとPD lease同期は別の機能である。 |
| `DNSResolver` (2), `DNSForwarder` (10), `DNSUpstream` (13), `DNSZone` (1) | `service.dns` のlistener、suffix source、upstream、zone、DHCP由来record | 条件付きforwardとDoHはnative例がある。listener addressの依存関係とsourceごとの詳細は、保存値を使って見直すまで同等とは扱わない。 |
| `DSLiteTunnel` (4), `EgressRoutePolicy` (3), `HealthCheck` (6) | `service.ds_lite`、`egress_policy`/`egress_route`、`service.health` | 機能種別を表すnative typeはある。候補、tunnel依存、選択条件、MTU、復旧動作は保存値から再構成していない。 |
| `NAT44Rule` (7), `NAT44SessionSync` (1) | `nat` resource、`service.vrrp.conntrack_sync` | 対応するnative機構はある。実際のsource/egress matchと同期除外値は未確認で、packet試験も必要。 |
| `FirewallZone` (3), `FirewallPolicy` (1), `FirewallRule` (2), `IPAddressSet` (2), `LocalServiceRedirect` (1), `FirewallEventLog` (1) | `firewall` rule、address set、local redirect、`service.firewall_log` | native primitiveはあるが、zone/policyとの一対一対応は確立していない。推測したrule setで置き換えない。 |
| `BGPRouter` (1), `BGPPeer` (1) | `service.bgp` | 機能種別を表すnative serviceはある。router/peer値と実route交換は別途確認が必要。 |
| `NTPClient` (1), `NTPServer` (1) | `service.ntp` のsource、listener、access prefix | native time serviceはclient/server意図を持つ。実際のsource選択とserving ACLは転記していない。 |
| `TailscaleNode` (1) | `service.tailscale` | native serviceはある。identityとenrollment stateは別に配備する。identity値はこの解析に取り込んでいない。 |
| `LogSink` (2), `Telemetry` (1), `LogRetention` (1), `TrafficFlowLog` (1), `WebConsole` (1) | `service.otlp_logs`/`syslog`、retention設定、`service.traffic_flow`、`service.web_console` | 対応するtyped service領域はある。sinkからtelemetryへの参照とretention policyは実値での対応が未確認。 |
| `ManagementAccess` (1) | 対応するResourceProgram payloadを特定できていない | 未対応。Web consoleから推測せず、management/startup契約で明示する。 |
| `Package` (1) | ResourceProgram外のimage/build package manifest | runtime resourceには未対応。必要packageをimage側で確認し、任意package実行resourceは追加しない。 |
| `Sysctl` (1) | 意味が一致する場合に限り、typed `forwarding` field | keyと設定値を確認するまで未対応。任意sysctlを設定する汎用resourceはない。 |

## 抽出した依存関係の形

両設定から同じ依存種別を抽出した。resource identityと設定値は記載しない。

- BGP peerはBGP routerを参照する。DHCPv4 lease syncはserverを、DHCPv6 prefix syncはPD clientを、DHCPv6 server syncはDHCPv6 serverを参照する。
- DHCPv6 delegated addressはPD clientに依存する。PDと学習情報はLAN RA、DHCPv6 information、DNS upstream/bootstrap、DS-Lite source、NTP sourceへ渡る。
- DS-Lite tunnelはdelegated IPv6とローカルIPv4 addressに依存する。Egress候補はWAN/tunnel deviceとHealth条件を参照し、Firewall zoneにも依存する。
- DHCPv4 gateway/DNSとNAT source addressはvirtual addressを参照する。Local redirectはaddress setを参照する。DNS listener/recordはinterface、static、delegated、virtual addressを参照する。
- legacy参照名の一部は複数のresource kindで重複している。今回の解析ではscalar値を保持していないため、その参照先は曖昧なままであり、native profileを完成させる前に解決が必要。

## 隔離環境で実行する前に置き換える値

例のinterface名、アドレス、domainは文書用である。再現可能な隔離試験では、test link名、WAN peer/AFTR動作、LAN prefixとlease pool、安定したDHCP client/server DUID、VRRP source/VIP/peer、TLS certificate pathと証明書、DNS条件付きupstream、実測したMTU/MSS policyを明示的に置き換える。例の証明書・秘密鍵は存在せず、pathはschema値だけである。文書用アドレスを実LANへ接続しない。

## 検証の範囲

両例は `routerd config validate` で受理された。これはResourceProgramのdecodeとcompile-time契約の確認だけである。worker起動、物理WAN/ONU、DHCP交換、端末DNS/Internet接続、HA failoverは確認していない。profileを利用可能と扱う前に、実routerd2 process、kernel、独立client/peer、隔離実回線fixtureで確認する必要がある。
