# routerd2 開発引き継ぎ

2026-10-25。全体ゴールは継続中。対象実機で利用可能とは判断していない。

[AGENTS.md](../AGENTS.md) とユーザー指示に従い、未公開API・保存形式・過去の試験を
維持するための実装は残さない。過去の `docs/evidence` は削除した。
機能範囲は [implementation-plan.md](implementation-plan.md) と
[implementation-plan.json](implementation-plan.json) を参照する。

## 実装した構造

- 宣言はcontrol所有の読み取り専用データとする。plan/programを一度compileし、
  有限の観測factを直接評価する。一時FactStore・仮producer登録・設定のJSON複製は廃止した。
- 通常通知は観測の受理を返し、BGP/VRRPはcoordinatorの実適用を待つ。
  config適用・定期更新・worker起動停止待ちでkernel書込みと完了処理を共有する。
  I/O中は観測mutexを解放し、古い結果で新しい観測を完了扱いにしない。
  kernel障害で受理済み観測を巻き戻さず、最新観測を再適用する。
- 通常監視・起動停止待ち・設定切替は同じreconcileを一回ずつ進める。installedの重複保持と
  停止待ち専用の処理を削除した。supervisorの実登録と設定identityで継続・退役を選ぶ。
  通常監視は停止を要求して戻り、次回に実終了を確認する。設定切替だけが期限付きで待つ。
  切替前は退役workerの終了だけを待つ。継続workerのReadyは新設定適用後に確認し、
  現在の準備失敗が復旧設定を妨げないようにする。停止要求前にapplyingへ更新する。
  前提がI/O中に変わったworkerは起動せず、最新の観測で再評価する。
- instanceファイルは同じ準備済みbytesでdigestを計算し保存する。二重書込みは廃止した。
  ファイル準備・BFD解析は観測mutexの外で行い、停止中の更新をcutoverへ反映する。
- VRRPのgrant発行・返送、仮VIP、期限昇格、受信時刻から作る別リース、失効JSON書換えを
  削除した。選挙観測から直接factを作り、成功したkernel適用の選挙keyだけを保持する。
  `Effects.Apply` と同じservice要求の応答で、実際にVIPを所有したかをworkerへ返す。
- gracefulの準備条件はkernel適用を開始するときの入場条件である。成功後は、I/O中の喪失を
  含む準備喪失だけでは撤去しない。初回適用失敗は入場許可を残さず、再試行で準備を再判定する。
  条件除外・role喪失・producer交替・元のBOOTTIME期限は引き続き所有権を失効させる。
  GARP/NAは適用応答と実netlink検査の後に送る。準備待ちのgraceful選挙は広告だけを続ける。
- config適用終了と定期更新は同じ準備確認を使う。VIP未公開の起動直後や未適用の観測を
  Ready/confirmed commitの成功へ読み替えない。
- resourceの公開状態から投影中だけ使う期限を分離した。supervisorの準備時刻の更新で
  状態比較が永久に不一致になる原因を除いた。ReadyはACK済みlinkの元の期限も確認し、
  新しい準備観測だけで期限切れの実適用を成功扱いにしない。
- Healthの現在監視と履歴配送は分離済み。保存先障害・queue満杯でprobeを止めず、欠落と
  保存エラーをsnapshotへ表示する。全probeの無欠落保存は保証しない。
- VRRPはIPv4/IPv6 unicast peerに対応した。明示source、同familyのpeer一覧、実宛先、
  TTL/Hop Limit 255を使う。IPv6 global sourceにも対応し、checksumは実宛先で計算する。
  multicastはフィールド省略で選び、明示空配列をmulticastへ読み替えない。
  受信側の仮Ethernet/IPv6ヘッダ生成と本番でのEthernet再解析を削除し、kernelのIP metadataを
  直接検証する。unicast送信はkernelのrouting/ARP/NDへ任せ、VIP/GARP/NAの適用ACKは維持する。
- Healthを参照するVRRP trackを追加した。penalty・連続probe回数・元の観測のmax ageを
  controlで判定し、既存のVIP適用応答で有限の判定を返す。再送やpollで確認回数を増やさず、
  worker/設定の交替や監視の空白後には新しい正常probeを要求する。VRRPは自分のboot clockで
  判定を失効させ、設定のbase priorityからwire priorityを導出する。Health flapでintentを
  書き換えたりVRRPを再起動したりしない。compiler contractは `resource-program-native-7`。
