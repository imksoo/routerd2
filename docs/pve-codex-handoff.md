# routerd2 開発引き継ぎ

2026-10-30 JST。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)、
[試験計画](test-plan.md)、[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

## 現在の候補版: original-egress-native76（2026-10-30 JST）

契約76/journal18/worker ABI5。旧routerdのVRRP設定で有効なIPv4 /24・IPv6 /64等を拒否していた
host-prefix限定を削除し、入力のプレフィックスを保持する。Go VRRPの選出・広告処理は外部化していない。
DHCPv4 engineはdnsmasq、接続状態同期/NTP/PPPoE/Tailscaleにはconntrackd/chronyd/pppd/tailscaledが残り、
少数Goバイナリーによる外部依存削減の当初目標は未達。PID1運用・kernel writerの統一だけで完成とはしない。

実4 AFTRと共通WAN/LAN VMACを持つ署名PID1二台で、大きいnft transactionが既定の送信bufferを越え、
`sendmsg: message too long`となった。既存netlink接続の送信bufferを実際の整列済みbatch byte数へ設定し、
一つの原子的datagramを維持する。受信側の不要なEcho/成功ACK削減とは別の修正で、sysctl変更・分割・再試行は加えない。
旧主系復帰時のIPv6経路には旧設定の共通LAN VMAC/リンクローカルアドレスが必要であり、実設定から取り込んだ。
物理ポートを試験側で先にUPにして作った競合RA経路や、DNS worker指定漏れは試験側の誤りとして解消した。

続く新規IPv4確認では、Health正常なのに実PBR経路/振り分け先が失効していた。
AFTRとHealth/DHCPv4/BGPのfactをsupervisor照会の1秒期限で短縮する処理を除き、元の観測期限を渡す。
実世代照合・元期限切れ・ネットワーク適用ACKは残す。単に待ち時間を延ばした修正ではない。
署名PID1二台、独立ISC/4本のLinux AFTR/BusyBox/実kernel/captureで、主系停止から同じTCP接続・UDP変換を保持し、
新規IPv4、IPv6、取得DNSのUDP/TCP応答、旧主系の待機復帰、PD/DHCP再同期を完走した。
元期限のUTC/BOOTTIME表現を含む最終候補でも同経路が再度終了0。UDP損失0、TCP最長応答5.41秒。captureは一つのTCP sequence spaceと実IPv4-in-IPv6を確認した。
上流DNSは独立UDP peer、同一kernelの隔離構成。実HGW二台/IX2215/全109設定/DoHを合わせたHA、異なる起動時計、
対象物理機器と全34項目の受入とは分ける。VIP /24と公開状態を待つ5 WAN試験は84.61秒で終了0だったが、
この期限修正より前の候補だった。最終候補の同試験も85.86秒で終了0。

03:56:16 JSTに開始した旧候補の自然PD観測は、05:05:11.755の寿命0 RA、05:05:12頃のIPv6既定経路喪失を伴い失敗した。
T1到達前であり、旧05:56/06:07予定は無効。最新修正でこの旧障害を解消したとの判定はまだしていない。
05:27:15開始の4出口・実HGW自然観測は継続中。実貸出のT1は07:27:21 JST。
これは元観測期限修正より前に起動した別のバイナリーで、途中差し替えや中断はしていない。
私有logは`/tmp/routerd-four-sized-batch-native76-natural.log`、identityは`~/.local/state/routerd2-homert-four`。
同じWAN MAC/DUIDを重複使用しない。最終候補のfmt/vet・CGO0両architecture・署名六ELFは終了0。Alpine3.24.3/kernel6.18.59のイメージ生成も終了0。
署名更新/採用後失敗復旧は実行中で未受入。実HGW長期の版とは区別する。

## 直前版: election-authority-native76（2026-10-30 JST）

契約76/journal18/worker ABI5。条件付き静的IPv4アドレスの値と起動条件をSNATとDS-Liteへ引き継ぎ、
内側アドレスは既存B4Address所有へ集約した。Healthを実際の動的トンネル所有者へ依存させた。
元PD/VRRP期限が生きているのにRAを撤回していた原因は、VRRP条件を監視照会の1秒期限で切り詰める処理だった。
元の`election.ExpiresNanos`を保持し、独立ISC/Keepalived/dhcpcdとの短い停止中の通信/RA維持、
元期限後の撤回・復帰が72.37秒で終了0。grace追加や元期限延長はしていない。

4本目のDS-Liteを実設定から取り込むと、nftablesのENOBUFSが再現した。成功ACK削減後も残っていた
未使用のルール全文応答要求`NLM_F_ECHO`を除いた。実kernelの途中操作失敗による全体拒否と
NAT/ACL/DS-Lite等はRace付き24.26秒で終了0。受信buffer増設や受信goroutineは追加していない。
同版の5 WANで、適用完了後も次の通知を待つとHealth通知の連続でVIP公開が進まない問題を再現した。
実適用内容を現在の観測と照合し、変更のない内容では元の適用期限を保ったまま、その場で公開条件を消費する。
FlowRouteの期限延長も他の適用済みgrantと同じ規則で比較し、bucket変更や期限短縮を一致扱いにしない。
最新の実5 WANはVIP公開・120→80→60/60秒復帰抑制・confirmed rollback、元期限内UDP維持、
元期限後の停止・writer再起動後の停止維持、所有権復帰後の同じTCP/UDPが85.69秒で終了0。
LAN6631 packet/kernel drop0。元期限のBOOTTIME値と実停止を照合した。

最新の実HGW試験は、保存済homert03のPD由来4 DS-Lite・Health・120/80群・SNAT/zone/VIPを使用した。
独立BusyBox/dhcpcd端末の自動DHCPv4/SLAAC/DHCPv6/DNS、UDP/TCP DNS・条件付き転送/DoHと両IP HTTPS、
1.5秒control停止中のIPv6/DNS維持・寿命0 RAなし、3 AFTR個別選択を確認した。
優先3出口の遮断で第4出口/mark275へ切替え、実IP-in-IPv6 packetと通信を確認した。
設定60秒の復帰抑制中も通信を維持し、59.06秒で優先120群へ戻った。その後90秒/9巡と両端末の自然DHCPv4更新、
PD強制終了1.62秒・通常交換5.97秒・全体再起動12.73秒での復帰、DUID/IAID/prefix保持、
LAN直接DNS返信/WAN側漏出なし・最終RA撤去/上流Releaseなしまで終了0。
同じHGWを使う複数AFTRであり、独立物理WAN・継続TCP・同構成二台の受入ではない。
名前が`ds-lite-ra`でも、保存済homert02/03の実宣言はPD由来だった。名前から別のRA送信元経路を作らない。
試験値に合わせてアドレス/ifnameとVRRP参照を置き換え、生成物のownerRefsを除いた構成であり、109宣言全体ではない。

03:56:16 JST開始の7800秒自然PD観測は05:05に失敗した。T1前であり旧05:56:22予定は無効。
これはVRRP元期限修正後、Echo削減/即時VIP公開より前の別バイナリであり、最新版全体の長期受入とは区別する。
旧native75の両長期観測は失敗し、旧予定は無効。私有ログは
`/tmp/routerd-election-authority-native76-hgw-natural.log`、最新4出口は
`/tmp/routerd-homert-four-immediate-admission-native76-live.log`。
3出口identityは`~/.local/state/routerd2-homert-multi`、4出口は`~/.local/state/routerd2-homert-four`に保持する。
同じMAC/DUIDを並行起動しない。私有source/policy/program/uplinkは`/tmp/routerd-homert-{multi,four}-native76-*`。
最終全make check/Race/vet、CGO0 amd64/arm64 build、署名六ELF検査は終了0。
自然PD更新・長期安定性、IX2215、同構成HA/管理更新復旧、k8s/pve、Cloud SAM、全34項目と対象実機は未完了。
現行homertの再取得は未完了で保存済snapshotを使用した。診断専用のvalidation error出力は削除した。

## 直前版: conditional-nat-native75（2026-10-30 JST）

契約75/journal18/worker ABI5。外部VRRP daemonは追加せず、Goの選出・広告と単一network writerを維持した。
実設定由来のWAN zone参照は旧routerdのsuffix解決に合わせ、条件付きNAT44Ruleを独立した宣言から
同じfirewall transactionへ接続した。古いMasquerade型・描画・validatorを削除し、NATRuleへ一本化した。
NATの元条件期限をwriterが独立して失効させる。既存のconntrackは表の置換で消さない。

ルーター発IPv6のWAN規則がLAN宛てDNS応答までtable120へ送る設定を修正した。
WAN lookupの直前にmainの具体経路だけを引く規則（suppress_prefix_length=0）を置き、同じ所有期限で撤去する。
独立radvd/dnsmasqの3構成で取得送信元・DNS/Health/TCP/UDP・所有者停止中の失効・復帰が終了0。

実5 WANのzone policyでnftables適用後にENOBUFSを再現した。成功ACKを読み始める前に受信queueがあふれ、
表だけが作成され所有記録が残らなかった。並行受信でも再現したため、その案は破棄した。
Linuxのbatch処理を確認し、最後の操作だけに成功ACKを要求する小さい修正にした。操作失敗の通知は維持する。
実kernelで途中の存在しないchainへの操作がENOENTとなり、表全体が残らないことを確認した。
独立WAN端末から実listenerへのSYNを採取して着信拒否を確認し、VIP gateway/条件付きNAT、
120→80→60、60秒復帰抑制、confirmed rollback、所有期限/writer交換と同じTCP/UDPの復旧が77.77秒で終了0。
LAN 5947 packet/kernel drop0。既存のkernel NAT/ACL/DS-Lite等もRace付き26.14秒で終了0。

実HGWでは同じ版・旧設定由来の単一DS-Lite/PD/DNSへWAN zones・条件付きNAT・準備条件/VIPを組み合わせた。
90秒、両端末の自動DHCP/DNS、IPv4/IPv6 HTTPS・UDP/TCP DNS、DoH/条件付き転送、PD交換と
全体再起動後の復帰は通過したが、その実行全体は採取ファイル不備で終了1。
採取開始と端末SLAAC/DAD・既定経路の準備待ちを直した1巡の再実行は終了0。
PD強制終了後1.64秒、通常交換8.68秒、全体再起動9.61秒で通信が復帰し、DUID/IAID/prefix保持、
LAN向けIPv6 DNSのmain経路とWAN送信側の該当packet0、最終RA寿命0/撤去・上流Releaseなしを確認した。
途中の失敗には、短いAFTR停止状態のポーリング要求、Release必須という誤った追加条件、
交換中のgeneration欠落を扱えない試験も含まれた。これらは修正し、通信の不成立と混同しない。
この構成は全homert設定・複数WAN・二台を同じ実回線に接続した受入ではない。

全make check（Race/vet）、CGO0 Linux amd64/arm64 build、署名六ELF検査は終了0。
この版でAPK/署名PID1二台を再受入したとは扱わない。直前native74の実Alpine3.24.3/kernel6.18.59
APK/PID1/2 NICではPD期限を1秒の監視照会期限で切り詰めるRA再撤回を修正し、control交換後20秒以上の
両IP通信とISC18秒リース更新・再撤回なしが終了0だった。native74のHGW長期は02:18頃のIPv6 UDP DNS timeoutで
終了1、自然更新前に停止した。03:08開始のnative75観測も03:27頃、1096秒/103巡後の端末IPv6非推奨化で終了1。旧05:08/05:19予定は無効。
未変更native75を03:36:58に再構築・再観測し、03:45:08.028の寿命0 RA、直後の両端末の非推奨化/既定経路削除、
03:45:10.774の試験停止を記録した。後片付け前の撤回であり、試験の例外だけが原因ではない。
PD Bound・VRRP ACTIVE/Readyは元期限内だった。VRRP factを照会の1秒期限で切り詰め、RA ActiveUntilへ渡す経路を
修正中。native76で元のelection.ExpiresNanosを保持する。元期限内のcontrol短期停止・実期限切れの撤回と再観測は未判定。
私有logは`/tmp/routerd-host-lan-native75-hgw-natural.log`、fixtureは
`/tmp/routerd-homert-single-native75-uplink.py`と同program/source/policy。実回線値を公開しない。
永続identity/leaseは`~/.local/state/routerd2-homert-single`。同じWAN MAC/DUIDを重複使用しない。

全34項目の期限01:56:38 JSTは未達。次は長期観測を止めず同じ構成の二台同期・管理更新復旧と
全homert差分を進め、その後k8s-rt/pve-rt、Cloud SAMへ進む。所要工数・不足する対象環境は未確定。
自然PD更新、ONU直結、対象NIC/ARM機器の資格と全体完成を、この部分成功から推定しない。

## 旧BGP設定と独立FRR（2026-10-29、bgp-import-native72）

旧pkg/controller/bgpのpolicy継承、広告prefix集合、peerとgraceful restartを既存GoBGP workerへ接続した。
redistributeの許可prefixも旧実装では静的広告であり、kernelの接続経路を観測した結果とは区別する。
VRF/table・IPv6 family・RR情報の型変換はあるが、今回の実相互接続はIPv4・一対向・GR無効である。
BFD参照・外部password参照・peer別community変換は未接続の宣言を拒否する。新外部BGP daemonは追加していない。

公開値の旧Router/BGPRouter/BGPPeerを実CLIで取り込み、製品supervisor/control/network/BGPと独立FRRで
LAN広告、FRRから学習した経路のkernel反映、LAN端末のTCP/UDP、停止時の広告撤回、製品全体再起動後の
再学習とTCP/UDPが終了0。WAN/LAN captureも照合した。標準TCP179で試している。
最初は対向bgpdだけでZebraなしだったため、FRRがNHT待ちでOPENを拒否した。送信OPENと対向状態を観測し、
通常のfrr accountと専用一時dirでZebraを対向namespaceへ追加した。bgpd -nで対向のkernel route書込みを止める。
FRR/Zebraは試験対向だけであり配布製品へ含めない。license/noticeは保持した。

全make check（Race/vet）、CGO_ENABLED=0のLinux amd64/arm64 build・署名六ELFが終了0。
compiler contract72/journal17。全homertと他環境を組み合わせた受入、IPv6/VRF/RR/GRの取込設定の実相互接続は残る。
次は同じ単体構成へ主従条件・複数WAN・管理を接続する。全34項目は未完了。

## 宣言済みPD→DS-Liteと再起動（2026-10-29、prefsrc-order-native71）

旧設定のdeclared delegatedAddressは、追加ds_lite policyがなくても、宣言済みPD割当と既存AFTR workerへ
直接接続する。既定はfirst/照会2秒/再試行5秒。hostname/AFTR情報を使う場合だけ同じWANのPD DNSを使い、
literal AFTRには不要なDNS依存を加えない。既存の元期限・DAD確認・唯一のnetwork writerを使う。
DNS listenerが明示したsources順を起動時に準備するよう修正し、照会ごとの許可map作成と全sourceコピーを除いた。
旧DNSも同じ許可集合処理だったが、明示順の条件付きsourceより全域sourceが先に選ばれる実失敗を修正した。

実設定からPD/Information、subnet1のLAN割当とWANの::21/128、MTU1454のDS-Lite一本、LAN RA/DHCPv6を
取り出し、実CLIで単体用native設定へ変換した。単体化ではwhenを外し、VMACの代わりに二つの既存試験linkを
明示管理する。試験用の固有MAC/DUID、DoH/条件付きDNS、DHCPv4、NAT/MSS/firewallとhost policyを使用する。
取込み結果のservice/割当名だけを既存通信fixtureの役割名へ結び、protocol値は保持した。全homert設定ではない。
初回に元の固定DUIDを残していた試験設定を修正した。元の本番identityを試験で共有してはならない。

途中、通常PD再起動後のLAN IPv6 DNS一照会がtimeoutし終了1。次の短期観測では通常再起動が通ったが、
全体再起動でtable120/metric50が所有権競合となった。Linuxが送信元アドレスの削除時に経路を残してsrcだけ消す
動作を隔離namespaceで再現した。不要な経路をアドレスより先に撤去し、アドレス/link変更後の経路を再観測してから
更新する順序へ修正した。所有権の拒否条件は緩めていない。

同版の実HGW・独立二端末で90秒/9巡の取得DNS・条件付きsource障害分離/復帰・IPv4/IPv6 HTTPSが終了0。
PD SIGKILLから1.51秒、通常再起動から3.83秒、製品全体再起動から3.9秒で通信が戻り、DUID/IAID/prefixを保持した。
停止時はzero-lifetime RA・kernel撤去・upstream Releaseなしを確認した。先の通常再起動DNS失敗の原因は未確定で、
修正版90秒では再現しなかった範囲に留める。別の同版5秒観測/再起動試験でも終了0で、
全体再起動後に自動LAN端末→公衆IPv4/IPv6 UDP DNSを追加確認した（全体復帰3.94秒）。
自然PD更新0回、長期安定性、全homertの主従/複数WAN/管理、対象物理NIC、直接ONUは未受入。

固定/学習/暗黙host gatewayの3試験へ全体再起動を加え、独立radvd/dnsmasqで全て終了0。
全make check（Race/vet）、関連4package Race、両CGO0 Linux buildと署名六ELFも終了0。
compiler contract71/journal17。次は同じ単体構成へ残るBGP/主従/複数WAN/管理を接続する。

## 明示RAのない旧WANの受信口（2026-10-29、implicit-ra-native70）

旧hostdeps.DerivedSysctlResourcesForOSのRA受信対象を直接使い、PD/Informationと、LAN配布に使わない
delegated interfaceへ既存RA workerを接続する。IPv6 host policyのinterfaceも受信口を持つ。
明示RA policyが同じinterfaceを所有している場合はそれを優先し、socketを重複しない。
暗黙受信口の新しいnative intentはSLAAC/default/on-link/RIO/DNS/MTU/hop-limit、main table/metric1024。
旧NetworkManagerの全既定値との同一性は主張しない。kernel RA/autoconfを再び有効にはしない。

明示RAなし/補足policyなしの旧host設定を実CLIで取り込み、独立radvd/dnsmasqと通常DAD、
IPv6 TCP/UDP、静的DNS upstream経由の端末照会、Health、元期限撤去・IPv4継続・復帰が終了0。
元の固定/学習gateway・RA学習DNSの両試験も終了0。PD試験の手動RA追加は製品生成のRAと競合したため
除去し、その受信口を使うISC対向PD/IA_NA/実DAD重複/端末通信・rollback/worker交換も終了0。
同版の旧設定取込み条件付きLAN、独立端末のRA/SLAAC/DHCPv6・取得DNS・期限と復帰も終了0。
そのPD試験はIA_NA観測のためSLAAC無効化を明示する。homertそのものの一貫した構成の合格ではない。

全make check（Race/vet）と両CGO0 build・署名六ELF検査が終了0。compiler contract70/journal17。
全homert取込み・単体構成の通し通信と二台は未完了。長期HGWのdoh-reset-native69は15:24 UTC頃に終了1となった。上の長期結果と未確定事項を参照する。

## 実HGWのDoH切断修正（2026-10-29、doh-reset-native69）

native-67で再利用TLSへの対向RST→LAN SERVFAILを観測した。旧routerdも同じ単発POSTであり、
同一上流の再利用接続で起きたtransport errorだけ、元の照会context/期限内に一度再送する。
新規接続の失敗、HTTP status、DNS rcodeはこの再送対象にしない。別情報源への迂回は追加していない。
実TLSのHTTP/1.1・HTTP/2でRSTと一度の再送を再現し、DNS全packageのRaceが終了0。

修正版の製品daemonと実HGW・独立LAN clientで、既存DoH接続の送信portだけを隔離namespaceの
一時nft reject tcp resetへ指定した。counter=1、端末の新規照会が83msでNXDOMAIN/正常応答、
別portでのTLS再接続と後続TCP/UDP DNSを確認し、一時ruleは撤去した。DNS worker交換はしていない。
条件付き情報源の分離・停止中SERVFAIL・復旧も同版の実HGW端末で成功した。
native/旧設定DHCP-DNS実daemon、全vet、両CGO0 Linux build、署名六ELF検査も終了0。
この局所切断の回復は、上流・実回線を含む長期無障害の証明ではない。

同版の実HGW長期観測は14:50 UTC頃開始したが、15:24 UTC頃、Tailscale peer発の公衆IPv6 UDP DNS
一照会が3秒以内に返らず終了1。直前1954秒/175巡でready、最後の照会は出口WANから送出され、
対応する受信packetを確認できない。直前照会はWAN受信→逆NAT→peer受信まで通っていた。
DoH再発・製品が返答を落とした結果とは判定しないが、長期合格でもない。原因は未確定。
自然PD更新前に終了し、旧16:50更新/16:52頃終了の見込みは無効。保存DUID/MACは維持するが、
再取得でT1が約2時間なら残る期限内に自然更新を観測できない。全34項目の期限16:56:38 UTCは変更しない。

## 旧経路参照に従うHealth（2026-10-29、native-69）

旧routerdのResolveSpecForResourceへ、境界でdecodeしたRouterを直接渡し、EgressRoutePolicy由来の
監視interface・markを再利用した。同じRouterをroutingDefaultsにも渡す。DS-Lite上の監視は
既存tunnel所有者のactive条件に接続し、生成前のinterfaceを無条件に作成しない。

公開fixtureのHealth interfaceと補足policyを除去し、実CLI・製品worker・独立5 WANの
TCP/UDP分散、障害切替、60秒抑制、既存接続維持、rollback、期限切れとworker交換が終了0。
既存Health/PBR・ECMPと固定/RA学習host routeの実通信も終了0。
全make check/Race/vet、CGO_ENABLED=0のLinux amd64/arm64 build、六ELFの署名検査が終了0。
compiler contract69/journal17。homertの固定監視先に必要な解決であり、動的target discoveryや
全homert設定の取込み・二台運用まで成立したとは扱わない。次は暗黙RA対象と全単体構成を接続する。

## 旧ルーター設定からのOS前提の導出（2026-10-29、native-68）

旧sysctlprofile.NeedsForwardingを直接再利用し、routing構成からIPv4/IPv6転送とrp_filter=0を
native設定へ明示する。明示した値を優先し、global rp_filterの指定はall/defaultと管理対象interfaceへ
既存journalで適用・復元する。個別interfaceの明示値はその選択値を上書きする。
後から生成するtunnelも同じ管理経路を使い、ほかのinterfaceは変更しない。
OSのRA受信を再度有効にせず、既存Go RA workerと二重所有にしない。

公開値の旧EgressRoutePolicy fixtureから、試験用Sysctlと追加の転送resourceを除去した。
実CLI・製品worker・独立5 WANのTCP/UDP分散/切替/60秒/旧接続維持/rollback/元期限が終了0。
同版の固定/RA学習host gatewayのIPv6・DNS・Health・IPv4継続、native/旧設定取込みの
独立DHCP/DNS clientもすべて終了0。全make check（Race・vet）も終了0。
Linux amd64/arm64のCGO_ENABLED=0 build、六ELFの署名catalog/publisher/stageも終了0。compiler contract68、journal17。全router-linuxの性能調整値を
移植したとは主張しない。Healthの経路参照はnative-69で接続した。
暗黙RA対象と全homert単体構成は未完了。native-67の実HGW試験は下記のとおり失敗した。
全homert二台・k8s/pve/SAM等の全34項目は未完了である。

## IPv6 host経路と実HGW継続観測（2026-10-29、native-67）

`host-route-native67` は旧routerdのIPv6 host policyを読み、WANの適用済み・DAD完了済みGUAを
preferred sourceへ指定し、`iif lo` と専用tableへ接続した。host source不在はそのresourceだけの
未準備として扱い、IPv4の適用を続ける。RA/PDの元のpreferred/valid期限を経路とruleへ渡す。
静的gatewayに加え、明示したRA clientの現在のdefault routerを使う`router_from`もある。
PD由来DS-Liteの外側送信元は、宣言した先行ruleでmain tableへ送る。新workerは追加していない。

独立radvd/dnsmasqと実CLI・製品workerで、homert相当の固定gatewayと、RA学習gatewayの両方が終了0。
自動取得値・通常DADの実送信元で、ルーター自身のIPv6 TCP/UDP・学習DNSのUDP/TCP・Healthが成立した。
観測元を停止しても元期限でhost route/ruleが撤去され、IPv4 TCPは継続し、RA再開後に通信が戻った。
途中で期限付きnetlink送信がRTA_PREFSRCを落としていた不足を検出して修正した。
peerのradvd最短広告間隔と、IPv4専用だったcapture照合も修正した。製品のDADは無効にしていない。

実行版はcompiler contract67/network journal17。全make check（Race・vet）、既存RA/RA-AFTR実通信は終了0。
最後のrouter_from追加後も、関連4packageのRace、全vet、両gatewayの実通信、amd64/arm64静的buildと
六ELFの署名catalog/publisher/stage検査が終了0。ARM実行と対象NICの資格は示していない。

同版の実HGW/二つの自動LAN client/Headscale・公式Tailscaleを14:06 UTCから実行したが、
14:35:46 UTC、DoHの再利用TLS接続が対向RSTを受け、LAN端末のDNSがSERVFAILとなり終了1。
直前まで1687秒/151巡の通信が成立したが、継続安定性は不合格である。
routerd SHA-256は`aa659481337c2ab880dbb0630eb6e5c743c4c636027fc8e67ddb70d4ec64b969`。
初回のDHCPv4、RA連動DHCPv6/SLAAC、IPv4/IPv6 HTTPS・学習DNS・条件付きDNS・Tailscale通信が成立し、
実kernelでWAN RA由来のhost source/table120と、PD DS-Lite送信元のmain迂回も照合している。
初期T1残7179秒だったが、自然更新前に終了した。従来の16:16 UTC終了予定は無効。
その後の回復不能を示した結果ではない。旧DNSも同じ単発POSTで、この障害の修正を進めている。
このHGW構成は一つのWAN MAC、固定対向網と追加IPv6 NATを含む。全homert二台・直接ONU・対象NICとは分ける。
次は旧hostdepsのrouting用OS設定を明示native intentへ取り込み、動的Healthと全homert単体を接続する。
全34項目と期限内完成は未達で、k8s/pve/SAM・配備/更新/復旧の全体受入も残る。

## 接続マークを使うIPv4候補グループ（2026-10-29、native-66）

`egress-policy-native66` は旧routerdの候補順位と256バケットのrendezvous計算を取り込み、
旧EgressRoutePolicyのIPv4 priority/hash・mark用tableを既存resource/control/networkへ接続した。
固定の分散計算は設定時に準備する。新規接続は選出グループへ送り、既存接続はconntrack markで
元の経路表を使う。Health喪失で旧接続の経路を消さず、基盤トンネルの所有期限とは分ける。
別の外部daemonや選択管理processは追加していない。VRRPは引き続きGo実装である。

公開値の旧設定を実CLIで取り込み、Linux kernelと製品supervisor/control/network/Health、
独立した5 WAN peerとLANで71.76秒の試験が終了0。3出口への送信元別TCP/UDP分散、
重み120→80→60、障害時は60秒抑制を待たない切替、旧TCP/UDPの維持、60秒後の上位復帰を確認した。
confirmed rollback、control/Health停止中の元期限で新規通信を遮断、network worker交換後も
期限を延ばさないことと既存TCP/UDP維持、再開後の新規通信復旧まで通り、5 WANのcaptureを照合した。
実WAN断ではなくHealth用受信口を停止した試験であり、異なる公衆WANの同一TCP維持は示していない。

初回はSYN-ACKが線上へ返ってもrp_filterで接続が成立しなかった。all=0だけでは既存interfaceの
値が残るため、明示したinterface値を既存の所有・復元journalで適用する。試験は全前提を旧Sysctlへ
記載し、手動sysctl修復は使わない。旧hostdeps.derivedInterfaceSysctlsとrouter-linux profileも読んだ。
旧routerdはrouting時のall/default=0とtunnelごとの0を暗黙に補うが、その全派生profileの取込みは残る。
途中のcapture照合は別fixtureのポートを参照し、次の実行はSysctlのapiVersionを誤った。
いずれも訂正後に同じ通信条件で完走した。旧試作readerは加えず、compiler contract66/network journal16とした。

関連する既存Health/PBRとkernel ECMPの実試験、全make check（全Race・vetを含む）、
Linux amd64/arm64のCGO_ENABLED=0 build、最終六ELFの署名catalog/publisher/stage検査はすべて終了0。
次はルーター自身のIPv6 hostTraffic/preferred source、動的Health target、暗黙OS設定の明示化と
homert単体全構成を接続する。今回の固定IPv4・模擬WANはPD/DS-Lite実回線の受入に数えない。
全homert二台、k8s/pve、SAM、配備・更新・復旧の全34項目は未完了である。

## RAで取得する自己DNS（native-65）

RAのRDNSSも、DHCPv6と同じ実DNS待受の観測に接続した。観測の2秒期限は広告許可であり、
正のRDNSS寿命は元PD期限で決める。期限切れではDNSだけを撤回し、既存の最小広告間隔を保つ。
更新時に許可期限だけが変わってもRAの設定世代を増やさない。DNS起動入力のアドレスは
待受確認で絞らず、循環待ちを作らない。両consumerの設定参照と観測解釈を共有した。

独立dhcpcd10.0.6をDHCPv6無効のRA専用clientとして起動し、標準resolver hookが私有ファイルへ
書いたDNSを使用した。製品process・ISC・keepalived・dnsmasq・Linux端末で70.50秒、終了0。
RA由来DNSへの最初のUDP/TCP照会、PD喪失時のresolverからの撤去、再取得・名前解決・
IPv6 TCP/UDP、VIP DNS、同じ固定DNS TCPの維持まで成立した。初回失敗は試験側がdhcpcdの
変数名を誤って固定していた。送出RAの55秒RDNSSと標準hookを照合し、独自解釈を削除した。
関連RA/runtimeplanのRace、全make check（Go/Python・全Race・vet）、
既存PDとRA端末のMake再実行、両architecture CGO_ENABLED=0 build、実6 ELFの署名検査は終了0。

同版の実HGW＋独立2端末＋Headscale/公式Tailscaleは75秒/7巡、全体再起動まで終了0。
自動DHCP/RA/配布DNS、条件付きDNS/DoH、IPv4/IPv6 HTTPS、双方向TCP/UDPとexitを確認した。
underlay固定と追加IPv6 NATの差は残り、全homert・直接ONU・NIC/ARM資格とは区別する。

別に`d5e87bd-hgw-native61`の7800秒/700巡が終了0。自然PD Renew/Reply1回、LAN両clientの自然更新、
PD強制交換1.38秒・通常交換2.91秒・全体再起動4.17秒で通信が復旧した。
旧版の長期観測であり、native-65の長期安定性には置き換えない。以前の外部IPv6 UDP DNS失敗の
原因はこの成功で解消扱いにしない。全34項目と対象環境での利用可能性は未達。

## 動的DNS待受・自己DNS配布と端末復旧（2026-10-29、native-64）

旧routerdのDNS controller/engineの参照展開と待受差分処理を照合した。
Interface、VRRP VirtualAddress、IPv6DelegatedAddress由来のDNS待受と、DNSUpstreamの
addressFrom/bootstrapFromを既存Go resolverへ接続した。Interfaceはworkerが実kernelを観測し、
PDは適用済み・DAD完了後のアドレス、VIPは適用済みの保持者・世代・元期限だけを渡す。
追加/撤去したソケットだけを変更し、固定待受のTCP接続と上流poolを維持する。
ソケットに論理listenerの転送元選択と元期限を結び付け、期限後の問い合わせも拒否する。
新しい外部DNS daemonや独立した状態管理機構は追加していない。

実CLIで公開値の旧設定を取り込み、製品supervisor/control/network/workerと独立ISC・
keepalived・dnsmasq・BusyBox・Linux端末で67.55秒の試験が終了0。
自動SLAAC/default/配布DNS、IPv6 UDP/TCP DNSとTCP/UDP、IPv4 VIP宛てDNS、
主従変更・control停止の元期限撤回/復帰、PD失効時の学習転送先/hostname bootstrapのSERVFAIL、
同じ固定DNS TCP接続を保ったPD再取得と端末通信復旧を確認した。固定TCPのSYNはcaptureで1回。

PD再取得時のDNSなし応答を実際に検出し、自己の委譲アドレスをDNSに指定するDHCPv6は
DADで使えるまで配布許可を渡さないよう修正した。線上ではPD Reply11:48:33.591、
router自身のDAD NS33.743、Information-request33.863/34.675、DNSを含むReply35.687だった。
無応答の間にDNSなしのReplyは送らず、独立clientの通常再送で取得した。
試験の修正: IPv6 defaultの表記とDAD待ち、選出完了と実DNS待受の差、TCP既定128照会の過剰消費、
古いLAN経路と新しいWAN取得を区別した。製品の期限・TCP上限・client再送数は延長していない。

全make check（Go/Python・全Race・vet）は終了0。DAD修正後の関連5packageのRace、
Linux amd64/arm64 CGO_ENABLED=0 buildとvetも終了0。最後のMake再実行は既存PD試験が成功し、新しい実経路はPD再取得後の
配布DNSへの最初の照会が接続拒否で終了1。DAD後とDNSソケット準備完了の差が残り、
native-63の単発成功を安定受入とはしない。

native-64では、DNS workerが実際のUDP/TCP待受を既存worker通知へ報告する。
DHCPv6は適用済み・DAD完了と実待受の両方を要し、配布許可は元PD期限と観測期限を超えない。
観測は2秒で失効し、古い通知の再送では延ばさない。配信が失敗しても固定DNSは動作を続け、
制御側は過去の観測を失効させる。静的待受のアドレス/ソケット情報は設定時に準備する。
新台帳・ファイル再読込・新daemonは追加していない。

この版の絞った実試験は70.04秒で終了0。さらに最終`make check-dhcp6-ra-pd check-dhcp-dns`で
既存PD、新しい取込/失効/再取得、native/取込DHCP-DNSの全4試験が終了0。
再取得後に配布されたDNSへの最初のUDP/TCP照会とIPv6 TCP/UDP、VIP宛てIPv4 DNSが成功し、
同じ固定DNS TCPを維持した。絞った試験のcaptureでも固定TCPのSYNは1回だった。
新しい通知の配信失敗時にも、既存のcontrol/supervisor停止試験で固定TCPが継続した。
全`make check`（Go/Python・全Race・vet）、関連5packageのRace、両architectureのCGO_ENABLED=0 build、
実6 ELFを使う署名catalog/publisher/stageのRace検査も終了0。

これはhomert設定の当該構造を公開値で確認した範囲であり、全homert二台・実HGW最新版・
ONU直結・対象NIC・ARM実行の受入ではない。全34項目と実環境での利用可能性は未達。

旧routerdのorigin/main `e3b49dbb`（2026-10-27）も取得し、baselineとのpkg/cmd差分を確認した。
追加は配布conntrackdのsystemd unit競合防止とそのartifact出力だった。
routerd2はPID 1が同梱engineを直接所有するため、そのsystemd処理は導入しない。
全homertの残る構成、k8s/pve/SAM、全34項目と配備・更新・復旧のゴールは継続中。

## 条件付きLAN配布（2026-10-29、native-62）

RAをPD内の暗黙生成から独立serviceへ変更した。`ra.from.service/delegation`で選択し、
RA自身のwhenと配布設定を持つ。旧prototypeのnested advertise形式は削除した。
旧routerdのchain/dnsmasq描画と現homert設定を参照し、明示RAのM/O・MTU・優先度・寿命・
PD由来RDNSSとDNSZone由来DNSSL、stateless DHCPv6の独立when/Applied依存・logDHCPを取込へ接続した。
明示RAがない旧DHCPv6Serverの暗黙RAは、取込境界で一つの明示serviceへ変換する。

実Linux名前空間の製品supervisor/control/network/workerと独立ISC4.4.3-P1・keepalived2.2.8・
Linux SLAAC/BusyBox/digで、実CLI取込から自動SLAAC/default/DNS、UDP/TCP DNS、IPv6 TCP/UDP、
主従変更時のRA/DHCPv6停止と復帰が成立した。主従変更中もPDはboundを維持した。
任意名のRA消費者を実際の参照で撤回し、旧生成名を推測しない。
`active_until`は元の配布許可期限であり、PD/パケット寿命とは別に保持する。
7200秒の設定に対し線上の正のrouter lifetimeが1秒より長いこと、controlだけのSIGSTOPで
2秒以内のゼロRA、停止1秒以降の正のRAなし、SIGCONT後の端末通信復旧を確認した。
これは通常DADを使う隔離試験であり、homert本番全体・ONU直結・対象NICの受入ではない。

`make check-dhcp6-ra-pd`は既存PD/IA_NA/RA/DUID/期限試験と新しい取込試験の両方が終了0。
最後の線上寿命の判定を含む絞った再実行も終了0。WAN/LAN captureを確認した。
同版の`make check-ha-pd`も署名二台PID 1・独立ISC/Linux端末で終了0。
同じTCP、双方向PD/lease/conntrack引継ぎ、元期限と復旧が成立し、UDP損失7、最長応答3.504秒だった。
`make check`（Go/Python・全Race Detector・vet）と関連7packageのRace検査、
Linux amd64/arm64のCGO_ENABLED=0製品buildも終了0。ARM実行は未検証。

native-62の当初試験は静的DNS待受だった。動的参照は上記native-63で接続し、別に確認した。
ルートmainの`d5e87bd-hgw-native61`は変更から分離して7800秒の実HGW/Tailscale観測を継続中。
自然PD更新・新版native-62の実HGW受入とは区別する。

## 現在の成立範囲と残作業（2026-10-29 09:47 UTC）

この配布試験で実行した版は`authority-history-native61`（worker ABI5、network journal15）。

追試`dbf4a6e-hgw-native61`の7800秒観測は、自然PD更新前の10:05 UTCに終了2。
最後の定期正常記録は609秒/55巡。Tailscale対向から外部IPv6 DNSへのUDP照会で失敗した。
標準出力が捨てられており、製品・外部経路・試験の原因は未確定。終了値/出力と
同じ照会のpeer/WAN capture・経路・接続追跡・NATを失敗時に採る。長期安定化とは扱わない。

- **実装**: 認証で取得済みのsupervisor情報を共有し、問い合わせ開始時のBOOTTIMEから1秒で失効する。
  遅い応答やlock待ちで更新せず、古い照会で新しい観測を上書きしない。確定成功の直前にも期限を確認する。
  NTP入力は要求workerの実世代・PID・digestへ結び付け、旧世代や起動待ちの登録を渡さない。
  履歴の再デコード・圧縮直後の再展開を除き、使用量は一つのtransactionで一括更新する。
  保存上限・原子性・読出しの整合性検査は保持し、診断用計測は削除した。
- **実通信**: 同版の実APK/PID 1・512MiB/2 CPU・ext4・独立RP-PPPoE/ISC/chronyd/Linux端末で終了0。
  保存13.29秒、再起動65.24秒。再起動後63.56秒/9巡のIPv4/IPv6 TCP/UDP・DNS・NTP・管理、
  進行するHealthと履歴（tip202/archive200）が成立した。全9巡healthy=true、履歴破棄0、
  管理4照会の合計最大2.79秒、NTP世代1。DUID・SSH identity・構成・MTU・driftも保持した。
- **実回線**: 同版の実HGW＋独立2端末＋Headscale/公式Tailscaleは75秒/7巡と全体再起動まで終了0。
  自動取得、学習DNS UDP/TCP、条件付きDNS/DoH、IPv4/IPv6 HTTPS、双方向TCP/UDPとexitが復旧した。
  全体再起動の復旧5.04秒。両homertの現設定にTailscaleNode自身のwhenは無い。
- **補助検査**: make check、履歴変更後のevents/control Race Detector、全vet、両architectureの静的build、
  実6 ELFの署名catalog/publisher/stage試験が終了0。実二WANHealth/PBRの交換・旧通知・期限・rollbackも終了0。
  `archive-batch-native61`の取込試験は元PD期限・再取得・VRRP解除/復帰・同じchronydの起動一回、
  独立LAN NTP/IPv4/IPv6 TCP/UDPとcaptureまで終了0。停止supervisor/control中の元期限失効も別実行で終了0。
- **未受入**: 今回も起動直後に履歴整理の2秒期限超過があり、継続8巡目にはHealth履歴の1秒配送超過が一度あった。
  次巡では回復し破棄0だが、全体安定化ではない。以前のDHCP/NTP起動10秒超過も解消扱いにしない。
  HGWの自然PD更新は今回0回。固定対向網・追加IPv6 NATとhomertとの差、全homert二台、k8s/pve/SAM、
  ONU直結・対象NIC・ARM実行・長期受入は残る。期限内に全34項目を完了する見通しは立っていない。

準備上の失敗: `archive-batch`のVMは書出し完了前のinitramfsコピーで起動不能となった。
完成後のgzip/hashは正常であり、製品不具合とは扱わない。イメージbuildの終了0を待ってから実行する。
`release-built-test`は`RELEASE_BUILT_DIR=dist/bin`を指定する（architectureの子directoryではない）。
入力の親directoryは他userが書込めないこと。旧計測image5個・計1.5GiBを削除した。

以下は機能別の成立範囲。旧版の成功を最新版の受入へ置き換えない。

- main `935369d`: native-58/network journal15。PPP default経路は実unitの寿命に従い、条件付きPolicyRuleは
  元のBOOTTIME期限をwriterが独立して撤去する。PAP/CHAP・二WANのHealth/PBR・元期限・旧通知409・
  control停止・PPP強制終了・AC停止/認証失敗/復旧と、同じDNS TCP接続の保持が成立した。PDなしの試験を
  両系PD復旧の合格にはしない。隔離した変更単位のmake check/Race/vet/Linux amd64+arm64 buildは終了0。
- 署名SSH更新を製品経路へ接続した。127MBの転送、中断後の通信、175.15秒の準備中の状態照会・DNS・
  両family TCP/UDP、新版/revision3の採用・正常確定・DUID/SSH identity保持が成立。90秒の判定期限は不変。
  prepareは重い検証前にcontrol全体のlockを解放する。確定/失敗記録は検証済み選択情報をatomic更新し、
  判定期限内に全payloadを再hashする重複を除いた。実際の稼働世代・採用記録・元期限を確認する。
- 整合したext4上の候補ELFを1 byte破損させ、実行前拒否・既知の正常版への復帰・失敗対象記録・
  自動DNS・両family TCP/UDP・DUID/SSH・最後の正常電源断まで終了0。`corrupt-recovery`はこの残工程に絞る。
  通常更新後の確定版へ戻る元の実行も通信まで成立したが、未採用候補でもrevision増加を要求する試験の誤りで
  終了2だった。候補を実行前拒否した場合、既に採用済みの設定とrevisionは変わらない。期待を修正した。
  PID 1のsync後もext4 journal再生を要し得るため、offline編集前にjournalのみ再生し、編集前後のread-only
  fsckを要求する。未再生journalへdebugfsを使った以前のfilesystem破損を製品fallbackの失敗には数えない。
- 全4engine（PPP/DHCP/conntrack/Tailscale）のAPK/imageを固定同梱し、native-58のAlpineを構築した。
  実QEMU/PID 1起動は34.3秒で終了0。全engineの同時通信・arm64実行・対象実機・kernel/rootfs更新復旧は未受入。
  コアはCGO無効で両architectureを構築。署名試験の独立対向はRP-PPPoE/pppd・ISC DHCP・dhcpcdと実packet。
- `time-sources` / native-59/network journal15: DHCPv6学習NTP・元期限・代替先を接続。
  独立ISC/chronyd/keepalivedと実worker/captureで、通常DAD、学習→元期限で代替先→再取得、
  VRRP主系条件、supervisor/control停止中の元期限とLAN応答、同じchronyd PIDを確認し終了0。
  観測取得不能は503とし、既に受信した元期限内の情報を「撤去済み」と偽って返さない。
- 同じ512MiB/2 CPUの配布イメージで、毎回大きなGo補助をexecする2秒処理がchronydまで
  再起動させる実障害を検出した。専用client socketディレクトリだけをengineへ共有し、
  workerから直接Unix IPCする方式へ置換した。query/reload用の補助モードと繰返しexecを削除。
- `ntp-quality-native59`の`make qemu-ntp`は終了0。固定chronyd4.8/独立ISC/RP-PPPoE/Linux client、
  実APK/Alpine/PID 1とext4を使用。仮想RTCを20分遅らせ、DHCPv6学習先により
  1201.110677秒補正した。初回の周波数推定誤差は1,000,000ppmから64.72秒後に53.63ppmへ収束し、
  標準clientの採用基準を変えずLAN時刻差0.000152秒を確認。同期済みflagと品質は別で、skewも表示する。
  再起動時は`Frequency 1.552 +/- 67.615 ppm read from /state/drift`を記録し、1200.474695秒補正。
  二度目のLAN時刻差は0.000160秒。IPv4/IPv6 TCP/UDP・DNS・管理・DUID・設定保持と最後の電源断まで成立。
  初回起動31.87秒・再起動64.35秒。二度目のWAN/LAN captureも保存し、NTP応答のstratum9と
  root delay/dispersionも照合した。以前の8秒取得失敗は初期推定未収束と整合するが、当時の二度目の
  captureが無いため、そのpacket上の不採用理由を断定しない。現在のclientの8秒/品質基準は不変。
- PPP/DHCP/conntrack/Tailscale/chronyの5engineと対応sourceをAPK/image/OCIの構築経路へ接続。
  native-59のAPK検査・イメージ構築、amd64/arm64のchronyd ELF照合、`ntp-quality-native59`のcontainer起動停止は終了0。
  最終コードのmake check/Go/Python/Race/vetと両architecture CGO_ENABLED=0 buildは終了0。
  DHCPv4学習、旧NTP resource変換、IPv6 LAN access制御、無限leaseのpeer資格、時刻未同期時のTLS方針、
  全homert受入は残る。現行homertは常設NTPServerの静的3同期先と、主系NTPClientの学習/代替2先を
  別fragmentで併存させる。現nativeの一つの学習/代替setを、その全体の同等実装とは扱わない。
  履歴処理の段階計測・修正後の成立範囲と残る失敗は次項に記載する。
- `history-startup-observe-native59` / 同じ512MiB・2 CPUの実APK/PID 1/ext4で、設定保存9.56秒、
  再起動51.4秒、独立端末のIPv4/IPv6 TCP/UDP・DNS UDP/TCP・NTPと履歴の両立を終了0で確認。
  再起動後65.2秒/9巡でtip181・archive179へ進み、Healthの観測時刻も進行、履歴破棄0・配送エラーなし。
  保存済みHealthイベントcursor141–156を製品の`show observations`から読めた。管理4照会の合計最大2.97秒。
  空の整理/圧縮はwrite transactionを作らず、整理のSELECTとwriteは同じ排他connectionで保持する。
  整理・圧縮・容量取得は既存の1秒tickで一工程ずつ処理し、各2秒の期限を共有させない。
  警報は該当処理が成功するまで保持し、残った警報を回復ログで隠さない。期限延長や健全条件の緩和はしていない。
  最終コードのmake check（Go/Python/Race Detector/vet）とCGO_ENABLED=0のLinux amd64/arm64 buildは終了0。
  計測専用の本番コードは削除。abuildの実際のGOCACHEを既存cache mountへ接続し、再ダウンロードも除いた。
- この成功を全安定化には数えない。同実行の設定変更中に履歴の期限超過が一度あり、再起動直後には
  Health履歴の1秒送信超過を観測した。継続確認時は回復していたが、途中2巡は設定healthy=falseで理由未確定。
  前の`history-phased-native59`はNTP/DHCP workerの10秒startup timeoutで世代2になり終了2。
  計測版のNTPは両起動で世代1、設定読込52–61ms、helper開始からready4.23–4.37秒、展開1.47–1.54秒だった。
  前回の10秒超過は未再現・未解消。以前のDNS TCP SERVFAILも今回未再現で原因未確定。
  同じ計測版で理由採取を追加した追試も終了0（63.41秒/9巡、tip215/archive214）。
  今回のhealthy=falseは`supervisor status validity expired`と特定した。状態取得・適用所要時間とop待ちを
  250ms監視/1秒有効期間と照合し、起動超過時の区間も観測する。単に期限を伸ばさない。
  実通信版には起動計測ログを含み、最終差分から除去した。対象物理機器・ARM実行・全homert受入は別途必要。
- `ntp-groups-native60-work` / native-60: 旧`chain/ntp.go`の独立client/server fragmentと
  `resolveServers`・list分割を再利用し、一つのchronydの下で同期先群を独立させた。
  常設serverの固定先を残し、clientのwhenはその学習/代替群だけを撤去する。学習情報と条件には別の
  元BOOTTIME期限を持たせ、workerが独立失効する。DHCPv4も受理済みの型付きgrantからNTP optionを参照する。
  旧NTPClient/NTPServerの取込は通常の`config migrate`→`config apply`へ接続。
  DNS名は宣言済みのローカルDNS listenerを使い、開発ホストのresolverや外部service managerへ依存しない。
- `make check-ntp-import`は、実homertのNTP構造を公開試験値へ置換したfixtureで終了0。
  固定3先＋主系のみの学習/代替2先、DNS名の解決、元PD期限の失効・再取得、独立keepalivedとの
  条件解除/復帰を同じchronydで確認し、独立LAN chronydの時刻採用とIPv4/IPv6 TCP/UDPも通った。
  captureでは固定3endpointと学習endpointの実NTP通信を確認した。実supervisor/control/network/DNS/VRRP/PD/time
  workerとISC/chronyd/Linux端末を使用し、ホスト時計は変更していない。旧形式を全部新規書換えした試験ではない。
  先行する群形式の実試験では、元期限・fallback・再取得・control/supervisor停止中の独立失効も終了0。
  停止からの復帰直後はwatchdogがworkerを交換し、設定適用を開始できなかったため、importを独立profileへ分けた。
  停止後の全worker自動復旧そのものは今回未受入。適用受付とworker Readyを分けて確認する。
  最終make check（Go/Python/Race/vet）とCGO_ENABLED=0のLinux amd64/arm64 buildは終了0。
  DHCPv4 option42の実学習・失効、条件付きserver待受/動的allow prefix、IPv6 LAN拒否、slew/回線断、
  時刻未同期時のTLS、全homert二台と実機の受入は残る。既存1秒監督情報の失効も未解消。
- 実行版`hgw-dns-observation-f253575-work`の実HGW/Tailscale 7800秒試験は05:16 UTCに終了0。
  独立2端末693巡のIPv4/IPv6 HTTPS・学習DNS UDP/TCP、条件付きDNS、自然PD Renew/Reply一組、
  端末DHCP自然更新、PD SIGKILL復旧1.91秒・通常再起動3.72秒・全daemon復旧4.21秒が成立した。
  全体再起動後も独立tailnet peerとの双方向TCP/UDPとInternet exitを確認し、最後のRA/kernel撤去と
  private namespace回収まで終了した。観測期間中の全worker世代は1。固定作業版の結果であり、
  今回のnative-59/main追加分の長期受入には数えない。対向underlay固定/明示IPv6 NATとhomertとの差も残る。
  以前の初期DoH期限切れ、48分Tailscale障害・106分DNS障害の原因解消や、直接ONU/対象NICの資格は主張しない。
- VRRPの選出/広告/VIPとDNSはGo実装。keepalivedやsystemdは使わない。DHCP=dnsmasq、PPP=pppd、
  接続状態同期=conntrackd、NTP=chronyd、Tailscale=公式engineの依存は残る。PID 1による所有・
  一括適用・署名runtime全体の固定/更新復旧と、外部依存削減の達成は区別する。

更新試験はprojectのMake経路で生成した`base/next/damaged`の3 command setを
`QEMU_UPDATE_BINARIES`へ指定する。`make qemu-pppoe QEMU_PROFILE=update`は通常更新と復帰、
`QEMU_PROFILE=corrupt-recovery`は既知の正常版からの破損候補拒否と最終停止、
`QEMU_PROFILE=worker-recovery`はworker交換を対象とする。更新/復帰profileには
`QEMU_POWER_ACTION=reboot`、構築済み`IMAGE_DIR`、独立ISCの`DHCP6_SERVER`が必要。
全command/publisherのnative ABIとarchitectureを揃える。試験中の入力はprivate stageへ固定する。
QEMUの512MiB/2 CPU/TCGでのPD更新後UDP失敗は別のworker-recovery実行で未解決であり、全性能の合格とはしない。

## 現在のLAN実装: routerdの再利用（2026-10-28）

`handoff-stop` / native-56は、同期断中のDHCP再起動で停止時PUTとHTTP接続待ちが残り、
3秒の管理API期限とVRRP適用確認が失敗する実経路を修正した。停止時のpeer送信を削除し、
同期ソケットを閉じ、受理済み保存を終えてから状態lockを解放する。解放通知のローカル保存と
次回の認証付き同期で回復し、切断peerを停止処理で待たない。
実署名PID 1二台・ISC・Linux AFTR・独立二WAN・BusyBox・実captureを使った
`ha-state-peer.py --pd-dslite --release-holds`が完走した。停止controlの元権限失効、双方向切替、
古い通知、engine強制終了、同期断中の更新・再起動・新規別割当、3日保持と再取得、上流停止中の
PD引継ぎ・元期限での端末失効、予備WANの新規TCP/UDP、PD復旧後DS-Liteが成立した。
同じTCPはSYN一回・再接続なし、UDP損失0件、最長TCP応答待ち6.82秒。今回の最終設定撤去は待機系である。
試験が最終PD所有者を常にbと仮定して止まる不具合も直し、実際の所有者・引継ぎ先・clock差を使った。
1秒広告のこの実行結果であり、従来の連続障害15秒・200ms広告・実設定全体・対象機器を一律に閉じない。
make checkと両architecture静的ビルド成功。Alpine3.24.3/Linux6.18.59-0-ltsの同梱DHCP engineを含む
イメージもQEMU/amd64で実起動した。独立BusyBoxの自動IPv4/gateway/DNS、独立RP-PPPoEのCHAP、
ISC PD自然更新、SLAAC、IPv4/IPv6 TCP/UDPと取得DNSでの問い合わせ、PPP/control交換と通常電源断の
RA撤去→PADTが通った。固定LAN設定を除き、端末は実割当値を使う。OS標準resolverの試験ではない。
これはPPPを使う配布経路で、homertのDS-Lite/conntrackd/Tailscale全体イメージ・実機の資格ではない。
19:44開始の実HGW7800秒版（先行`lan-reuse`）は、21:31:52 UTCにLAN DNSのA問い合わせが
SERVFAILとなり終了1。直前まで577回の通信と全worker世代1を観測したが、自然PD更新前に失敗した。
製品・上流・試験の原因は未確定。次回は既存DNS workerのstatus/eventsも失敗時に取得する。

`reuse-routerd-lan` は独自DHCPv4割当器・wire処理・JSON台帳・私設状態APIと対応する旧試験を削除し、
旧 `RenderDnsmasqLines` と固定版dnsmasq/muslを接続した。配布archiveと対応sourceを同梱し、
supervisorが検証したFDを渡す。PID 1から管理し、host dnsmasqやsystemd/OpenRCには依存しない。
DNS・DHCPv6・RA・VRRP選出は既存Go worker、接続状態同期はconntrackdを使う。
外部依存が増えた事実とGo中心の目標は区別し、他protocolの一律外部化は決めていない。

貸出の正本は `dnsmasq.leases`。Go workerは旧routerdの実要求・解放履歴を一つの所有者として記録し、
保持hostをdnsmasqへ渡す。起動ファイルの再通知を再取得にしない。通常停止時は最後の通知を保存し、
script子プロセスを回収してから通知口を閉じる。古いpeer貸出が確定した解放を消す経路を除いた。
内部contractはnative-56。破棄したprototype状態の互換readerは作らない。

`lan-dnsmasq-history` の署名付きPID 1二台・独立BusyBox・kernel NAT・captureで、両方向切替と同じ
TCP/UDP、更新・別端末の異なる割当・同期断復旧、worker/engine交換、3日保持と再取得を確認した。
実RELEASE中に履歴workerを止め、同期回線断と製品停止を重ねても、通知の保存、古いpeer貸出の排除、
両側の元保持期限、第三者への割当防止、再起動と元端末の再取得が成立した。
先行版では自然貸出失効と古い所有者の再昇格も確認したが、最新版の全分断条件の資格にはしない。
1秒広告の範囲の結果であり、200msで旧ノードが再選出された事象は未解決。

同版の実HGW・独立BusyBox/dhcpcd二端末では、標準resolver hookによる自動取得、DNS/条件付き上流/DoH、
IPv4/IPv6 HTTPSとDHCPv4自然更新、PD強制終了1.89秒・通常交換3.79秒・全体再起動3.84秒での通信復旧が
成立した。通信区間75秒。ONU直結・対象物理NIC・長時間安定性・homert全体の置換は未受入。
`lan-reuse` は明示option 6を含む独立BusyBoxのnative/移行DHCP→DNS、元観測期限、復旧も通った。
`lan-reuse` の make check（Go/Python・Race Detector・vet）、両architectureのCGO_ENABLED=0ビルド、
実6 ELFの署名catalog/publisher/stage検査、DHCP engine/sourceを含む両architectureの実CLI pack/stageが成功。
初回publisher検査は開発worktree/build先のgroup書込権限で拒否され、所有build先を直して同じ検査が通った。
ARM64実行と、この変更を含むappliance実行は未検証。次はTailscale・homert全体二台、k8s/pve差分、
Cloud SAM、配備更新復旧を計画順に進める。新しい有料環境・本番切替の許可は推定しない。

旧 `hgwdns-e2364af` の7800秒試験は通信727回と自然PD更新1回まで進み、PD SIGKILL復旧も通ったが、
最後の通常PD交換後に自動端末の `curl(6)` で終了1。完走とは扱わない。旧resolver hookを使った実行であり、
新しい標準hookの上記75秒結果と混同しない。長時間の最新版受入と実機の資格は残る。必要なWAN識別子と
更新済みgrantだけをprivate stateへ保持し、解析後の生ログ・capture・一時環境は削除する。

## 単体 router・LAN 端末を優先

`lan-reuse` / native-56の新統合版は、独立Headscale0.29.6・公式Tailscale1.104.2による
署名PID 1の既存試験を完走した。固定WAN/LANでの両family subnet/exit、DERP、worker/control/全体交換、
identity、期限・取消・再認証、DNS、tags/shieldsUp、confirmed rollbackとcaptureの範囲である。
さらに実HGWの同一製品でWAN DHCPv4/RA/PD→LAN DHCPv4/SLAAC/DHCPv6情報/DNS/DS-LiteとTailscaleを
同時に使い、独立自動端末からのIPv4/IPv6 TCP/UDP、広告IPv4 LANへの着信、両familyのInternet出口を
通信75秒で確認した。PD強制交換1.27秒・通常交換4.09秒、全体再起動4.06秒で端末通信が戻り、
全体交換後のTailscale identityと上記通信も保持した。`make check`と両architectureの静的ビルドも成功した。
Headscale/公式peerの専用underlayは試験用固定192.0.2.0/24。IPv6 LANは広告せず、LAN発IPv6には
明示masqueradeを追加した。このNATは現行homert intentとの差分であり、同一設定の置換合格とはしない。
出口だけでは未広告LANへの着信・返信を許可しないupstream動作を、両端captureと
1.104.2の`ipn/ipnlocal/local.go`で確認した。未広告の委譲IPv6 LANへの着信を受入条件へ加えない。
19:44 UTC開始の同構成7800秒試験は進行中。観測T1は開始後約7180秒で、完走・自然更新は未確定。
PD専用captureを加え、traffic用captureの循環で更新packetを失わない。次は二台全体と残る配備・更新へ進む。

`hgwdns-e2364af` は実HGWと独立Linux端末で、委譲IPv6アドレスによるDNS配布、
DHCPv4/DHCPv6情報・SLAAC、条件付きDNS・DoH、IPv4/IPv6通信を確認した。
隔離WANの通信遮断からの復旧、daemon全体の再起動と保存状態を使った別実行でも通信・PD識別子が復旧した。
旧版の7800秒試験は自然更新後、最終通常交換の名前解決失敗で終了1。ONU直結・対象機器での受入は未実施。
細分化した試験を増やすことより、homertに必要な機能を端末から使える構成に組み込み、実通信の失敗を直す。

`homert-dns-reference` は稼働homertと同じ `dnsServerFrom: IPv6DelegatedAddress/...` の `address` 参照を
既存の `DNSDelegation` に接続した。旧routerdの `chain.go` のDHCPv6Server処理と `expandServerSources` を参照した。
独立ISC・Linux端末・実daemonで、変換した設定によるRA/DHCPv6の同じ委譲DNSアドレスとUDP/TCP名前解決を確認した。
既存試験の古い端末アドレス保持を修正し、元期限失効・再取得・worker交換・confirmed rollbackも含め完走した。
Race Detector、vet、amd64/arm64静的ビルドが成功。条件付きHAの設定変換とhomert全体の受入は残る。

`pd-prep-native53` の実 HGW 90秒試験で、独立 BusyBox 端末の DHCPv4による address・gateway・DNS取得と
自然更新、DHCPv6 Information-request/Reply、SLAAC、IPv4 NAT/IPv6 HTTPS、IPv4/IPv6 DNSを確認した。
公開名は実 Cloudflare DoH、条件付き名前は独立 dnsmasq へ送り、条件付き上流の停止時に公開上流へ
問い合わせを流さず SERVFAIL、復旧後に応答することを確認した。PD workerの強制終了・通常停止後も
同じDUID/IAIDと通信が復旧した。別の90秒試験では独立dhcpcd端末がRAからDHCPv6情報取得を自動起動し、
DHCPv4とSLAAC、取得DNSを隔離resolverへ反映して名前を手動指定しない両family HTTPS/DNSを使用した。
両端末のDHCPv4自然更新と管理hostのresolver設定不変も確認した。resolver hookは試験用であり、対象端末の
OS標準resolverとの統合は未検証。IPv6 DNSは固定ULAで、homertの委譲IPv6アドレス方式は未受入。
90秒は長時間安定性ではない。
初回試験はIPv6表記の文字列比較、固定IPv6アドレスを含めた撤去判定と条件付きDNS設定順の誤りで失敗し、
各試験を修正して再実行した。実ONU直結・対象物理機器での受入は未実施。

## 作業中: PD 引き継ぎ準備 / native-53

旧 routerd の DUID 明示値優先、DUID-LL、DUID/IAID と期限を照合したリース復元を参照した。
保存した自動 DUID はリースと別に保持し、HA では同じ DUID/IAID と元の grant 期限を使う。
昇格時、共有済みの生存 PD から B4 のみを先に準備し、WAN link-local と B4 の DAD を並行させる。
準備中は LAN RA、DHCP serving、AFTR を公開せず、B4 の有効期間は元の PD と選出期限の短い方に制限する。
実二台と独立 ISC DHCP・Linux AFTR・LAN client の試験では両 DAD の重複、準備中断時の B4 撤去、
元の TCP/UDP の復旧、および control 停止中の選出期限による B4 撤去を確認した。
通常の二台 HA 全体も、独立 peer との双方向切替、TCP/UDP、DHCPv4・PD の元期限失効、
予備 WAN と DS-Lite の復旧、設定撤去および packet capture を含めて完走した。同一 TCP の最長応答待ちは
この一試行で約6.8秒、観測された UDP 損失は0件だった。`make check` の Race Detector と vet、
amd64/arm64 の静的ビルドと実バイナリの署名・配置検証も成功した。arm64 実行は未検証。

同版を ens18 の隔離 namespace から実 HGW に接続し、30秒の IPv4/IPv6 HTTPS、学習 DNS、
PD worker の SIGKILL と通常再起動後の同じ DUID/IAID・prefix と通信復旧、寿命0 RA・kernel 撤去と
上流への Release がないことを確認した。自然 Renew は未到達。ONU 直結、対象物理 NIC と本番受入は未実施。
連続障害の一試行では TCP が15秒で失敗し、予備 WAN への元の主 WAN アドレスの誤送出は観測されなかった。
別試行では同じ TCP の双方向切替と PD の元期限失効・再取得まで到達したが、最後の HA 設定撤去で
control API が3秒以内に完了せず失敗した。連続障害と設定撤去の原因調査を継続する。
追加の連続障害試験では、一回は既存 TCP が約13.3秒で復旧し、別の一回は15秒で終了した。
別試行の昇格直後には既存 conntrack は復元済みだったが、主 WAN 用 route table はまだ存在せず、
mark 付き route lookup は予備 WAN に向いた。誤送出防止がその TCP を止め、主 WAN 復帰後の
再送で通信が回復した。再送間隔も長くなるため、VIP の復帰時刻だけでは TCP の復旧を示せない。

## 作業中: VRRP の ARP 応答と連続障害

旧 `routerd-vrrp-vmac` は Linux が親インターフェースの MAC で子の VIP に
ARP 応答することを防ぐため、親と VMAC の `arp_ignore=1` を設定していた。
routerd2 にこの処理がなく、実二台試験でクライアントが旧ノードの物理 MAC を
学習し、切替後も旧ノードに TCP を再送する問題を観測した。IPv4 を持つ管理対象
macvlan の親と子に `arp_ignore=1` を適用し、アドレス撤去後に元の設定へ戻す。
予備 WAN の親にも IPv4 を設定した試験では、親が子のアドレスを名乗る ARP 要求を
物理 MAC から送り、対向の近隣情報を上書きした。両側に `arp_announce=2` も適用し、
撤去後に復元する。
この変更は compiler native-51・network journal 13 とする。開発用の旧 journal は再作成する。
`arp-vip-native51` でも新しい記録を使い、実二台の新規 ARP 応答、同一 TCP/UDP の
切替と旧親インターフェースの設定復元を確認した。
`macvlan-arp-native51b` は独立 Linux client と予備 WAN の両側で新規 ARP を行い、
二台それぞれで仮想 MAC だけの応答と共有アドレスを名乗る要求、同じ TCP/UDP の
切替、旧ノードの両設定の復元を確認した。親に IPv4 のある予備 WAN を含む通常 HA
全体も通過し、同一 TCP の最長応答待ちは約13.32秒、観測した UDP 損失は0件だった。
PVE Linux 6.8/amd64、`arp-vip-final` の実 daemon 二台と独立 Linux client では、
両ノードへの切替前後に近隣キャッシュを消して ARP を再問い合わせし、共有仮想 MAC、
同一 TCP/UDP の継続と旧親インターフェースの設定復元を確認した。
同バイナリの通常 HA 全試験は再試験で完走し、同一 TCP の最長応答待ちは約13.45秒、
観測した UDP 損失は0件だった。これは一試行の値であり、無損失の保証ではない。

別の連続障害試験は変更後も TCP の15秒タイムアウトで失敗した。再送時には切替先に
conntrack と経路が存在し、クライアントは仮想 MAC を使用していたが、主 WAN の準備中に
予備 WAN が選択されていた。`97d761a` の再試験では、主 WAN の NAT 元アドレスを
保持したまま既存 TCP の再送が予備 WAN に3回出て応答がなく、主 WAN 復旧時には
client が15秒でタイムアウトしていた。旧 routerd の conntrack mark を参照し、IPv4
source NAT の `connection_mark` と対応する policy rule を使って、新規接続の WAN と
既存接続の WAN を分ける変更を実装中。`nat-affinity-probe` の実二台試験では、主 WAN と
予備 WAN の新規接続に各 mark が付き、既存接続の mark も切替先へ渡った。通常 HA 全体は
通過した。別の連続障害では予備 WAN への誤送信が消えたが、TCP は15秒でタイムアウトした。
この変更は compiler native-52・network journal 14 とする。旧開発用記録は再作成する。
`nat-affinity-native52` の独立 Linux AFTR・予備 WAN 試験では、主 WAN 障害中に既存 TCP が
予備 WAN に出ず、新規 TCP/UDP は予備 WAN の mark で通信し、主 WAN 復旧後に元の TCP が
回復した。通常 HA 全体も実 conntrack の mark、両方向切替と packet capture を含めて
通過し、同一 TCP の最長応答待ちは約6.82秒、観測した UDP 損失は0件だった。
連続障害での同 TCP は、予備 WAN へ誤送信されなかったものの15秒で失敗した。
診断用に既存 TCP を主 WAN に固定すると予備 WAN への誤送信は止まったが、連続する
三回の所有切替では同じ15秒期限に失敗した。WAN の link-local と B4 の DAD が直列に
進むことを確認したが、PD を先に公開する試作はインターフェース停止時の RA 撤去を
壊したため削除した。DAD を省かず、所有権と撤去順序を保つ設計が必要。
通常二台試験の別試行では、1秒広告でも A の VRRP 更新が2秒の応答期限を超えて
worker が再起動し、B でも古いネットワーク適用期限の失効を観測した。計測付きの
再試験では同じ再起動は再現せず、原因は未確定。応答期限を延ばしていない。
物理 ONU・対象機器での受入は未実施。

## 作業中: 旧 routerd の PD / DS-Lite を反映 / native-49

旧 routerd の DHCPv6 client は明示 DUID を優先し、それ以外は interface の MAC から
DUID-LL を生成する。保存リースは DUID・IAID・interface・期限が一致する場合に復元し、
通常停止では自動 Release しない。NTT 回線の旧調査でも DUID-LL の利用と識別子の維持が必要だった。
routerd2 は自動 DUID をリースと別に保持する。通常停止・切替ではローカルのアドレスと RA を撤去し、
上流への Release は送らず、元の期限を持つリースを再起動用に残すよう変更した。
PVE Linux 6.8/amd64 の実 HGW 30秒試験では、SIGKILL と通常停止後に同じ DUID・IAID、
通常停止後に同じ prefix と LAN IPv4/IPv6 HTTPS・DNS の復旧を確認した。寿命0 RA と kernel 撤去を
確認し、停止時の Release は観測されなかった。自然 Renew、ONU 直結と物理機器の受入は未検証。
明示 Release 操作は未実装。
独立 dnsmasq の IA_NA 試験でも通常停止後のローカル撤去、同じ DUID とアドレスでの再起動、
TCP/UDP の復旧を確認した。自然更新、失効と再取得、NotOnLink と DAD failure も同試験で通った。
独立 PPPoE・ISC DHCP による PAP / CHAP でも PD 更新、期限切れ、同じ識別子での復旧、
PPP 交換と rollback を確認し、RA 撤去が PPP 切断に先行し Release を送らないことを確認した。
QEMU appliance の同条件での停止・再起動は未再検証。

旧 routerd の専用 DS-Lite 送信元 `/128` は他の IPv6 通信に自動選択されないよう非優先化される。
routerd2 も同様にし、元の PD の優先期限と有効期限、アドレス所有、DAD 完了を確認してから使う。
旧設定の PD Bound と専用アドレス Applied の依存を取り込み、role 条件とともに適用する。
旧実装の待機系での事前アドレス準備は採用せず、昇格後に期限付き B4 を準備する。PVE 上の実プロセスと独立 ISC DHCP・Keepalived・
Linux AFTR では、実 kernel の deprecated 状態、切替・期限切れ後の復旧、IPv4 TCP/UDP を確認した。
capture では双方の IPv4-in-IPv6 を確認した。複数障害の重なりによる既存 TCP の失敗は未解決。
変更後の二台試験では、PD と主 WAN の Health を必須とした VIP 公開条件のため、PD 失効時に
予備 WAN が使えても VIP が撤去された。主 WAN または予備 WAN のどちらかが使える条件へ試験設定を
修正し、PD 失効後の予備 WAN TCP/UDP と再取得後の DS-Lite 復帰を確認した。旧 homert の PD 必須条件も
同じ意図の確認が必要。
予備 WAN の通信を止めた再試験では、最初の失敗で旧 HealthCheck 状態機械が返す `Failing` を、
routerd2 が無条件に利用可能と判定して VIP を誤公開した。十分な連続成功で Healthy になった後の
一時的な失敗だけを猶予するよう変更した。修正後、両 WAN の相手が停止した起動時は VIP を公開せず、
相手の復旧後は PD と IPv4 TCP/UDP が戻った。二台の既存 TCP・UDP は双方向切替と worker/engine
再起動後も継続し、この試行の TCP 最長応答待ちは約13.3秒、UDP 損失は0件だった。
変更後の `make check`、独立 Health→PBR と複数 WAN 試験、amd64/arm64 の静的ビルド、
実行ファイルの署名・publisher・配置検証も通った。

旧 dnsmasq のリース同期は別の定期処理で、同期先の停止は配布応答を止めない。作業中の native-50 は
ローカルに保存したリースを非同期で同期し、同期状態とエラーを表示する。相手不在の起動・配布と、
同期断中の更新・worker再起動・第三端末への配布・同期復旧を実 BusyBox で確認した。未同期の貸出を
持つ端末が休止中に所有系を失うと別端末へ同じアドレスが配られる問題も実際に再現した。
所有者の永続記録と相手への確認を導入し、未同期の待機系が昇格した場合は新規貸出を保留する。
実二台試験で保留中の既知リース更新、worker再起動後の保留維持、同期復旧後の照合と別アドレス配布、
休止端末の元アドレスでの更新を確認した。初期化時は一台だけに `ha.bootstrap` を指定する。
保持期限の修正を含む最終版でも、所有側 PID 1 の強制終了と同じ復旧手順を通した。通常二台運用の
別試験では同期断・復旧、60秒失効、PD 失効時の予備 WAN と DS-Lite 復帰を確認した。
`make check`、Race Detector・vet、amd64/arm64 静的ビルドと署名・配置検証も成功した。
永続状態の喪失、bootstrapの誤設定、広い分断、物理機器での受入は未検証。

## 現在の変更: HA・PD・DS-Lite / native-48 / network journal12

旧 routerd の DHCPv6 client と DUID・lease 保存を参照した。自動 DUID は lease と別に永続化し、
明示 DUID は自動 identity を上書きしない。HA では設定した同じ DUID/IAID と元の grant 期限を共有する。
PD の delegation 先を client 起動の依存から外し、待機系が未作成の WAN identity を待たずに同期できるようにした。
DHCPv6 server は受信待ち中に届いた最新の入力を処理してから、応答の authority と option を判定する。

PVE Linux 6.8/amd64、`ha-egress` の署名済み実 daemon 二台、独立 ISC DHCP、Linux AFTR、
IPv6-only の主 WAN、独立 IPv4 予備 WAN と LAN client で検証した。PD 由来 B4、Health→PBR の
障害・復旧、同じ DUID/IAID・prefix と期限を保つ双方向切替、conntrackd 強制終了後の復旧、
同期断、古い通知、PD 失効と新規 TCP/UDP の予備 WAN・主 WAN 復旧が通った。capture は既存 TCP/UDP の
双方向 IPv4-in-IPv6、直結 IPv4 経路への迂回なし、TCP SYN 一回と同期 packet を確認した。
既存 TCP の最長応答待ちは約13秒、UDP 損失はこの試行で0件だった。広告間隔は homert の既定1秒で、
主 WAN の Health 成立を VIP 公開条件とした。既存接続を維持できない別の WAN へ切り替えた後は新規通信を確認した。
20cs の統合構成では worker の再起動と復旧の不安定さを観測しており、原因と切替遅延の改善は残る。
1秒広告で control 停止からの復帰中に次の切替を重ねた試行では、既存 TCP が15秒で応答せず失敗した。
各切替で同じ TCP/UDP の復旧を確認してから次を起こした再試行は完走し、最長応答待ちは約8.4秒だった。
その再試行では個別切替の VIP 公開まで約6秒、TCP 応答まで公開後約1秒以内だった。
別途観測した昇格では、選出から PD 再開まで約1.4秒、続く B4 の DAD と AFTR 準備に約2.4秒、
Health と VIP 公開に約1.1秒を要した。DAD の待機を省かず、連続障害の失敗と改善余地を追う。
初期試行で対向 AFTR の link down が戻り経路を削除し、IPv4 の直結迂回が生じたため、その試行は合格から除外した。

`make check`、既存の Health→PBR、直接 WAN と PD の HA 試験、独立 DHCPv6 information client、
amd64/arm64 の CGO_ENABLED=0 build、六 ELF の署名 catalog/publisher/stage Race 検査が成功した。
同版の実 HGW 30秒試験は DUID/IAID を保つ worker 復旧、IPv4/IPv6 HTTPS、学習 DNS、RA/Release 撤去が成功し、
自然 Renew は観測していない。ONU 直結、物理 NIC、ARM64 実行、homert 全体の受入は未検証。

## 直前の確認: source-hash / native-47 / network journal12

`forwarding.ipv4_ecmp_hash: source` を既存の writer/setting owner へ接続した。
namespace 全体の IPv4 ECMP に Linux custom source-only hash を使い、別の nft/connmark 経路は追加しない。
元 policy/mask を記録し、有効化と解除の順序、writer 交換・rollback を既存 journal で処理する。
独立二 WAN・三宛先の実 TCP/UDP で、送信元ごとの経路固定、両 WAN 利用、障害/復旧、
kernel 元値の解除と rollback 再適用、writer 交換・元期限撤去が成功した。
初回は不要な反復通信が capture の4096 frame上限に達したため、上限を維持して必要な通信へ絞った。
`make check` の Go/Python/Race/vet、両 architecture の CGO_ENABLED=0 build と最終六 ELF の
署名 catalog/publisher/stage Race 検査も成功した。旧 journal11 の開発 fixture は12へ更新し、互換 reader は作らない。
新形式でも既存 Health→PBR の切替・古い通知・元期限・rollback 試験が成功した。
実 HGW の30秒試験も PD/両 family HTTPS/DNS、DUID/IAID を保つ crash/Rebind、RA/Release 撤去を通し、
host 管理 address/MAC/default route は不変だった。native-47 の自然 Renew は未到達。

## 直前の確認: egress-route / native-46

旧 routerd の advisory 選択と実 `policyroute` 適用、既存の native ECMP を参照した。
`egress_route` は条件が真の最高 preference 候補を選び、同順位を一つの kernel route にまとめる。
相対重みは既存 next hop の `weight`、条件と元期限は既存 fact/hold と writer を使う。
別の worker・選択状態・互換経路は追加しない。入力と境界は [ResourceProgram](resource-program.md) を読む。

PVE Linux6.8/amd64、`egress-route` の実 supervisor/control/network/Health、独立二 WAN・LAN と capture で、
kernel の重み1:2、新規 TCP/UDP の両 WAN 分散、障害除外・復旧、上位 preference の選択と
confirmed rollback が成功した。control/Health を止め、network writer を交換しても元期限で撤去し、
同じ producer PID/世代の復帰後に通信が戻った。candidate の `for: 300ms` も実経路で使っている。
初回 rollback 判定は試験が古い応答構造体へ decode して confirmation を残したため timeout した。
応答ごとに初期化する既存の方法へ直し、経路・設定の復旧まで通した。製品期限は延長していない。
`make check-health-pbr` の既存切替/古い通知/元期限/rollback と新しい分散試験、`make check` の
Go/Python/Race/vet、両 architecture の CGO_ENABLED=0 build、最終六 ELF の署名
catalog/publisher/stage Race 検査も成功した。network journal は既存11、worker ABI は5。
IPv6 動的選択、policy ごとの mark/hash、既存接続継続、旧 EgressRoutePolicy 全体と homert 全体の受入は残る。

旧入力の PD/Information は、同じ master 条件と自分の PD の Bound 依存を一 client/socket へ接続する。
四つの Information 要求も既存 ORO を使う。DUID-LL・IAID・永続 identity・期限の owner は変えない。
独立した別条件は消して適用しない。conditional server と残る準備条件は [import contract](../internal/legacyconfig/README.md) を参照。
ARM64 実行、ONU 直結と対象 NIC の資格認定は未実施であり、本番 homert は置き換えていない。

## 開発環境と保護するもの

- 作業場所はPVEのnwadmin03、Go1.27.3。`go.mod`で選ばれるtoolchainを使う。
  hostの`GOTOOLCHAIN=local go`は1.22.2なので、上流source buildではprojectのGOROOTを先に解決する。
- ens18は管理経路。hostのaddress/route/sysctl/clockを試験の都合で変更しない。
  実通信fixtureは独立namespaceで実行する。別time namespaceはhost clock変更と区別する。
- `AGENTS.md`には今回より前からuserの未commit変更がある。保護し、今回のcommitへstageしない。
- `umask 022`でcheck/buildを実行する。private stateの権限検査を緩めない。
- `dist/bin`のnative3実行物、再実行するpeer/engine、通常の共有Go cacheは残せる。
  生ログ・capture・不要なcross build・publisher・比較worktree・一時releaseは試行後に削除する。
  build用sourceを展開したままproject配下へ置かない。
- `git add .`や`git add -A`を使わず変更pathを明示する。main commit/pushは既に許可されている。
  GitHub Actions待ちを実装・修正の前提にしない。

## 実上流: ens18 の実 HGW

`get-hgw-pd` の読取専用確認と homert の AFTR intent を使った。ens18 は管理経路でもあるため、
別 MAC・隔離 router/LAN namespace で試験する。host address/route/sysctl/clock は変更しない。
DHCPv6 Reply の送信元は49153。有効な DHCPv4 貸出も受けるが server identifier は HGW の IPv4 と異なる。

`3affd07` / `health-import` native-43 の7800秒試験が完走した。758回の LAN IPv4/IPv6 HTTPS と
学習 DNS UDP/TCP、再起動前の自然 Renew/対応 Reply 一組、最後の PD worker SIGKILL 後の
同じ DUID/IAID・Rebind/Reply・通信復旧、zero-lifetime RA・kernel 撤去・Release/Reply が成功した。
host 管理 address/MAC/default route は不変。private stage・capture は終了時に削除済みで、生存 owner は無い。
過去の長時間試験で起きた DNS 応答判定失敗の原因は未特定であり、今回の成功を修正済みとは扱わない。

最新 native-46 / `egress-route` でも30秒の `make check-uplink` が成功した。両 family HTTPS、
DNS UDP/TCP、DUID/IAID を保つ SIGKILL/Rebind 復旧、RA/Release 撤去と host 管理状態不変を確認した。
この短期試験では自然 Renew は未到達。7800秒の結果を最新実行版の結果に置き換えない。
再実行: `make check-uplink UPLINK_INTERFACE=ens18 UPLINK_AFTR=<確認したAFTR>
UPLINK_SECONDS=7800 RELEASE_ID=<実行版>`。
ONU 直結・物理 NIC・継続 TCP の資格認定は別である。ens20 の以前の無応答を上流全体へ外挿しない。

## 既存実装から維持する実運用の知見

- 旧routerdの`pkg/state/freebsd_duid.go`・`pkg/pdclient/snapshot.go`と現行clientを比較済み。
  自動WAN DUIDは永続identityで、明示DUIDやtrial checkpointから書き換えない。
  LAN server DUIDとは別所有。明示値変更のrollback後に試行値を残さない。
  新しいWAN DUIDへ古いserver/lease bindingを移さない。
- DHCP/PD/RAの権限は元のBOOTTIME期限を持ち、再送・poll・再起動で延長しない。
  昇格用の共有identityと降格時のRelease抑制は、その明示HA ownershipに従う。
- 通常のlink-local DADを待つ。NoDADや手動route注入を成立条件へ隠さない。
  独立optionが不正/失効しても使えるaddress・PD・routeを無関係に捨てない。
- `make check`は補助検査。実workerと独立ISC/radvd/dnsmasq/BusyBox/Linux端末、
  実Health→PBR二WAN、conntrackd/VRRPと実TCP/UDPのfixtureを必要な変更に合わせて使う。
  コマンドは`make help`と対応scriptを読む。過去の成功は今回の実行成功として転記しない。

## 次の作業

1. homert02/03の現行設定・旧routerdを比較し、残る一台分の通信と二台の切替/復旧を完成させる。
   統合構成の20cs広告時の不安定さと既存 TCP の最大約13秒の応答待ちを調査する。
   Tailscaleの残項目、旧stateless DHCPv6入力の条件付きHA/import不足、VRRP/PDの分断・復旧、
   管理・更新・戻し方を実際の相手で確認する。本番切替は行わない。
2. 実HGWの完走結果と最新版の短期結果を区別し、変更に必要な実回線検証を続ける。
   ONU/HGW側のRA/DHCPv4/DHCPv6 PDからLAN・DNS・DS-Liteまで、実相手と対象版を区別して進める。
   仮想peerとの成功を、この回線や物理NICの合格に置き換えない。
3. k8s-rt/pve-rtの稼働VMから現設定を読み取り専用で取得し、構文解析済み。BGP/DNSに加え
   SAMNodeSet/Transport/EnrollmentとMobilityPoolが使われる。秘密を含む原本は公開しない。
   取得・解析を変換・通信受入とは区別し、homert成立後に実差分を解消してCloud SAMへ進む。
   SAMは旧docsの再設計目標を読み、認証/取消、構成配布、/32所有とNAT無し転送、移動・復旧を完成させる。
   新しい有料環境やproduction操作の許可を推定しない。
