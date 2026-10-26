# routerd2 開発引き継ぎ

2026-10-26。全体ゴールは継続中。対象実機で利用可能とは判断していない。
機能範囲は [implementation-plan.md](implementation-plan.md) と
[implementation-plan.json](implementation-plan.json) を参照する。

[AGENTS.md](../AGENTS.md) とユーザー指示に従い、未公開API・保存形式・過去の試験結果を
維持するための実装は残さない。過去の `docs/evidence` は削除済み。
現在の設計に合わせてfixtureを更新し、prototype reader・adapter・migrationは作らない。

## 現在の実装

- PPPoE は旧 `LinuxPeer` の設定生成と pppd を土台にし、実 supervisor/control/network/worker へ統合した。
  pppd が PPP unit/address、writer が有限の経路を所有する。child の mount namespace に
  `/etc/ppp`・`/run` と root user option を閉じ込め、ホストの hook や設定を使わない。
  独立 RP-PPPoE 3.15 / pppd 2.4.9 との PAP/CHAP、DNS、MTU 1454、LAN TCP/UDP、
  Alpine 3.24.3 / pppd 2.5.3-r0 でも同じ経路を確認した。標準 plugin 名と一つの private DNS file を使う。
  IPv6CP link-local、writer の原期限撤去、worker 強制終了、古い通知、認証拒否/修復、
  AC 停止/復旧が通った。再実行は `make check-pppoe`。
  ローカル結果は `dist/pppoe-host-current/` と `dist/pppoe-alpine-current/` の log/pcap。
  `make check` と、最終変更の関連 race・全 vet・amd64/arm64 の静的 build が通った。
  scratch/appliance に engine を同梱したものではない。配布 component・PPP 上の PD・
  resolver 適用・二経路 PBR/MSS・実 ISP は未完了。[ADR 0002](adr/0002-pppoe-engine.md) を参照。

- controlは読み取り専用のsource宣言を所有し、一度compileしたplanへ有限のfactを渡す。
  一時FactStoreや仮producer登録は使わない。operator revisionと観測・投影世代を区別する。
- 一つのcoordinatorがconfig適用・通常更新・worker起動停止待ちのkernel適用とACKを所有する。
  I/O中も観測を受け取り、古い結果で新しい通知を完了扱いにしない。
  受理済み観測をkernel失敗で巻き戻さず、最新の観測を再適用する。
- supervisorの実登録とimmutable instance identityで継続・退役を判定する。
  通常監視は停止要求後に戻り、次回に実終了を確認する。config切替だけが期限付きで待つ。
  退役完了後に新設定を適用し、継続workerの現在の準備失敗が修復設定を妨げないようにする。
  I/O中に前提が変わったworkerは起動せず再評価する。
- instanceファイルは準備した同じbytesをhashし保存する。継続判定用identityは設定切替ごとに
  観測mutexの外で一度生成し、停止待ち前後は最新のmatching recordを選ぶ。
- VRRPは元の選挙観測を直接factへ写し、成功したkernel適用の選挙keyを保持する。
  grant交換・仮VIP・受信時刻から作る別lease・期限切れJSON書換えはない。
  実際にVIPを所有したかを同じ要求の適用ACKで返す。
- gracefulはkernel適用開始時の入場条件である。成功後の準備喪失だけでは撤去しない。
  条件除外・role喪失・producer交替・元のboot期限では失効する。GARP/NAは適用ACKと
  実netlink検査の後に送る。準備待ちでも選挙広告は続ける。
- IPv4/IPv6 v3 unicast、IPv4 v2と任意PASS、宣言resourceのHealth/BFD/BGP trackingに対応する。
  unicastはkernelのrouting/ARP/NDと実IP metadataを使い、仮Ethernet headerを作らない。
  Health penalty・連続probe回数・max_ageは元の観測で判定し、poll/replayで回数を増やさない。
  監視の空白・producer交替後はfreshなprobeを要求し、worker自身のboot clockでも失効させる。
