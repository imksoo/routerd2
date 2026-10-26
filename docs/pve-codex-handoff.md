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

現在のworker ABIは3、compiler contractは `resource-program-native-11`、network journalはformat 5。
`track.resource`は宣言IDであり、decisionはhealthy/unhealthy/neutral/unknownを明示する。
破棄したservice-only形式のreader・混在運用・migrationは実装しない。

## 今回の実装と検証

最新buildは `RELEASE_ID=bfd-vrf-ipv6`。NET-07の同一endpointを持つIPv4/IPv6の
blue/red VRFとdefault domainを実daemonへ接続した。IPv6 BGP transport、再帰next-hop、
learned route先のTCP/UDPも実Linux routerを越えて動く。設定例
[bfd-vrf.yaml](../examples/bfd-vrf.yaml) をdual-stackへ更新し、製品CLIでvalidateした。

三domainを同時に起動するとdefault BFDのUDP 4784 bindが`address already in use`で失敗した。
multihopの受信socketだけに`SO_REUSEADDR`を指定して修正した。domain/sourceの既存abstract
claimでrouterd受信ownerは一つに限定し、TX portの専有と`SO_REUSEPORT`不使用を維持する。
新しいmanager・lease・別の状態機械は追加していない。実ソケット試験でIPv4/IPv6とも二重ownerを拒否した。

BGP設定の正規化では、全configをJSONへencode/decodeして複製する処理を除いた。
固定版normalizerが既に再構築するslice/mapを使い、残るnative mapとgraceful-restart pointerを
明示的に複製する。結果を変更しても呼出元のpolicy・path・pointer・transportを変更しない。
512 peerのこのVMでの3回測定は、中央値3.606ms/835122Bから0.351ms/199417Bへ減った。
これは設定正規化の測定であり、packet throughputや対象機の性能受入ではない。
再測定は `go test ./internal/protocol/bgpconfig -run '^$' -bench BenchmarkNormalize512Peers -benchmem`。

PVE隔離amd64 VMで、CGO無効の実3 ELF、Race Detector付きharness、独立UDP/TCP peerと
Linux forwarderを使用した。三domainのIPv4/IPv6個別試験で次を確認した。

- 同じsource/remoteをblue/red/defaultで同時使用し、それぞれのBFDとBGPを接続する。
  利用者TCP/UDPの異なる応答で各domainの実経路を識別する。
- 初回BFD Down中はBGPを接続しない。blueだけのUDP停止でそのTCPを終了し、学習経路を撤去する。
  defaultに同じ宛先への通信可能な経路があってもblueの利用者通信を受け渡さない。
  UDP復旧、blue BFD SIGKILL後のfresh producer、同じBGP workerでの再接続・経路復旧を確認した。
- red/defaultのTCP session・通信・worker PID/世代は継続する。
- default BFDをSIGSTOPすると、保持したraw UP通知の元の期限でhealthが失効し、
  default BGPのTCPと学習経路が失効する。blue/redのsessionと通信は継続する。
  SIGCONT後は同じworkerの新しい受信でBGPと学習経路が戻る。
- 三つのWANのcaptureでUDP 4784、source port 49152以上、送信255／受信254を検査した。
  default domainの実BFD/BGPも同時に動くため、単にVRF同士のsocketが衝突しないだけの試験ではない。

