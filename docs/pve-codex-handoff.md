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
- IPv4/IPv6 v3 unicast、IPv4 v2と任意PASS、有限のHealth trackingに対応する。
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
- Health監視と履歴配送は分離する。保存先障害・queue満杯でもprobeを止めず、欠落と保存エラーを表示する。
  全probeの無欠落保存は保証しない。

現在のworker ABIは2、compiler contractは `resource-program-native-8`、network journalはformat 5。
以前のprototypeとの混在・rollbackは受け入れない。

## 今回の検証

最終製品buildは `RELEASE_ID=bfd-source-budget`。PVE隔離Linux VMで関連Race Detector、
fmt・全体vet・amd64/arm64静的build、全体`make check`が成功した。arm64は実行していない。
今回の修正ではworker ABI 2・compiler contract native-8・network journal 5を変更していない。

最終amd64製品とRace Detector付きharnessで、独立UDP BFD peer・TCP BGP peer・実kernelを使った。

- controlを350ms停止する。元のBFD期限はまだ有効なのに、c169643製品では一度のHTTP timeoutで
  実BGP TCP接続が閉じることを再現した。修正後は同じ接続・worker・経路が維持される。
- 実UDP BFDを止めると、TCP KEEPALIVEが続いていてもsessionとFIBを撤去する。再開後に復旧する。
- BFD workerのSIGSTOP中も、元の受信期限でBGPと経路が失効する。再開後はfreshな受信で復旧する。
- BGP・BFD・controlが停止中も、surviving writerはBFDの元の期限で経路を撤去する。
  長いBGP観測期限の失効に頼らない。
- BGP FIBの追加・withdrawと、writer再起動後の元の期限による撤去を確認した。
  再帰IPv6 routeの元の前提期限による撤去も、supervisor・control・BGP停止中に確認した。

ローカルでは、長い9秒のpacket検出期限をproducerの元の5秒観測期限で制限し、
原通知を変えずに失効させることを確認した。この長い間隔の同一条件は実daemonでは未試験。
期限後にqueueから届いた更新が失効を隠さないこと、再送での延長拒否、TCP終了・撤去ACKとの
対応付けも検証した。過去の再接続timeout自体には失敗requestの診断がなく、同じ原因とは断定しない。

直前の `RELEASE_ID=dhcp-source-clock`（c169643）では次を確認済み。今回DHCP経路は変更していない。

- 実DHCP ACK→DNSで元のboot ID・観測時刻・期限が一致する。native sourceとrouterd入力の両方を使う。
- server・control・supervisor停止中もDNSが元の期限でhostnameを除外し、static DNS・同じTCP接続・
  DNS PIDを維持する。APIは元通知のActive=trueと現在のOwnershipExpired=trueを区別する。
- server再起動とjournalからのlease復旧、clientのINIT-REBOOTへの同じaddressのACK、同じDNS接続での復旧。
- 遅い成功ACKからのDHCP応答拒否、publication上限超過時のReady／ACK拒否と割当除外の保存。
- 全体`make check`と最終6 ELFの署名bundle・catalog・CLI publisher検査。今回のBFD buildでは
  この6 ELF検査は再実行していない。publisherの入力は専用0700ディレクトリへ配置し、入力検査を緩めていない。

Health→PBRの独立2 WAN・実TCP/UDP・packet capture、worker再起動・古い通知・confirmed rollback、
graceful VRRP・追加WANとKeepalivedは、今回のBFD buildでは再実行していない。
過去の成功を全体完成の判定には使わない。

今回のログは `dist/bfd-control-budget-repro.log`（修正前の実通信失敗）、
`dist/bfd-source-budget-{build,final-race,native,check}.log`。
DHCP/DNSは `dist/dhcp-source-clock-{release-build,final-race,native,final-bundle,final-check}.log`。
出力との一致を実装条件にしない。

## 未完了と次の作業

1. 以前の再接続timeoutの失敗request、IPv6 multicast未受信、初回IPv6 VIP TCP timeoutは原因未確定。
   今回再現・修正した短いcontrol停止による不要な切断とは区別する。成功回数で閉じない。
2. 残るprotocol snapshotの型付き保持、条件式の静的計算、通常投影のinstance生成・digest再計算を整理する。
   DHCP client割当とjournalのUTC依存を整理し、実clock補正・停電・suspendで受入する。
3. HA-01のHealth以外のtrack・neutral、外部secret参照・peer-aware移行、clusterと、
   二native nodeの追加identity・RA・DHCP/PD/NAT session継続を実装・受入する。
4. 再帰next-hopの前提address/link期限はローカル投影で確認済みだが、同じ条件の実daemon試験は未実施。
   writer自体の停止中のlink fencing・IPv4 route撤去、物理suspendは保証しない。
5. 全実装計画、全更新復旧経路、外部engine、配備先と対象実機の受入は未完了。
   物理NIC・性能・suspend、QEMU時計補正は今回再実行していない。

## 環境

PVE隔離VM、Ubuntu kernel 6.8、amd64。隔離namespaceかNICなしQEMUを使い、
ホストの管理NIC・route・clockは変更しない。通常build/testは `umask 022`。
rootで動かすharness/binaryは一時的なroot所有0700ディレクトリへ配置する。
homeの権限は緩めない。対象実機・NIC・性能条件はユーザーへ照会中で、回答を待たず独立作業を続ける。
