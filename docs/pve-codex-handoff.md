# routerd2 開発引き継ぎ

2026-10-26。全体ゴールは継続中。対象実機で利用可能とは判断していない。
機能範囲は [implementation-plan.md](implementation-plan.md) と
[implementation-plan.json](implementation-plan.json) を参照する。

[AGENTS.md](../AGENTS.md) とユーザー指示に従い、未公開API・保存形式・過去の試験結果を
維持するための実装は残さない。過去の `docs/evidence` は削除済み。
現在の設計に合わせてfixtureを更新し、prototype reader・adapter・migrationは作らない。

## 現在の実装

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

現在のworker ABIは3、compiler contractは `resource-program-native-16`、network journalはformat 7。
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
実回線の長時間TCP/UDPと元T1でのPD更新は継続中で、まだ成功と数えない。
実行中の回線バイナリーは`ntt-aftr`であり、以降の修正まで実回線受入済みとはしない。

## 未完了と次の作業

1. 全実装計画、全更新復旧経路、外部engine、配備先と対象実機の受入は未完了。
   旧routerdの各モジュールとの比較を続け、任意optionの不良で有効な割当・経路まで捨てる受信処理を修正する。
   複数IA_NA/PDのDAD共存・Release・resource readiness/RA連携も未完了。
   NET-06はrelayのHA・動的VRF/interface変更の受入、DHCPv6 server、dynamic pool/options・DNS・sticky/lease importが残る。
   今回の試験成功を製品全体・実機利用可能の判定には使わない。
2. NET-07の旧resource/profile、動的source・VRF経路変更、大規模RIBを実装・受入する。
   IPv4/IPv6のmultihop BFD/BGPは実通信したが、自動link-local DAD起動、IPv6 single-hop VRF・link-local next-hop・ECMP等は未受入。
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