- 実通信で発見したVIP期限更新の不具合を修正した。kernelの秒単位lifetimeの近似一致を
  更新不要の判定へ流用せず、元の絶対期限が変わったらkernel timerも更新する。
  同じ期限の再適用ではtimerを延長しない。所有記録だけを延長してVIPが消える原因を除去した。
- VRRPのwire versionを明示する設定を追加した。既定はv3、IPv4はv2も選べる。
  v2は1..255秒広告と固定1秒基準のskew、広告間隔一致、下位広告の破棄を実装した。
  任意のPASSは8 byteまでとし、認証種別とzero paddingを含む全8 byteを検査する。
  v3/IPv6のPASS、長過ぎるpasswordは拒否する。自動version切替やchecksum fallbackはない。
  PASSは旧peer用の平文方式で、暗号認証やreplay防止ではない。snapshotにはversionだけを返す。
  ownership lifetimeの公開helperと繰り返す設定正規化も除去した。
- 追加WAN/LAN MACは既存の条件付きlink/interface/addressで表す。`.elected`で準備し、
  `.active`でRA等を公開する。新しいVRRP managerや選挙workerは追加しない。
  条件・依存先の準備の元の期限をwriterへ渡し、期限でowned linkをDOWNにして管理addressを
  撤去する。古い要求はUPへ戻せず、UP直前もjournal等のI/Oで期限が過ぎていないか確認する。
  addressの期限は自身とlinkの短い方に制限し、writer停止中もkernelの秒単位timerが残る。
  expired linkのaddress撤去は通常の削除と同じ所有権確認・secondary順序を使う。
- 有効なfinite MACはRARPで所在を通知する。初回と1秒間隔で送り、退出側の最終RA等の後も
  switchの学習を戻す。IPやdefault routeを広告せず、観測・所有期限も延長しない。
  送信直前にboot期限を確認する。scheduleは一時的で、永続的な別のauthorityを作らない。
  元のroute期限loopを共有し、metadata更新だけでkernel linkを書き換えない。
- BGP経路の元のboot IDとBOOTTIME期限をnetwork workerへ渡し、controlと独立して
  期限切れ経路を撤去する。期限更新だけではkernel経路を置換せず、所有記録を更新する。
  ACK済みBFD受信期限と、再帰next-hopに使ったaddress・route・linkの期限も短い方へ
  まとめる。gatewayを直接到達先へ展開しても前提期限を失わない。期限切れの前提は
  解決候補にしない。ECMPも含め一つの経路期限でwriterへ渡し、新しいleaseは発行しない。
  退役時も同じ所有権確認を使い、変更されたforeign経路を残す。table観測と経路選択は
  操作内で索引化した。network journal/runtime宣言はformat 4に直接置換した。
  経路追加は不在確認後に追加予定と元の期限をまとめて永続化し、kernel追加直後の
  writer停止でも復旧側が撤去できるようにした。期限切れ撤去もまとめて保存する。
  journal読取りはatomic置換でunlinkされた開いた旧inodeを許す。lock・書込み先は
  引き続きlink必須で、hard linkは読取り・書込みとも拒否する。

## 今回の確認

追加identityは `RELEASE_ID=link-authority` で `make check`、関連Race Detector、全体vet・fmt確認、
amd64/arm64静的ビルド、private 0700へ6 ELFを配置する署名bundle/publisher試験が成功した。
network journalはformat 4、compiler contractは `resource-program-native-7`。
arm64は実行していない。`examples/vrrp-additional-identity.yaml`のCLI validateも成功した。

実supervisor/control/network/VRRPと独立Keepalived 2.2.8で6系統が成功した。
IPv4/IPv6 v3 unicast、Health tracking、v2 multicastとPASSに加え、独立WANクライアントで
固定MAC・IPv6 link-local address、RA公開/撤回、両所有者へのWAN TCP/UDPを確認した。
controlとVRRPをSIGSTOPし、同じPID/世代でReady登録されたnetwork workerだけが
元の期限で主VIP/WAN linkをDOWNにして管理addressを撤去することも確認した。
再開後は新しい選挙世代と通信の復旧を要求する。captureでRARPの形式とMACを検査した。
元のVRRP期限・条件付きVIP所有・graceful公開の3系統も成功した。
実VIPのTCP/UDP、元の期限切れ・再適用、worker/control交替と古い世代の拒否、
foreign addressの保持、準備不足での公開待ちと公開後の準備喪失を確認した。

