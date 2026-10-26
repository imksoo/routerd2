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

最新buildは `RELEASE_ID=projection-ownership`。CORE-03の通常投影で繰り返していたworker設定生成・
JSON化・hashを除いた。受理したsourceごとに、条件除外中のworkerと生成予定のRAも含めて
設定・canonicalな子process用bytes・digestを一度準備する。通常投影はその中から必要なworkerを
選ぶ。新しいmanager、invalidation flag、旧形式adapterは作っていない。

BGP/VRRPの型付き設定とLAN RAの固定templateも同じ不変planに保持する。
BGPのFIB投影とAFTRの入力digest確認でもworker設定を再構築しない。
設定切替・停止待ちの前後は最新のmatching観測と選択したidentityを比較する。
RAのprefix・期限・DNSはlive入力のままで、更新してもworker自身のidentityは変えない。
childへ保存した実bytesのSHA-256とsupervisorへ渡すidentityの一致も確認した。

8つのBGP worker／各64 peerの静的投影＋worker選択を、このVMで3回測定した。
中央値は2.531ms・540004B・1238 allocationsから、7.756µs・4021B・20 allocationsへ減った。
準備は設定受理時へ移している。learned RIBの走査・kernel適用・packet性能の測定ではない。
この比較は `protocol-plan` build時の測定である。
残るprotocol snapshotのdecode、条件式の静的計算は未整理である。
再測定は `go test ./internal/runtimeplan -run '^$' -bench BenchmarkProjectEightBGPRouters -benchmem`。

続いてprotocol投影のnetwork intentのJSON往復複製を削除した。interface・address・
options・routeの変更する集合だけを複製し、追加だけを行うlink・tunnel・guardは
元の配列へ書き込まない。残りの固定設定は読み取り専用で共有する。
RA取得・学習情報撤去・address/route期限切れが、sourceや先に作った投影を変更しないことを
ローカルで確認した。writer内部のsnapshot複製にはJSON往復が残る。

PVE隔離amd64 VMで、CGO無効の実3 ELF、Race Detector付きharness、独立peerと実kernelを使用した。

- 実2 WANのHealth→PBR、TCP/UDPとcapture、worker交替、古い通知、期限切れ、confirmed rollbackが成功した。
- 直前の `61a2918` ではBFD/BGPの全6ケースが成功した。single-hop、routed IPv4/IPv6、
  同一endpointを持つblue/red/default domain、片側UDP障害、source凍結、worker交替と復旧を含む。
  最新buildではIPv6 VRFの独立domain・障害・撤去・復旧を再確認した。
- KeepalivedとのHealth trackingでpriority 150/50、finite neutral、同じ選挙worker、VIP TCP/UDPを確認した。
- AFTRの実DNS解決、source/instance identity、tunnelとrouteの期限処理が成功した。
- 新しい独立DHCPv6 serverは製品のPD codec/state machineを使わずに応答を組み立てる。
  実daemonのPD取得・renew・再番号付けで、同じRA workerから新旧LAN prefixの広告を確認した。
  LAN clientの実TCP/UDPを転送し、PD期限切れでRA workerと管理addressが消えることを確認した。
  再取得では新しいRA generationが、事前に準備した同じ設定digestとfreshな広告で動いた。
  link-local addressは隔離された静的topologyで明示・協調している。host SLAACやIA_NAの受入ではない。

Health/BFD/PD-RA/AFTRの実daemon起動は `scripts/daemon-tests.py` へ共通化し、旧scriptを削除した。
`make check-health-pbr`、`make check-bfd`に加え、`make check-pd-ra`と`make check-aftr`で再実行できる。
特定ケースは例として `python3 scripts/daemon-tests.py bfd --test TestDaemonBFDVRFIPv6Isolation`。
製品とharnessはroot所有の一時0700へ配置し、成功／失敗双方のcaptureを回収する。
rootまたはpasswordless sudoとLinux user/network namespaceが必要で、home権限は変えない。

全体`make check`（Go/Python・Race Detector・vet）とamd64/arm64の6静的ELF buildが成功した。
実daemon用amd64 ELFと、bundle検査用のELFは同一である。Health/PBR・PD/RA・AFTR・
IPv6 VRF BFD・Keepalived Health trackingが最新buildで成功した。signed bundle/catalog/publisher検査も成功した。
worker ABI 3、compiler contract native-11、network journal format 5は変更していない。
arm64実行、物理NIC、性能、suspend、clock補正の受入は今回行っていない。

現在の出力:

- `dist/projection-ownership-{local,check-build,bundle}.log`: ローカル投影・全体check・6 ELF build・bundle。
- `dist/projection-ownership-{health,pd-ra,aftr,bfd,vrrp}{.log,/}`: 実daemonのログとcapture。
- `dist/protocol-plan-{before,after}.log`: 直前のworker準備変更の静的投影benchmark。
- `dist/protocol-plan-final-bfd/`: `61a2918` の全6 BFDケース。

過去の出力・checkpointは実装制約にしない。前buildで一度発生したdefault BFD凍結後の
IPv4 client TCP 300ms timeoutは、原因未確定のままである。今回の高速化で解決したとは断定しない。
自動IPv6 link-local DAD起動も未受入であり、明示NoDADのtopologyを一般化しない。

## 未完了と次の作業

1. 全実装計画、全更新復旧経路、外部engine、配備先と対象実機の受入は未完了。
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