- 追加WAN/LAN MACは既存の条件付きlink/interface/addressで表す。
  `.elected`で準備し、`.active`でRA等を公開する。finite MACは期限内だけRARPで所在を通知する。
  別の選挙managerや永続authorityは作らない。
- 条件・依存先の準備期限はlinkだけでなくaddress／routeにも渡す。物理interface上でも
  writerが元の期限で管理IP・routeを撤去する。owned linkはindex・marker・属性を確認してDOWNにする。
  addressは条件と自身の短いlifetimeを使い、kernelの秒単位timerも設定する。
  明示したIPv6 routeのUTC期限はkernel timerとして残し、条件のBOOTTIME期限も適用する。
- controlとwriterは同じ純粋なlifetime投影を使い、意図した期限切れを適用待ちとして残さない。
  宣言自身のvalid期限も依存先へ渡し、期限切れは`lifetime_expired`として後続を停止する。
  UTC lifetimeがあるsourceも動的に評価し、設定切替前は時計だけを持つ空のfact集合で準備する。
  Ready/confirmed commitはACK済みlink/address/routeの元の期限を確認する。
  公開ResourceStateへ投影中の期限を混ぜず、準備時刻の更新で状態比較が永久に不一致になる原因を除く。
  authority更新もprojection generationを進めるが、operator revisionは変えない。
- BGP route期限は元の観測、ACK済みBFD受信期限、再帰next-hopのaddress・route・link期限の短い方。
  gatewayを展開しても前提期限を失わない。ECMPも一つの経路期限をwriterへ渡す。
  期限だけの更新でkernel routeを置換せず、routing table観測を操作内で共有する。
  route追加予定と元の期限を追加前にまとめて永続化する。撤去は通常削除と所有権確認を共有する。
- 受理済みprotocol通知は元のbodyを保持し、失効時にActiveや経路・leaseをJSONで書き換えない。
  BGPのFIB・状態表示は元のBOOTTIME期限を使い、BGPとDHCPv4の受信側重複leaseを廃止した。
  DHCP serverのDNS出力・active条件は利用時に失効を判定する。設定切替後も、同じ通知を
  大きい配送sequenceで再送してauthorityを復活させない。
  DHCP serverは通知前のtable walkからBOOTTIMEを固定し、その元の期限をDNSのA/PTR応答まで渡す。
  controlは受理時に一度decode・検証した型付き観測を条件・DNS・状態表示で使う。
  control／DNSの受信側期限表は廃止し、遅いACKの後も同じ期限を確認してからDHCP応答を返す。
  DNS内部の正規化も型付きsliceの複製と整列へ変更した。
  client個別の割当・journal保持・lease期限にはUTC依存が残る。suspendを含む受入は未完了である。
- BFD→BGPのHTTP受信leaseと失敗counterを廃止した。元のpacket期限とproducer観測期限の
  短い方をsessionとFIBへ渡す。失敗したpollでは元のauthorityを上書きせず、配送が止まれば
  元の期限で失効する。期限を跨いで届いた更新も、先に古いbudgetの失効を評価する。
  実際のDown／source交替／期限切れの後は、TCP Down/Idle・該当撤去のACK・新しい受信を要求する。
- trackingは元の型付きstatusをderived assertionと共有する。宣言IDで参照を解決し、
  現在のworker集合に含まれないsourceも設定から失わない。Healthは実probe回数、BFD/BGPは
  別の元観測を数える。BFD continuity・producer交替・期限切れ後はfreshな確認を要求する。
  tracked resource自身の明示的when=falseは、条件の元の期限内だけneutralとして回数とpenaltyを保持する。
  他のresource種別とDisabled/NotApplicable/Standbyのphase契約は未実装。
- kernel適用開始時の要求を固定し、その要求だけを完了する。I/O中の別通知で適用済み要求の
  ACKを保留せず、後から届いた要求を先に完了しない。BGP statusは自身のmatching適用identityで
  判断し、無関係なkernel I/Oで不明へ戻さない。
