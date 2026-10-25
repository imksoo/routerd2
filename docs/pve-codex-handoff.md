# routerd2 開発引き継ぎ

2026-10-25。全体ゴールは継続中。対象実機で利用可能とは判断していない。
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
- Health監視と履歴配送は分離する。保存先障害・queue満杯でもprobeを止めず、欠落と保存エラーを表示する。
  全probeの無欠落保存は保証しない。

現在のcompiler contractは `resource-program-native-8`、network journalはformat 5。
以前のprototypeとの混在・rollbackは受け入れない。

## 今回の検証

製品は `RELEASE_ID=conditional-authority`。PVEの隔離Linux VMで、最新変更の関連Race Detector、
fmt・全体vet・amd64/arm64静的buildが成功した。arm64は実行していない。
最終6 ELFを使う署名bundle/publisher検査も成功した。全体`make check`も成功した。

実supervisor/control/network/Healthと独立2 WANのHealth→PBR試験が成功した。
実TCP/UDPと両WANのpacket captureで次を確認した。

- 主経路→障害→予備経路→閾値復旧とconsumer hold。operator revisionは維持する。
- 元の観測期限、consumer max_age、古い通知・重複・再送による期限延長の拒否。
- 各workerと全process treeの再起動、旧tree通知の拒否、confirmed rollback。
- controlと両HealthをSIGSTOPし、network workerをSIGKILLして再起動する。
  同じPID/世代でReady登録のままの停止workerに頼らず、元の期限で条件付きIPとPBR routeが消える。
  期限後のTCP/UDPは失敗し、物理link・静的probe IP・foreign objectは保持する。
  再開後のfreshな実probeで、転送と条件付きIPのTCP/UDPが復旧する。

UTC lifetime・条件・依存期限の交差、宣言の不変性、ACK後に期限切れとなった
link/address/routeのReady拒否と再適用後の復旧はローカル試験で確認した。
明示UTC期限が切れたaddress/routeは正常に投影から除かれ、適用待ちとして残らない。

最終buildでgraceful VRRPと追加WAN/Keepalived、実BFD/BGP、再帰IPv6期限、
Health→DerivedEvent/EventRuleとworker/control/logging再起動の回帰が成功した。
追加WANのpeer macvlanは事前作成し、
IPはKeepalivedが管理する。bridge学習の収束待ちは2秒で、無欠落切替の受入ではない。

現在のログは `dist/conditional-authority-final-{check,release-build,health-pbr,protocols,bundle}.log`、
`dist/conditional-authority-dependency-race.log`、
`dist/health-pbr-test/{test.log,wan0.pcap,wan1.pcap}`、`dist/vrrp-unicast/*.log`。
過去の出力との一致を実装条件にしない。

## 未完了と次の作業

1. 以前のBFD再接続timeout、IPv6 multicast未受信、初回IPv6 VIP TCP timeoutは原因未確定。
   成功回数で閉じない。今回の変更で原因を修正したとは判断していない。
2. 残るprotocol snapshotの型付き保持、期限切れJSON書換えの削除、条件式の静的計算、
   通常投影のinstance生成・digest再計算を整理する。
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
