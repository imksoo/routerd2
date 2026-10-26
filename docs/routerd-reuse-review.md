# routerd 実装の再利用と置換の見直し

2026-10-26。一次照合中。下表は実装の由来であり、機能の受入完了表ではない。
対象は固定依存 `1329ef54f296507eda79bbbc3b853783dd27251e` と、現場の記録を持つ
ローカル routerd checkout `a0b1b3ba68f1a0210f3e9c4b6cb0483800a2a22c`。

既存の相互接続実績を、新しい保存形式・worker構成・native化の都合で落とさない。
既存engineの再利用を先に検討し、必要な変更は実際のpeer・clientで確認する。
旧routerdがdnsmasq・keepalived・FRR等に委ねていた動作も、単なる設定生成とは扱わない。
外部engineを配布runtimeへ含める案を、native実装と比較する。

## 現在の土台

| モジュール | 実際の土台・置換 | 見直す対象 |
| --- | --- | --- |
| DNS | `pkg/dnsresolver`、`pkg/logstore`と旧daemonを利用・改変 | native化したlease期限、更新、listener、保存の差分。再利用しているだけで全resource同等とは数えない |
| BGP | 旧`bgpdaemon`の設定・処理とGoBGPを利用・改変 | 補助状態/履歴保存失敗でGoBGPを停止する結合を除去し、実filesystem失敗下のsession/FIB更新・撤去を確認。FIB ACK、BFD連動、VRF、全世代RIB・上限。旧FRR連携の効果を別途照合する |
| Health | 旧`Probe`のTCP/DNS/HTTP、旧`ApplyResult`を利用。ICMP transportは新規 | ICMP実機、source/mark、threshold、旧状態・条件との違い |
| DHCPv4 client | 旧socket実装を移植し、旧wire/lease helperを利用。orchestrationは新規 | socketを再利用しても取得・再送・renew・restoreが同等とは限らない。実回線取得とLAN NAT通信は確認した。独立dnsmasqでsubnet mask要求漏れを発見し、旧encoderを土台に修正。/32・明示gateway routeでTCP/UDPも確認 |
| DHCPv6 client | 旧`pdclient` wrapperから、新規のIA_NA/IA_PD/information engineへ置換 | 受信・link-local待機の退行、リンクローカルDNS、無関係IA・不良optionで全体を捨てる処理を修正。旧codec/state machine、実Renew/Rebind/Release、複数割当の扱いを再照合する |
| RA受信 | 旧netplan/systemd側の動作をnative clientへ置換 | DAD待機と不良DNS optionの局所破棄を修正。managed/required/observe-only、DHCP起動、MTU/hop-limit、resolver効果 |
| RA送信 | 旧dnsmasq側の処理を新規raw ICMPv6 engineへ置換 | link/MAC変化、RS応答、prefix/DNS期限、withdrawal。MAC変更時の誤広告を今回修正 |
| DHCP server | 旧API型と変換を利用。旧dnsmasq engineの再利用ではない | Rapid Commit要求への通常OFFERはScapyとの実通信で修正確認。割当・再送・option・sticky・複数scope・DNS・journalを旧実運用と比較。dnsmasq採用も再評価する |
| DHCP relay | 旧dnsmasqの動作を新規relayへ置換 | giaddr/option/fragment/VRF/再構成・HA。独立server試験を全機器の受入と扱わない |
| DS-Lite/AFTR | 旧controller/host処理を新規resolverと通常のip6tnl所有権へ置換 | SelfAddressPolicy・DNS選択・endpoint維持。実回線の定期RA更新によるtunnel再生成を修正し、同じTCPで100秒継続を確認。PD由来source・完全な選択は未完了 |
| VRRP/HA | 旧keepalivedの選挙・VIP管理をnative VRRPとwriterへ置換 | keepalived 2.2.8との通常DAD起動・VIP TCP/UDP・選挙/停止/復旧を確認。NoDADが隠したDAD中のworker再起動を修正。全tracking/HA session継続とchecksum差分等は未完了 |
| BFD | 旧FRR/bfddの処理をnative BFDへ置換 | 独立FRR 8.4.4で通常DAD・/32と/128・実BGP/FIB/LAN TCP/UDP・障害/期限/再起動後復旧を確認。不要なsubnet/MAC/受信port制約を削除。旧profile/reset policy、認証、全VRF/規模の受入は未完了 |
| link/address/route | 旧OS renderer/applyを新規netlink writerへ置換 | option・source選択・所有権・bridge/VRF再構成。wireの実績とは別にOS設定の意味を照合する |
| firewall/PBR/NAT | 旧rendererを新規nftables writerへ置換 | ICMPv6・DHCP等の許可、条件、順序、NAT、conntrack。DS-Lite生成前の不要なinterface存在確認は削除した |
| WireGuard | kernel/WireGuard APIを利用。旧CLI/rendererから新規writerへ置換 | peer/source/secret/route/lifecycleの差分と実peer |
| config/control | 旧API/controllerからsource programとcoordinatorへ再構成 | resource条件・所有権・状態・適用順・rollback。protocol移植の退行を新構成の必然として扱わない |
| system/更新/storage | PID 1、supervisor、署名runtime選択を新規実装 | 起動・停止・回復の順序、kernel/rootfs更新、停電、対象実機。旧OSの成熟した機能を失った範囲を確認する |
| management | 新規SSH/CLI/control境界と既存Goライブラリ | 旧操作・認証・権限・運用契約。組み込みSSHがあるだけでは同等ではない |
| observability/events | 新規log/event永続化とrule処理。DNS部分では旧logstore等を利用 | 旧OTel・ログ・event/通知・運用操作との欠落と差分 |
| PPPoE/IPsec/Tailscale | 完成したnative置換はない | 旧engineと配布componentの再利用から実装方針を見直す |
| hybrid/mobility/federation | 旧機能範囲の完成した置換はない | 既存provider/engine/運用契約を土台にする。未実装を新設計で置換済みとは扱わない |
| time | protocol期限のBOOTTIME化はNTP等の実装ではない | 旧時刻同期機能、clock補正・suspend・rebootと運用契約 |