- Health監視と履歴配送は分離する。保存先障害・queue満杯でもprobeを止めず、欠落と保存エラーを表示する。
  全probeの無欠落保存は保証しない。

現在のworker ABIは3、compiler contractは `resource-program-native-17`、network journalはformat 7。
`track.resource`は宣言IDであり、decisionはhealthy/unhealthy/neutral/unknownを明示する。
破棄したservice-only形式のreader・混在運用・migrationは実装しない。

## 今回の実装と実回線での確認

nwadmin03の既存uplinkへ、専用MAC/DUIDのmacvlanを隔離namespace内で接続した。
ホストの管理address・route・clockは変更していない。上流機器の型番・ONU直結の配線は未確認。
実上流のRA、DHCPv4、DHCPv6 PD /60を製品workerで受信した。
DHCPv6 Advertise/ReplyはUDP 49153から届き、547限定の独自フィルターが取得を妨げていた。
旧routerdと同じくポート限定を削除し、transaction/client/serverの照合は維持した。

通常のlink-local DAD完了を待つ処理をRA受信・RA送信・DHCPv6で共有した。
保存状態なし・LAN配線済みのcold startで全関連workerが第1世代のままReadyとなった。
RA送信はbridge port変更後の現在のMACを広告する。起動時MACの保持でLAN IPv6が
切れていた問題を修正し、別LAN namespaceのkernel SLAAC端末からIPv4 NAT・委譲IPv6の
HTTPSとUDP DNSの実Internet通信を確認した。

既存routerdの設定で用いるAFTR名を実上流DNSで解決し、実AFTR経由のLAN HTTPS・UDP DNSと
IPv4-in-IPv6の往復もcaptureした。通常RA更新の前後でDS-Lite linkが再生成され、
同一TCP接続が数秒で切断する不具合を再現・修正した。有効期限だけの更新は問い合わせ世代と
再試行時刻を変えず、投影時に現在の期限を交差させる。DNS RRsetの順序変更でも現在のAFTRが
新しい応答に残っていれば維持する。古い問い合わせの結果はsequenceで除外する。
最終3バイナリー（`ntt-aftr`）の実回線再接続で、同じTCP source portのHTTPS 100回と
UDP DNSが100秒継続し、110秒の監視でtunnel indexも不変だった。RA・DNS再問い合わせ・
IPv4-in-IPv6往復をcaptureで確認した。IPv6 LAN HTTPS/UDP DNSも成功した。
独立RA/DNS/Linux AFTRとの元期限での撤去・再取得・worker交替も既存の実daemon試験が通った。
全実装計画・実機受入は未完了であり、製品全体の利用可能判定には使わない。
PDのT1は7200秒であり、今回の実回線確認は取得まで。実回線Renew/Rebind/期限切れ/Releaseは未確認。
実装の再利用・新規置換と残る比較対象は[routerd-reuse-review.md](routerd-reuse-review.md)を参照。
一次ソース照合を全モジュールの機能受入完了とは数えない。

最終AFTR修正のsourceで`make check`（Go/Python 33件・Race Detector・vet）、
amd64/arm64の6静的ELF buildは成功した。直前のuplink/DAD修正では署名bundle検査も成功した。
ローカル出力は`dist/ntt-aftr-check-build.log`、`dist/ntt-aftr-final-related.log`、
`dist/live-aftr-final-results.log`、`dist/live-aftr-final-link.log`、
`dist/ntt-aftr-ra/`、`dist/ntt-aftr-daemon/`。修正前の失敗は`dist/live-dslite-continuity.log`。
過去の試験結果を維持する互換コードは残さない。

## 受信制約の見直し

旧routerdの`pdclient`と現行のsocket→decode→accept→投影を照合した。
DHCPv6 decoderへ既知のIAIDを渡し、無関係・不良のIAだけを除外する。
DNS/SNTPのリンクローカルaddressを許可し、不良の独立optionは有効な割当を捨てず除外する。
RAの不良RDNSS/DNSSLはoption単位で破棄する（RFC 8106 §5.3.1）。
Rapid Commit付きDHCPDISCOVERも通常OFFERで処理する（RFC 4039 §3）。
以前の全拒否を正解にするfixtureは削除・更新した。