IPv6で自動link-localがDAD中の初回TCPは、SYNもNSも出ずneighbor cacheが未解決だった。
このfixtureでは各転送linkへ協調した明示link-localをNoDADで設定している。
loopback sourceではLinuxが送出link上の利用可能なND sourceを必要とする挙動と一致する。
[Linux 6.8の実装](https://github.com/torvalds/linux/blob/v6.8/net/ipv6/ndisc.c#L612-L715)を参照。
自動生成link-local DADの起動受入へ成功を広げない。client deadlineとprotocol期限は延長していない。
新規隔離namespaceで`udp_l3mdev_accept=0`も確認した。別のaccept policyは未受入である。

IPv4のdefault BFD凍結後、blueの利用者TCP接続が一度300msでtimeoutした。
captureにはその試行のSYN/SYN-ACK/ACKがあり、直後に接続を閉じている。
trace付きと通常の再実行、IPv6の同じ試験は成功したが、原因は未確定である。
成功した再実行を原因解決とは扱わない。失敗時刻の出力はローカル
`dist/bfd-vrf-ipv4-timeout/`、syscall比較は `dist/bfd-vrf-ipv4-syscall/`。

再実行は `make check-bfd`。rootまたはpasswordless sudo、Linux user/network namespaceが必要。
製品とharnessはroot所有0700へ配置し、失敗した実行のcaptureも回収する。home権限は変えない。
関連Race Detector、fmt・vet、amd64/arm64の6静的ELF buildは成功した。
全体`make check`、最終6 ELFのsigned bundle/catalog/publisher検査、最終native全6ケースは成功した。
実ソケットの二重owner拒否も最終コードで再実行して成功した。amd64は実通信、arm64はbuild/bundle検査までである。
worker ABI 3、compiler contract native-11、network journal format 5は変更していない。
prototype reader・migrationは追加していない。

現在の個別実行は `dist/bfd-vrf-ipv{4,6}-expiry{.log,/vrf*.log}`。
BFD native socketは `dist/bfd-vrf-ipv6-ownership.log`、関連Race Detectorは
`dist/bfd-vrf-ipv6-race.log`、benchmarkは `dist/bgp-normalize-{before,after}.log`。
全体check・build・bundleは `dist/bfd-vrf-ipv6-{final-check,static-build,final-bundle}.log`。
最終全ケースの出力先は `dist/bfd-vrf-ipv6-final-native{.log,/}`。
過去の出力・checkpointは実装制約にしない。

先行buildでは実2 WANのHealth→PBR・実TCP/UDP・worker再起動・古い通知・期限切れ・
confirmed rollback、およびKeepalivedとのtracking・IPv6 multicastを確認している。
これらを今回のbuildで再実行したとは扱わない。

## 未完了と次の作業

1. 全実装計画、全更新復旧経路、外部engine、配備先と対象実機の受入は未完了。
   今回の試験成功を製品全体・実機利用可能の判定には使わない。
2. NET-07の旧resource/profile、動的source・VRF経路変更、大規模RIBを実装・受入する。
   IPv4/IPv6のmultihop BFD/BGPは実通信したが、自動link-local DAD起動、IPv6 single-hop VRF・link-local next-hop・ECMP等は未受入。
   HA-01の残るresource種別・phase tracking、外部secret参照、peer-aware移行、cluster、
   二native nodeの追加identity・RA・DHCP/PD/NAT session継続を実装・受入する。
3. 過去のBFD再接続timeoutの失敗request、IPv6 multicast未受信、初回IPv6 VIP TCP/DAD timeoutは
   原因未確定。IPv6 unicastの初回TCP timeoutと同時の選挙authority期限切れは今回も一度再現した
   （`dist/vrrp-multicast-related.log`）。syscall trace付き実行では再現せず、要求前後の選挙snapshotを
   失敗時に表示するようfixtureへ追加したが、同じ失敗は再取得できていない。接続deadlineや
   選挙authorityは延長していない。今回修正した二つのACK/status問題と同一とは断定しない。
4. 残るprotocol snapshotの型付き保持、条件式の静的計算、通常投影でのinstance生成・digest再計算を整理する。
   DHCP client割当とjournalのUTC依存を整理し、実clock補正・停電・suspendで受入する。
5. 再帰next-hopの前提address/link期限はローカル投影で確認済みだが、同じ条件の実daemon試験は未実施。
   writer自体の停止中のlink fencing・IPv4 route撤去、物理suspendは保証しない。
6. arm64は静的buildとbundle検査までで、実行していない。物理NIC・性能・suspend、
   QEMU時計補正は今回再実行していない。対象実機の条件は未確定である。

## 環境

PVE隔離VM、Ubuntu kernel 6.8、amd64。隔離namespaceかNICなしQEMUを使い、
ホストの管理NIC・route・clockは変更しない。通常build/testは `umask 022`。
rootで動かすharness/binaryは一時的なroot所有0700ディレクトリへ配置する。
homeの権限は緩めない。対象実機・NIC・性能条件はユーザーへ照会中で、回答を待たず独立作業を続ける。