## 実接続で判明した退行

- 旧DHCPv6 daemonは受信元UDPポートを限定していなかった。routerd2は移植時に547限定を
  加えており、実上流から49153で届くAdvertise/Replyを捨てていた。制約を削除して/60取得を確認した。
- 旧DHCPv6にはlink-local readiness待機があった。RA/DHCPv6でDAD完了を待つようにし、
  通常DAD・保存状態なしの実接続で、全関連workerが第1世代で起動することを確認した。
- RA送信が起動時MACを保持し、bridge参加portの追加後も古いSLLAを広告していた。
  送信時のMACで広告するようにし、同じ実上流・LAN clientでIPv6通信の復旧を確認した。

実回線のLAN IPv4/IPv6 HTTPS・UDP DNSと単発の実AFTR転送は確認した。
DS-Liteでは通常RAの期限更新とDNS RRset順序変更でトンネルを壊す経路を修正した。
最終バイナリーの実AFTR経由で同じTCP接続のHTTPS 100回・UDP DNSが100秒継続し、
110秒の監視でtunnel indexは不変だった。その後、自然なT1=7200秒のRenew/Replyと
PD期限更新を実回線captureで確認し、同じTCP接続・UDP DNSが100分継続した。
更新後のLAN IPv4/IPv6 HTTPSも通った。旧PD由来source、実回線Rebind/失効/Release、
その他の置換による不必要な受信拒否は引き続き見直す。
全モジュールの実機受入・旧動作との差分確認は未完了である。

受信処理の見直し後、dnsmasqとの通常DAD・リンクローカルDNSの実問い合わせ・
IA_NA更新/失効/復旧、悪いDNS optionを含むRAからの経路/TCP/UDP、
ScapyのRapid Commit要求から通常OFFER/REQUEST/ACKまでを確認した。
DHCPv4 off-subnet gateway・subnet mask要求漏れ・BGP補助保存と生存の結合は修正し、
独立dnsmasq・実GoBGP worker/独立wire peer・kernelで確認した。部分Renew/Rebindは元のexchange/T2と未更新IAの期限を保持するよう修正した。

ISC Kea 2.4.1との同時NA/PD取得、WAN/LAN TCP/UDP、リンクローカルDNSを実通信した。
KeaのReplyからPDだけを省略しても、元T2/Rebind・PD失効とNA通信継続を確認した。
完全なReplyを戻すとLANも復旧した。省略された旧prefixの期限を勝手に変更する処理を削除し、
WANリンクローカルDNSを別LANへ広告しないようにした。全体の機能受入完了ではない。

DHCPv4の任意option全拒否も削除し、旧`LeaseFromACK`の完結した要素・規定timer処理を使う。
不良optionを含むACKは局所確認、独立dnsmasqは通常の/32とTCP/UDP確認までである。
raw timer指定をpeer自身が正規化したため、不良timerの実socket受信を確認したとは数えない。

BFDも独立FRRとの初回接続で、直結host routeがある/32・/128をsubnet判定で拒否し、
通常DAD中にworkerを再起動する問題が判明した。DADだけを起動contextで待ち、
不要なprefix/MAC保持と受信元port制約を削除した。FRRの実payloadのportだけを
3784へ変更した場合も両系統UPを維持した。独立FRR bgpdとの経路学習・LAN TCP/UDP、
片系BFD障害・元期限切れ・worker交替と復旧まで確認した。既存fixtureの合格とは別の結果である。

VRRPの実keepalived fixtureからelection sourceのNoDADを外すと、IPv6 unicast/
multicastとも初回起動が第6世代まで再起動していた。通常DADを起動contextで待つよう直し、
第1世代起動と両ownerのVIP TCP/UDP・切替・復旧を確認した。追加WAN identity試験の
通信直前diagnosticがSIGSTOP中のcontrolへ同期問い合わせしていたため、その不要処理も削除した。
550msのwriter停止では、同時check中に元期限内の適用に失敗した実行があり、単独では通った。
この期限内性能と、過去の初回TCP timeout等は未解決として残す。