製品バイナリー`receive-tolerance`で、独立dnsmasqのIA_NA・リンクローカルDNS取得と
そのscopeでの実DNS問い合わせ、通常DAD、TCP/UDP、worker交替、Renew/Rebind/失効/復旧が通った。
`python3 scripts/daemon-tests.py dhcp6 --test TestDaemonDHCP6AddressIndependentServer`で再実行できる。
不良DNS optionを併送した既存の実RA/経路/TCP/UDP/期限切れ試験も通った（`make check-ra-client`）。
隔離LANのScapy 2.8.0からoption 80長さ0のDiscoverを送信し、修正前の無応答をcaptureで確認、
修正後は通常OFFER→REQUEST→ACKを確認した。Scapyのbroadcast返信照合設定を修正して
最後まで完了した出力は`dist/rapid-commit-final-peer.log`。BusyBoxの空option指定はoptionを送らず、
Rapid Commitの検証には使えなかった。RA/DHCPの出力は`dist/receive-ra/`、`dist/receive-dhcp6/`。
`make check`・Race Detector・vet・6静的ELF buildも成功した。
Renew/Rebindの部分Replyは有効なIAだけを更新し、未回答IAのexchangeと元T2を保持するよう修正した。

## 経路と補助保存の結合の修正

BGPの状態表示・履歴への書込み失敗は、その操作でログへ報告し、GoBGPと経路所有通知を止めない。
設定の適用journalやkernel ACKの失敗と、補助保存の失敗を同じ停止理由にしない。
実際の保存先をdirectoryにした製品workerでBGP session、IPv4/IPv6 FIB、更新・撤去が通った。
writer再起動後の元期限での撤去と、再帰先の静的IPv6経路の元期限による撤去も通った。
後者はsupervisor/control/BGP停止中でも、BGP観測の期限より先に撤去した。
`make check-bgp`で既存の2つの実daemon試験を実行できる。
address/link由来の再帰前提期限は別であり、その実daemon受入はまだ残る。

DHCPv4はサブネット外という理由だけで投影全体を拒否しない。
明示したgatewayの直結routeを含む通常のkernel適用を使う。架空のonlink routeは加えない。
独立dnsmasqとの最初の接続では、必須のsubnet maskを旧request encoderが要求しておらず、
custom maskを含まないACKを捨て続けた。旧encoderを土台に送信箇所へ必要なfieldをまとめ、
subnet maskを要求するよう修正した（出典とlicenseは`internal/protocol/dhcp4/NOTICE`）。
同じ相手で/32取得、gateway host route、default route、TCP HTTP 200とUDP echoが通った。
製品版`route-continuity`、出力は`dist/dhcp4-offsubnet-final-peer.log`と`dist/daemon-bgp/`。
最終sourceの`make check`・Race Detector・vet・6静的ELF buildは成功した
（`dist/route-continuity-final-check-build.log`）。

## 独立Keaとの同時NA/PDと部分更新

ISC Kea 2.4.1を隔離namespaceで動かし、製品`dhcp6-partial`のIA_NA/IA_PD同時取得、
WAN source付きTCP/UDP、委譲LANのkernel SLAAC/TCP/UDP、scope付きリンクローカルDNSを確認した。
peerは製品writerによるlink UPと通常DADの完了後に起動する。NoDADやhost NIC変更は使わない。
Keaの実ReplyからIA_PDだけを除いたpacketをScapyで配送し、元の完全なpacketだけを隔離INPUTで落とした。
NAを更新しても元T2でRebindへ進み、未更新PDの元期限は不変だった。
PDは元の期限で失効し、その間も更新済みNAのTCP/UDPは継続した。
完全なKea Replyを再び配送すると、委譲LANのTCP/UDPが復旧した。
WAN workerは第1世代のまま、LAN RAはPD失効で停止し再取得で第2世代となった。
LAN captureのRDNSSは明示global serverだけで、WANから学んだリンクローカルDNSを別LANへ転送しない。