WAN試験の初回TCP timeoutでは、peerのGARP後に退出側のRA撤回が送られる時系列を確認した。
MAC所在通知とpeer側の`garp_extra_if all 1`を用い、bridge学習先の収束を観測してから
steady-state TCP/UDPを試験する。許容する収束待ちは2秒で、無欠落切替の受入ではない。
peerのWAN macvlanは事前作成し、IPはKeepalivedが管理する。両nodeのnative link fencing、
IPv6 clientのdefault-route継続、DHCP/PD/NAT session継続はまだ受け入れていない。

v2誤認証試験ではpeer広告間隔が延びる事象を診断した。変更前binaryとの比較とcaptureでは
宛先MACが正しく、届いた2広告は両方拒否されていた。Keepalived 2.2.8のread dispatcherは
既定設定で不正受信時もsend timerを再設定する。v2 peerへRFC準拠の`lower_prio_no_advert`を
明示した。routerd2の認証条件も、2回の拒否を確認する試験条件も弱めていない。

以前のIPv6 multicast未受信・初回IPv6 VIP TCP timeout・BFD再接続timeoutは原因未確定。
HealthのTCP/HTTP・DNS・raw ICMP実ソケット試験も成功した。
実supervisor/control/network/Healthと独立2 WANのHealth→PBRも成功した。TCP/UDPと
各WANのcaptureで切替・閾値復旧・consumer hold・元の期限とmax_age・古い通知・
各workerと全process treeの再起動・confirmed rollbackを確認し、foreign objectを保持した。
QEMU時計補正・物理NIC/性能/suspendは再実行していない。
writer自身が停止中のlink fencingとIPv4 route撤去は保証しない。
全計画と対象実機の受入は未完了。

続く `RELEASE_ID=bgp-prerequisites` の関連Race Detector・fmt・全体vet・amd64/arm64静的buildと、
実BFD/BGPの失効・再接続、writer再起動、再帰IPv6 routeの期限撤去が成功した。
BFD試験ではcontrol・BGP・BFDを同時に停止し、元のBGP期限が残る間に稼働中writerだけが
BFD受信budget内で経路を撤去した。再帰試験もsupervisor・control・BGPを止め、展開した
learned routeが元の前提routeの期限で消えることを確認した。前提address/linkの期限伝播は
ローカル投影試験で確認し、同じ条件の実daemon試験は未実施。全体`make check`も成功した。
以前のBFD再接続timeoutの直接原因が確定したという意味ではない。

現在のログは `dist/link-authority-final-{check,readiness,build,native}.log`、
`dist/link-authority-readiness-race.log`、`dist/link-authority-health-pbr.log`、
`dist/link-authority-health-sockets.log`、`dist/health-pbr-test/{test.log,wan0.pcap,wan1.pcap}`、
`dist/link-authority-release-built.log`、`dist/vrrp-unicast/*.log`、
`dist/link-authority-original-authority.log`、`dist/bgp-prerequisite-{race,build,bfd-native,check}.log`。
過去の出力との一致を実装条件にしない。

## 続ける実装

1. 初回BFD再接続timeoutを再現・診断し、原因を確定する。成功回数で閉じない。
   BGP/BFD・再帰next-hopの期限は稼働中のnetwork writerが独立して失効させる。
   writer自身が停止した間のlink fencing・IPv4 route失効は保証しない。
   finite linkを持たない条件付きaddress/route自身のfact期限もwriterへ渡す処理は残る。
2. 残るprotocol snapshotの型付き保持、期限切れJSON書換えの削除、条件式の静的計算と、
   retained判定で繰り返すinstance生成・digest計算を整理する。
3. HA-01のHealth以外のtrack・neutral、外部secret参照・peer-aware移行、clusterを実装する。
   二native nodeの追加identity・RA・DHCP/PD/NAT継続を受け入れる。IPv6 multicast未受信と
   起動直後のND/DADを診断し、VRFと対象実機での相互接続を確認する。
   全実装計画・全更新復旧経路・外部engine・配備先・対象実機は未完了。

## 環境

PVEの隔離Linux VM、Ubuntu kernel 6.8、amd64。隔離namespaceかNICなしQEMUを使い、
ホストの管理NIC・route・clockは変更しない。通常build/testは `umask 022`。
rootで動かすharness/binaryは一時的なroot所有0700ディレクトリへ配置する。
home配下の権限は緩めない。対象実機・NIC・性能条件は未指定。