Replyから省略された旧prefixのpreferred/valid期限は元のまま残す。省略を明示deprecationに変えない。
`make check-pd-ra`の既存実daemon試験も、元のpreferred期限での旧prefix非推奨化・新prefix通信が通った。
Keaのローカル出力は`dist/kea-partial-peer-4.log`、private setup/captureの場所は
`dist/kea-partial-location.json`。最終3バイナリーのhashは配備先とbuild出力で一致した。
`make check`・全体Race Detector・vet・6静的ELF build、追加の小さな部分Reply再発防止試験と
関連Race Detectorも通った（`dist/dhcp6-partial-check-build.log`、`dist/dhcp6-partial-regression.log`）。
実回線では元T1=7200秒の自然なRenewと上流source 49153からのReplyをcaptureし、
PD期限の更新を確認した。同じTCP接続（LAN側SYNは1組）とUDP DNSが100分継続した。
更新後のLAN IPv4/IPv6 HTTPSも301、PD workerはgeneration 1、tunnel indexは不変だった。
出力`dist/live-long-continuity.log`、更新snapshot`dist/live-after-natural-renew-services.json`。
private capture場所は`dist/live-uplink-location.json`。所有者が製品・capture・隔離namespaceを停止済み。
停止captureにReleaseはなく、実回線Rebind/期限切れ/Releaseは受入済みとしない。
実行中の回線バイナリーは`ntt-aftr`であり、以降の修正まで実回線受入済みとはしない。

DHCPv4受信も旧`LeaseFromACK`の実装と照合し、任意T1/T2・MTU・broadcastの不良だけで
message全体を捨てる制約と、DNS/NTP/router listの端数だけで割当を捨てる制約を削除した。
旧helperが完結したaddress要素を使い、使えない任意timerを規定値へ戻す経路を使う。
必須identity・subnet mask・lease期間・packet境界の検査は残す。局所的なACK試験は通った。
独立dnsmasqとの/32・明示gateway・TCP/UDPも最終`dhcp4-options`で通った。
ただしdnsmasqはraw T1指定を通常の4-byte値へ正規化したため、その実行を不良timer受信の
相互接続試験とは数えない。出力は`dist/dhcp4-optional-local.log`、`dist/dhcp4-optional-peer.log`。
`make check`・全体Race Detector・vet・6静的ELF buildも成功した
（`dist/dhcp4-optional-check-build.log`）。

## 独立FRRとのBFD/BGP相互接続

製品`bfd-frr`とFRR 8.4.4の`zebra`/`bfdd`を別namespaceで起動した。
/32・/128のsourceと明示直結host route、通常DADを使う。初回接続でsubnet制約による
拒否とDAD中のworker再起動を再現した。DADを起動contextで待ち、不要なprefix・MAC保持と
受信元high-port制約を削除した。送信は規定high-portを維持し、受信のpeer/discriminator・
interface・TTL/Hop Limit 255検査は残す。
修正後は両系統generation 1でUP、Down transitionとrejectは0だった。
FRRの実control payloadのUDP source portだけをScapyで3784へ変え、元packetを隔離INPUTで
落としてもUPを維持した。出力`dist/frr-bfd-final-peer.log`、private capture場所は
`dist/frr-bfd-location.json`。FRRのdistributed-dataplane設定とstatus大文字小文字を
誤った初期runnerのtimeoutは、製品の結果には数えない。

別の実FRR `bgpd`/`bfdd`と製品BGP/BFD・network worker、独立LAN clientで、
IPv4/IPv6経路学習・kernel FIB・転送TCP HTTP/UDP echoが通った。
IPv4だけのBFD遮断でIPv4 routeを撤去し、IPv6 TCP/UDPは継続した。遮断解除で復旧した。
BFD4のSIGSTOPでは元受信期限で経路を撤去し、SIGCONTで復旧した。
SIGKILL後の新workerでも復旧し、無関係なBFD6はgeneration 1を維持した。
最初の再実行は新worker起動直後の旧routeを復旧と誤認し、TCPの直後の撤去でUDPが失敗した。
captureの到達不能と世代を照合し、新BFD世代に結び付くBGP再学習とFIBを待つようrunnerを直した。
製品の検出期限・通信deadlineは緩めていない。
出力`dist/frr-bgp-fresh-peer.log`、capture場所は`dist/frr-bgp-location.json`。
`make check-bfd`の既存6件も成功したが、手書きwire peerによるkernel/失敗試験であり、
独立FRR相互接続とは区別する。出力`dist/bfd-frr-daemon-related.log`。
`make check`・Race Detector・vet・amd64/arm64の6静的ELF buildも成功した
（`dist/bfd-frr-final-check-build.log`）。arm64実行、認証・全profile・VRF/規模・物理機器は未完了。

## VRRP通常DADとkeepalived

製品`vrrp-dad`、keepalived 2.2.8、実supervisor/control/network/VRRPと隔離namespaceで確認した。
election sourceのNoDADを外すと、IPv6 unicast/multicastともDAD中にworkerが終了し、第6世代で起動した。
DADまたは自動link-local生成だけを起動contextで待つよう修正し、両方ともgeneration 1で
VIP TCP/UDP・keepalivedとの優先度切替・worker交替・復旧が通った。
HAの仮想addressに対するwriterの意図したNoDADとは区別する。
IPv4、Health/BFD/BGP tracking、v2/PASS、追加WAN identityの既存実peer試験も個別に通った。
追加identityの試験は、SIGSTOP中のcontrolに通信前diagnosticが同期問い合わせしていた。
この不要処理を削除し、writerによる元期限のidentity/RA撤去・peer通信・復旧を確認した。
root stagingのcleanupも所有した一時directoryだけのPython削除へ揃えた。

550ms writer停止の初回は、再開時の元期限残り約114ms内に適用できずworkerが再起動した。
`make check`同時実行中だったが、それだけを原因とは断定しない。単独再実行は通った。
期限を延ばしたり成功条件を緩めたりしていない。期限内性能は未解決で、通常DAD修正の完了と分ける。
出力は`dist/vrrp-dad-generation-{unicast,multicast}/`（修正前）、
`dist/vrrp-normal-dad-peer.log`（IPv4/IPv6とmulticast成功、slow ACK失敗）、
`dist/vrrp-dad-slow-ack.log`、`dist/vrrp-dad-remaining-peer.log`（最後のdiagnostic失敗以外）、
`dist/vrrp-dad-additional.log`（diagnostic削除後成功）。
`make check`・全体Race Detector・vet・6静的ELF buildは成功した（`dist/vrrp-dad-check-build.log`）。
旧checksum方式のpeer、全resource/profile、HA session継続、物理/性能の受入は未完了。

## IPv4 VRRPv3の標準keepalived checksum

従来のIPv4 peer fixtureはkeepalived側に`v3_checksum_as_v2`を付けていた。
これを外すと、標準keepalived 2.2.8と双方でchecksum拒否になり、優先度切替が8秒で失敗した。
移植元routerdはkeepalivedを使っており、この相互接続範囲を落としていた。
受信はIPv4 VRRPv3のmessage-onlyとpseudo-headerの両方式を許可する。
送信は省略/`ipv4_checksum: rfc9568`で現行message-only、`rfc5798`で実source/destinationの
IPv4 pseudo-headerを含める。送信形式はpeerに合わせて明示し、受信条件と結合しない。
IPv6とv2のchecksum規則は変えない。内部prototype形式の維持ではなく、実在するnetwork peerへの対応である。

製品`vrrp-checksum`、実supervisor/control/network/VRRPと独立keepalivedで、標準設定peerと
RFC 9568方式peerの両方がVIP TCP/UDP・優先度切替・worker SIGKILL/rebind・peer停止/復旧を通った。
標準checksumのHealth連動と追加WAN identity/RA撤去・復旧も通った。
出力`dist/vrrp-checksum-final-peers.log`と`dist/vrrp-checksum-final-peers/`、
その`*-lan0.pcap`と`*-wan0.pcap`に実packetがある。runnerは既存captureの出力を保存する。
実keepalived packetから抽出した2つのchecksumを、反対の送信設定でも受け入れる小さな回帰試験も通った。
`make check`・Race Detector・vet・6静的ELF build、関連raceとcapture回帰試験は成功した
（`dist/vrrp-checksum-check-build.log`、`dist/vrrp-checksum-related-local.log`、
`dist/vrrp-checksum-captured-regression.log`）。前節の期限内適用失敗はこれで解決したとはしない。

同じ版を既存uplink上の専用macvlanと別LAN namespaceへ再配備した。
保存状態なし・通常DADで全workerがgeneration 1、実上流RA/DHCPv4/PD /60、
LAN IPv4/IPv6 HTTPS 301を確認した。この版の130分予定試験はPD source版へ切り替えるため
途中で意図的に停止した。`dist/live-current-continuity.log`末尾のSIGINTはその停止で、
完走/自然T1更新の成功でも製品障害でもない。既に完走した100分の版は前述の`ntt-aftr`である。
その実行場所は`dist/live-current-location.json`で、所有者がcleanup済み。
`dist/live-current-continuity.log`に経過を出す。ホスト管理NIC・route・clockは変更していない。

## PD/IA_NA由来のDS-Lite source

旧routerdの`localDelegatedAddress`/`localAddressSuffix`と専用source address設置を照合した。
`source_from`はRAまたはDHCPv6を参照する。DHCPv6で`source_delegation`を指定すると、
そのLAN割当のmanaged addressと元preferred/valid期限を使う。省略時はIA_NAのWAN addressを使う。
同じPD /64にLAN用`::1`とB4用`::100`を別bindingで設置でき、advertiseするbindingだけがRAを起動する。
旧宣言の別suffixも追加managed bindingへ変換する。nativeは/64 addressを使い、旧専用/128と
byte-for-byte同等ではない。全SelfAddressPolicy候補・HA・複数WAN/VRFのsource契約は未完了。

static sourceの宣言とdelegation解決は設定compile時に一度行う。kernel queryは実際のowner
interfaceを読み、PDではLANの所有権・DAD・preferredを確認する。DNSとtunnelはWANへ束縛する。
既存の同じ型付きprefix計算をLAN投影とsource選択で使う。Network契約にSourceAddressを必須とし、
optional method probeを削除した。新しいsource registry・leaseコピー・DNS source管理は作らない。
[設定例](../examples/wan-dslite-pd.yaml)はCLI validate済みで、provider名/interfaceは置換が必要である。

製品`pd-aftr-source`と独立ISC Kea 2.4.1・dnsmasq・Linux ip6tnl AFTR・別LAN clientで、
PD `::100`とWAN IA_NAの両方を確認した。WAN/LANは通常DADで全workerがgeneration 1。
実DNS query sourceとencapsulated TCP/UDPをcaptureした。Keaの更新を跨いだ同一TCP、
tunnel index不変、元preferred期限での撤去、Kea再開後のTCP/UDP復旧が通った。
このpeerはWAN接続/通常DAD後に起動し、固定した単一NA/PD poolを使う。追加aliasとLAN本来のaddressは同じ/64である。
出力`dist/pd-aftr-final-{pd,na}.log`、private setup/capture場所は
`dist/pd-aftr-{pd,na}-location.json`。最初の15秒処理/8秒runner timeoutはsetup失敗で、製品結果には数えない。
既存`make check-ra-aftr`も成功し、RA source/DNSと元期限の独立writer撤去を維持した。
`make check`・全体Race Detector・vet・6静的ELF buildと、引数整理後の関連race/vet/6buildも成功した
（`dist/pd-aftr-source-check-build.log`、`dist/pd-aftr-source-reviewed-{race,vet,build}.log`）。

同じ機能を実上流の専用macvlanで実行した。RA/DHCPv4/PD /60とLAN `::1`/`::100`を取得し、
通常DAD・fresh stateで全workerがgeneration 1。PD内`::100`から実DNS/AFTRを使い、
LAN IPv4/IPv6 HTTPS 301とUDP DNSが通った。route getはdslite0、captureはPD source/実AFTR間の
IPv4-in-IPv6往復を確認した。別のIPv4 defaultによる成功とは数えていない。
現行の場所は`dist/live-pd-source-location.json`、captureはそのprivate stage、
実行ELFのSHAは`dist/live-pd-source-executed-hashes.json`。
同一TCP/UDPの130分試験は`dist/live-pd-source-continuity.log`へ経過を出す。
元T1=7200秒の自然更新を含むこの版の完走はまだ確認していない。所有者は3時間上限でcleanupする。
対象ONU型番・直結配線・物理NIC/性能のqualificationとは区別する。

## 未完了と次の作業

1. 全実装計画、全更新復旧経路、外部engine、配備先と対象実機の受入は未完了。
   旧routerdの各モジュールとの比較を続け、任意optionの不良で有効な割当・経路まで捨てる受信処理を修正する。
   複数IA_NA/PDのDAD共存・Release・resource readiness/RA連携も未完了。
   NET-06はrelayのHA・動的VRF/interface変更の受入、DHCPv6 server、dynamic pool/options・DNS・sticky/lease importが残る。
   今回の試験成功を製品全体・実機利用可能の判定には使わない。
2. NET-07の旧resource/profile、動的source・VRF経路変更、大規模RIBを実装・受入する。
   IPv4/IPv6のmultihop BFD/BGPは実通信したが、routed/VRFでの通常DAD起動、IPv6 single-hop VRF・link-local next-hop・ECMP等は未受入。
   HA-01の残るresource種別・phase tracking、外部secret参照、peer-aware移行、cluster、
   二native nodeの追加identity・RA・DHCP/PD/NAT session継続を実装・受入する。
3. 過去のBFD再接続timeoutの失敗request、IPv6 multicast未受信、初回IPv6 VIP TCP/DAD timeoutは
   原因未確定。IPv6 unicastの初回TCP timeoutと同時の選挙authority期限切れは以前の試験で一度再現した
   （`dist/vrrp-multicast-related.log`）。syscall trace付き実行では再現せず、要求前後の選挙snapshotを
   失敗時に表示するようfixtureへ追加したが、同じ失敗は再取得できていない。接続deadlineや
   選挙authorityは延長していない。過去に修正したACK/status問題と同一とは断定しない。
4. 残るprotocol snapshotの型付き保持、条件式の静的計算、writer内部のsnapshot複製を整理する。
   DHCP server割当/journal等の残るUTC依存を整理し、実clock補正・停電・suspendで受入する。
5. 再帰next-hopの前提address/link期限はローカル投影で確認済みだが、同じ条件の実daemon試験は未実施。
   writer自体の停止中のlink fencing・IPv4 route撤去、物理suspendは保証しない。
6. arm64は静的buildとbundle検査までで、実行していない。物理NIC・性能・suspend、
   今回の変更でQEMU時計補正は再実行していない。対象実機の条件は未確定である。

## 環境

PVE隔離VM、Ubuntu kernel 6.8、amd64。隔離namespaceかNICなしQEMUを使い、
ホストの管理NIC・route・clockは変更しない。通常build/testは `umask 022`。
rootで動かすharness/binaryは一時的なroot所有0700ディレクトリへ配置する。
homeの権限は緩めない。実回線接続では既存uplink上の専用macvlanと別LAN namespaceを使う。
namespace・daemon・captureの所有者が時間上限と終了時cleanupを持つ。実機の型番・NIC性能・
直接ONU接続の受入と、今回の実上流相互接続は区別する。
