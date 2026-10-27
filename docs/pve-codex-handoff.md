# routerd2 開発引き継ぎ

2026-10-27。全34 work packageのゴールは継続中。homertの本番置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を参照する。
実行順序はhomert02/03、実態を確認したk8s-rt/pve-rt、その後既存文書に基づくCloud SAM。

## 現在の変更: dhcp6-confirm / native-35

旧routerdのnetworkd IA_NA生成、DUID/PD保存・Restoreを再確認した。PD/RA連動を
独立ISC DHCP4.4.3-P1/radvd2.19と別Linux SLAAC端末で接続し、M解除中のPD取得→MでIA_NA追加、
同じDUID/IAID/workerの維持とWAN/LANのIPv6 TCP/UDPが通った。さらに別Linux端末との実DAD失敗・
Decline中も委譲LANのTCP/UDPが通り、重複端末の撤去後に同じworkerでIA_NAを取得した。
サーバ停止・M解除後のSIGKILLから、同じIA_NA/PDと元期限を復旧し、Rebindで更新できた。
peerがPD poolへの戻り経路を実clientのlink-localへ設定し、製品側とLAN端末への経路注入はしない。
再実行は`make check-dhcp6-ra-pd DHCP6_SERVER=/path/to/isc/dhcpd RELEASE_ID=dhcp6-confirm`。

IA_NA単独の同boot復旧をConfirmへ実装した。保存値のboot/interface/IAID/DUID照合と
RA開始履歴の復旧を一か所にまとめ、元preferred/valid・T1/T2・lease serverは変えない。
Confirmingは使用可能として投影しない。初回送信から10秒以内のSuccessで再開し、無応答なら
生存する元grantだけを使用する。NotOnLinkだけなら待機window後に旧addressを捨てて新規取得する。
別serverからの後続Successは先行NotOnLinkより優先する。期限切れは確認中でも新規取得へ進む。
PDを要求するclientは従来どおりRebindを使う。[RFC 9915 §18.2.3/18.2.10.3/18.2.12](https://www.rfc-editor.org/rfc/rfc9915.html#section-18.2.3)と
独立ISC clientの`start_confirm6`/`do_confirm6`を比較した。compiler native-35、worker ABI5、
network journal11、DHCPv6 checkpoint2。旧開発形式の互換処理は追加しない。

最初のIA_NA取得試験は、試験がworkerのbound通知をkernel適用/DAD完了と誤認して失敗した。
実kernelのaddress非tentative状態を待つ。DAD peerをISC自身に置く案は、ISCが自interfaceのaddressを
poolから除外するため廃棄し、独立Linux端末へ分離した。通常DADを無効にして通していない。
独立dnsmasqとのConfirm/Reply・元期限/T1/T2保持・TCP/UDPが通った。無応答Confirmも
初回送信から10秒の待機後、同じ元期限と通信を保持した。対向の実prefixを42→43へ変更した
NotOnLinkでは、旧IA_NA撤去→同DUIDの新規取得→IPv6 TCP/UDP・DNS復旧が通った。
従来の自然Renew・Rebind・120秒失効/復旧、worker交換、MTU rollback・最終復元、Releaseと
実DAD/Declineも維持した。生ログ/capture/一時binaryは確認後削除する。

全体Race Detectorで既存VRRPのretired-generation試験がACK待ちで失敗した。修正前の単独10回は
通る一方、受付と適用待ち登録が別lock区間で、世代更新により完了した待ちを後から再作成し得た。
VRRP/BGPも受付時に一つの適用待ちを作り、callbackがその完了を待つ形へ統一した。
同じ観測のtransport再送は進行中の処理を共有する。callbackの後からの登録と状態再構築を撤去し、
待ち時間は延ばしていない。修正後の同試験10回とruntimeplan全体のRace Detectorは通った。
独立BGPのFIB/所有期限・再帰依存期限、keepalivedの初回ACK遅延・追加WAN identityとTCP/UDPも通った。
最終`make check`はGo/Python・Race Detector・vetまで通った。
`make build build-publisher build-linux RELEASE_ID=dhcp6-confirm`も通り、amd64/arm64の6静的ELFを生成した。
署名catalog/publisher/stageもRace Detectorで成功した。
`make release-built-test RELEASE_ID=dhcp6-confirm RELEASE_BUILT_DIR=<private input>`には、
group書込み不能なhome配下の一時directoryへ6 ELFだけコピーして渡す。copy/bundleは確認後削除した。
ARM64では実行していない。

required/observe-only readiness、system resolver効果、RA triggerとHAの組合せ、複数IA_NAのDAD共存、
実link切断/VM reboot/suspend、最新版の実ONU/対象NIC受入と全34 packageの残作業は継続する。

## 直前の変更: ra-dhcp6 / native-34

旧routerdのnetplan RA/DHCP生成、`networkdAdoptionDropin`の`DHCPv6Client=no`と、
DHCPv6 DUID/lease保存・復旧経路を比較した。明示`dhcp6.ra_from`で同じinterfaceのRAを指定し、
一つのDHCPv6 worker/socket/DUID/IAIDのまま、Oで情報取得、Mで設定されたIA_NAを開始する。
明示PDはM/Oと独立して取得する。後続の解除は取得済みgrantを撤回しない。
開始履歴を同boot/identityのcheckpointへ保存し、worker/operator再起動で失わない。
RA宣言だけでDHCPを起動せず、旧networkdの明示無効設定と両立する。

RA `use_mtu`/`use_hop_limit`は既定false。単一writerがIPv6 stack MTUとhop limitを所有し、
元値をjournalへ保存する。物理MTU/IPv4はRAから変更しない。IPv6 MTUは設定/物理上限で制限する。
Linuxの物理MTU変更はIPv6 MTUもresetするため、その副作用を同じintent/ACKへ記録し、
物理MTU→IPv6 MTUの順で復元する。RA worker停止は最終撤去のwriter適用ACKを待つ。
compiler native-34、worker ABI5、network journal11、DHCPv6 checkpoint2。
破棄可能な旧開発journalは再作成し、互換readerは追加しない。

PVE Linux6.8/amd64の隔離namespace、実supervisor/control/network/RA/DHCP/DNS workerと、
独立radvd2.19/dnsmasq2.90を使用した。対向・製品とも通常DADを使い、残存経路を注入しない。
M0O0でDHCP送信なし、Oで情報/DNS、MでIA_NA、解除後も同workerのgrant維持が通った。
RA学習のdefault/on-link経路でIPv6 TCP/UDPとUDP/TCP DNS、実Renew・Rebind・120秒失効と
復旧、client交換時のDUID/IAID保持、Release/Replyと無応答4送信の停止が通った。
IPv6 MTU1400/hop42（物理1500）、実TCP hop42、writer SIGKILL、物理1300への変更と
confirmed rollback、終了後の元MTU1500/hop64復元を確認した。
再実行は`make check-dhcp6`（独立`radvd`、`dnsmasq`、`dig`が必要）。

先行終了試験では元値がjournalに残っても復元されなかった。RAの最終通知が受付ACKだけで
worker/controlを終了させる原因を修正し、DHCPv6と同じwriter適用ACKへ接続した。
独立peerのDADは製品側vethのUP後に完了を待つ。radvdの送信元は実選択されたlink-localを使う。
先行失敗を成功に含めず、一時診断コード・生ログ・capture・試験binaryは削除する。
既存の実daemon DAD/DeclineとRA経路失効/設定撤去も通った。`make check`はGo/Python・
Race Detector・vetまで成功し、`make build build-publisher build-linux RELEASE_ID=ra-dhcp6`で
amd64/arm64の6静的ELFを生成した。`release-built-test`の署名catalog/publisher/stageも
Race Detectorで通った。入力はgroup書込み不能なhome配下の一時directoryへ6ファイルだけ
コピーし、`RELEASE_BUILT_DIR`で指定する。group書込み可能なrepoからの署名は意図どおり拒否された。
一時copy/bundleは削除した。ARM64では実行していない。

required/observe-only readiness、system resolver効果、RA triggerとPD/HAの組合せ、
VRF・実suspend・別VM再起動・最新版の実ONU/対象NIC受入は未完了。
HA-02のpartition/fencing・切断peer復帰・delta/gap等も継続する。
全34 packageの完了・対象実機での利用可能とは判断しない。

## 直前の変更: ha-pd / native-33

HA-02のPD checkpoint移送を実装した。旧routerdの`chain/filesync.go`と共有DUIDを使う
PDの保存/復旧経路を比較し、worker内の相互TLS認証付きcomplete snapshotへ置き換えた。
既存VRRPの現所有観測で、事前生成した主系/待機系workerを選ぶ。待機系はWANがなくても
同期受信のため稼働し、DHCP/DAD socketや受信用TCP listenerを開かない。
TLSの証明書固定・鍵読取・endpoint検証はDHCPv4/PDで共用する。別の同期managerは増やしていない。
設定・順序と制限は[DHCPv6 worker](../internal/protocol/dhcp6/README.md#two-node-pd-checkpoint-transfer)を参照する。
compiler native-33、worker ABI5、network journal10、DHCPv6 checkpoint2。
旧checkpoint1のreaderは残さない。破棄可能な開発stateは再作成する。

送信元のsnapshotとBOOTTIMEを保存し、受信側の要求開始時刻へ残り期限を対応させる。
UTCを使わず、往復/処理遅延で貸出を延ばさない。同じrevisionの再受信や待機worker交換では
時計対応を作り直さない。昇格は保存したepochを進め、有効なPDをRebindで確認する。
古いepoch/revision・同一revisionの競合を拒否する。同期link断でも主系の上流通信は続け、
待機系の`snapshot.sync`へ最終受信時刻と同期エラーを表示する。
メンバー停止はkernel/RA撤去をACKしても共有リースをReleaseしない。単独clientのRelease順序は維持する。

PVE Linux6.8/amd64、二つの署名済み実PID1 tree、独立ISC DHCP4.4.3-P1とLinux SLAAC端末を使用。
片側だけLinux time namespaceでBOOTTIMEを+7200秒にした。host時計は変更していない。
ISC応答停止中のA→B→Aで同じDUID/IAID/prefix、延長しない元期限と実IPv6 TCP/UDPが通った。
待機worker交換、古いPD通知409、同期link断のエラー表示と復旧、peer不在の追加切替後の
router/clientの元期限失効、失効後再起動の非復活、ISC復旧後のTCP/UDPも確認した。
captureでRebindと、メンバー切替時の共有リースRelease不在を確認した。
同じ試験内で既存DHCPv4の実60秒失効・sticky同期、証明書/古い/競合revision拒否と
同一TCP socket・UDP NAT対応も維持した。最終runの観測UDP損失は0。先行runでは1 datagramの損失も観測し、損失ゼロの保証とはしない。
再実行は`make check-ha-pd DHCP6_SERVER=/path/to/isc/dhcpd DOCKER='sudo -n docker'`。
最終形の`make check`（Go/Python・Race Detector・vet）とamd64/arm64の6静的ELF buildも成功した。
単独IA_NAの独立dnsmasq試験も成功し、取得/更新/失効/復旧・worker交換・DAD/Decline・正常停止Releaseを確認した。

初回は生成RAの最大間隔10秒と設定側の最小寿命600秒が食い違い、600秒制限を10秒へ揃えた。
独立peerも通常DAD完了を待ってからsocketを開く。次に、WAN不在が待機serviceまで除外する
暗黙依存を修正した。同期link断では意図的にDHCPv4が非Readyになるため、PD転送を全serviceの
Readyで待つ手順を廃棄し、対象workerと独立SLAAC端末・実TCP/UDPで確認する。
失敗した先行runを成功結果には含めない。生ログ・capture・一時catalogは確認後削除する。

これは周期的complete checkpoint移送で、poll間に未同期の更新が残り得る。
異なるkernel/boot IDのVM再起動、partition/fencing・切断peer復帰、delta/gap、pair全体を明示的に
Releaseする操作、最新版の実ONU/対象NIC・負荷・ARM64実行の受入は未完了。
PD replicaの古い/同revision競合を受信側へ直接再送する負試験も残る（今回の409確認は旧worker通知）。
HA-02と全34 packageのゴールは継続し、実機置換可能とはまだ判断しない。

## 直前の変更: pd-recovery / native-32

旧routerdの`cmd/routerd-dhcpv6-client`のDUID選択、`restoreLease`/`saveLeaseLocked`、
`pkg/pdclient.Restore`と設定照合を読み、PDの保存・再起動復旧を実装した。
明示DUID→保存済みDUID→Ethernet DUID-LL/PPP DUID-UUIDの順に選択する。
DUID/IAID・server・割当・元BOOTTIME期限を一つのprivate checkpointへ保存し、
fsync後に公開する。同じboot/interface/IAID/DUIDで有効なPDだけ復旧し、Rebindで確認する。
サーバー無応答でも元期限を延ばさない。正常停止はkernel/RA撤去ACK→空checkpoint保存→Release。
別boot・変更されたidentity・失効PDは新規取得する。旧identity形式のreaderは残さない。
設定と詳細は[DHCPv6 worker](../internal/protocol/dhcp6/README.md)を参照する。
compiler native-32、worker ABI5、network journal10、DHCPv6 checkpoint1。

PVE Linux6.8/amd64の隔離namespaceで、署名済み実PID1とRP-PPPoE、独立ISC DHCP4.4.3-P1、
Linux SLAAC clientを接続した。PAP/CHAPでworker・PPP交換時のDUID保持とIPv6 TCP/UDPが通った。
ISC停止中のSIGKILL復旧ではprefixと元期限が完全一致し、captureで応答前のRebindを確認した。
最終CHAPではpeer終了中の復旧・router/clientの元期限失効・再起動による失効PDの非復活も確認した。
古い通知拒否・confirmed rollback・control/operator/PID1停止・RA撤去→Release/Replyも通った。
先行CHAPは復旧直後のcurlで失敗した。SLAAC addressがあってもclientの短いdefault routeが
切れている場合があり、実clientのRA学習とcontrol適用完了を確認してから転送を検査する。
routeの手動注入やDAD無効化はしていない。再実行は`make check-pppoe-pd DHCP6_SERVER=/path/to/isc/dhcpd`（PAP/CHAPの両方）。

最新ソースの専用QEMUでは実UTC±24h、停止したproducer/controlと独立writerの元期限失効、
RA router/PIO/RDNSS撤去、期限切れ再送拒否と新規取得/TCP/UDPが通った。
停止直前の情報通知やtransport再送の到着はあり得るため、外側sequenceの完全固定ではなく
実停止時のBOOTTIME以前の観測と元IA期限を検査する。heartbeatで既に停止処理中のproducerを
Ready待ちしていた試験手順も直し、同じ期限・後続の新規取得条件で通した。
再実行は`make qemu-dhcp6-clock`。host時計は変更していない。
`make check-dhcp6`も成功し、独立dnsmasqとのIA_NA取得・worker交換・TCP/UDP・実Renew/Rebind・
失効/復旧・DAD/Declineと正常停止Releaseを同じ保存経路で確認した。
`make check`（Race Detector・vetを含む）とamd64/arm64の6静的ELF buildは成功した。
生ログ・capture・一時catalogは確認後削除し、蓄積しない。

これはHA-02のローカル保存・復旧部分であり、二台間PD同期は未実装。
異なるboot時計間の期限移送、owner/fencing、待機系移行時のRelease抑止、IA_NA-only Confirm、
最新版の実ONU/対象実機受入は残る。全34 packageの完了・実機利用可能とはしない。

## 直前の変更: dhcp-request-clock / native-31

独立dnsmasq2.90とkernel netemの5.2秒遅延で、WAN DHCPv4 clientがACK待ち時間を
貸出期限へ加算する問題を再現した（要求から約5.21秒後を取得時刻にしていた）。
旧routerdの`cmd/routerd-dhcpv4-client:bind`も受信時刻を渡していたため、wire/lease型は再利用しつつ
[RFC 2131 §4.4.1/4.4.2/4.4.5](https://www.rfc-editor.org/rfc/rfc2131.html#section-4.4.5)の
最初のREQUEST送信時刻を保存する。再送で更新せず、boot期限とUTC表示を同じ要求へ揃えた。
到着時に既に期限切れのACKでは新しい交換を始める。更新中の旧貸出が有効なら、その元期限を保持する。
`dhcp-request-clock`の実supervisor/control/network/DHCP workerは、独立dnsmasqとの通常取得、
INIT-REBOOT、自然更新を遅延・再送込みで通した。ARPは遅延させず、captureで要求/応答時刻と
貸出時刻を比較する。再実行は`make check-dhcp4`。kernel経路、TCP/UDP、失効・再起動復旧も通った。

QEMUの新鮮な`qemu-daemon-test` binaryでも、実UTC±24h補正・同じboot期限・kernel再適用、
SIGSTOP中の3秒貸出失効・古い通知拒否・worker/control交換後の実TCP/UDPが通った。
先行試験は再起動後の転送/復旧で失敗した。20秒に延ばす一時手順は廃棄し、3秒条件を保持した。
貸出観測はkernel適用完了ではないため、転送確認はcontrolの適用完了と実kernelを待つ。
ACKを止めた後の再送で3秒の貸出を受けても、元REQUESTから既に失効している場合があった。
捨てるだけでは同じ古い要求を60秒まで続けるため、新しい交換へ進むよう実装を修正した。
最終QEMUは3秒の失効・worker/control復旧まで通り、TCP/UDPも同じ条件で通った。
基準`ecf8eae`の既存QEMU試験も別worktreeで実行して成功した。物理機の故障再現ではない。
`make check`（Race Detector・vetを含む）、amd64/arm64の6静的ELF buildも成功。
daemon runnerは通常実行の試験binary/log/captureを終了時に削除する。明示`--output`時だけ残す。
ARM64実行・最新binaryでの実ONU長時間通信・対象機器の受入は未完了で、全34 packageを継続する。

## 直前の変更: ha-dhcp / native-31

HA-02のDHCPv4同期を実装した。設定・保存順序と制限は
[DHCPv4 worker](../internal/protocol/dhcpserver/README.md#two-node-replication)を参照する。
旧routerdの`chain/filesync.go`の15秒ファイルコピーと比較し、worker内の相互TLS認証付き
complete checkpointへ置き換えた。主系・待機系のfsyncとcontrol反映を終えてからDHCPACKを返す。
待機workerはVIP不在でも受信し、現VRRP所有観測で配布設定へ交換する。
compiler native-31、worker ABI5、network journal10、DHCP private journal2。
廃止した開発形式の互換readerは追加しない。

PVE Linux6.8/amd64、二つの署名済み実PID1 tree、独立BusyBox udhcpc二端末とLinux TCP/UDP peerで
DORA、A→B→Aの同一貸出更新、二端末の重複防止、待機worker交換、TLS送信途中の切替、
誤証明書・重複/古い/競合revision、外側同期link断時のACK停止と復旧が通った。
実60秒失効は、主系ファイルの更新だけでなくworkerの完了済みcheckpointを待って両系を比較する。
主系保存直後を同期完了と誤認した先行runは最終比較で失敗したため、成功として扱わない。
完了済みcheckpointで実失効とsticky保持の両系一致も通った。
既存TCP/UDPは同じsocket/NAT対応を維持し、captureで単一SYNとFTFWを確認した。観測UDP損失は0。
再実行は`make check-ha-state DOCKER='sudo -n docker'`。
生ログ・capture・試験binary・一時catalogは確認後に削除し、蓄積しない。
`make build build-publisher build-linux RELEASE_ID=ha-dhcp`成功。
amd64/arm64の6静的Go ELFを生成した。ARM64での実行は未検証。
`make check`（Go/Python・Race Detector・vet）と
`gitleaks protect --staged --redact --no-banner`も成功した。

現在は両node稼働を要する同期方式で、peer不在では起動/新規貸出/更新を止める。
partner-down継続、owner epoch、delta/gap、PD/DUID/IAID・IPv6同期、maintenance切替、
対象実機・ONU・負荷・clock skewの受入は未完了。homert置換可能とはまだ判断しない。

## それ以前の変更: peer-receive / native-30

旧routerdの受理・利用経路と照合し、実通信を遮る追加制限を修正した。

- `conntrackconfig.Validate`の`IsGlobalUnicast()`を外した。同期interfaceと異なる二つのIPv4
  unicast addressを指定でき、IPv4リンクローカルも使える。未指定・multicast・limited broadcast
  は対向unicast addressとして扱わない。旧`pkg/config/validate_firewall.go`と
  `pkg/render/conntrackd.go`にはglobal-unicast制限がなかった。
- DS-Liteの`b4_address`/`b4_peer`にも同じ追加制限があり、解除した。
  旧`validateTunnelAddress`と`TunnelInterfaceController`はIPv4 prefix/addressを利用する。
- RAのframingが成立する不正PIO/MTU/RIOだけを無視する。重複RIOはwire順で更新する。
  以前はRA全体を拒否していた。旧`pkg/raobserver/raobserver.go`は観測専用であり、
  旧Linux環境の実際のRA適用者はkernelである。観測器の受理だけを旧転送の実績には数えない。
  checksum・hop limit・link-local送信元・packet境界の検査は保持する。
- DHCPv6のIA_NA/IA_PDで、不正な一割当だけを無視し、同じIAの有効割当を保持する。
  prefix外のhost bitsはmasked prefixへ正規化する。旧`pkg/pdclient/message.go`もmaskする。
  [RFC 8415 §21.6/21.22](https://www.rfc-editor.org/rfc/rfc8415.html#section-21.22)の
  不正preferred lifetimeの破棄対象は一つのaddress/prefixである。
- AFTRのAAAA集合に使えないaddressがあっても、有効候補を利用する。旧
  `cmd/routerd/runtime_network.go:resolveAAAA`と`chain/phase2.go:resolveAFTRIPv6`は
  一つの不適格候補で集合全体を捨てる処理を持たない。DNS交換の識別・CNAMEとRRset寿命は保持する。
- DHCPv4 serverは実送信元の確認後、DISCOVER/option50付き要求の残存`ciaddr`を0へ正規化する。
  旧routerdのLAN配布はdnsmasqであり、公式dnsmasq2.90 `src/rfc2131.c:150–153`にも同じ処理がある。
  これは特定端末での実観測や旧稼働dnsmasqの厳密な版を確認したという意味ではない。
- RAのDNS server/search domainが満杯なら、[RFC 8106 §6](https://www.rfc-editor.org/rfc/rfc8106.html#section-6.2)
  に従って最も早い期限を除く。32件ずつの上限を保ち、独立するdefault route更新を拒否しない。
- 正当なIPv6の大文字・省略・先頭0表記をBFD/DS-Liteの入力で正規化する。
  旧`pkg/controller/bfd/controller.go`と`phase2.go:localIPv6Address`も解析後に正規化する。
  BGPのBFD参照/local addressも同じ値へ揃え、参照検査は文字列でなくaddressで比較する。
  compilerはnative-30。新たな互換readerやadapterは持たない。

PVE Linux6.8/amd64の`peer-receive`で、既存fixtureと実product processを使用した。
二つの署名済みPID1 tree/実conntrackd、独立Linux client/server、専用`169.254.42.2/16`↔
`169.254.42.3/16`同期linkで、同じTCP socket/UDP NAT対応のA→B→A、engine SIGKILL/復旧、
設定削除時のsysctl復元が通った。captureは一つのTCP SYNとlink-local FTFWを確認し、UDP損失は0。
修正前binaryでは同じlink-local設定がbootstrapで拒否された。
不正独立RA option/重複RIOと不正IA siblingを含む実socket受信も、修正前では経路/PD取得に失敗し、
修正後はRA経路、PD再番号付け/expiry/再取得、LAN RAと実TCP/UDPが通った。
内側IPv4 link-localのDS-Liteでも、独立Linux AFTRへ実TCP/UDPを転送した。
DNS AAAAにmulticastと有効AFTRを同梱しても同じ通信が通った。
実supervisor配下のDHCP workerでは、既存IPv4を送信元と`ciaddr`に残したDISCOVER/REQUESTで
同じ貸出へのOFFER/ACKが通り、DNS反映・元期限失効・journal復旧も確認した。
RAの40 DNS server/40 search domainは32件ずつに収まり、既存default routeの1秒期限を
更新して実TCP/UDPが通った。長い期限のresolverも保持した。新DNSを先頭へ置く順序と
起動直後の空module一覧を試験観測側で誤認した先行runは失敗し、観測処理を直して再実行した。
大文字/先頭0のIPv6設定では、独立Linux転送peerを越えるBFDの障害/復旧/worker交換と、
同じendpointの二VRF/default domainでBFD→BGP→学習経路→TCP/UDPが通った。
DS-Liteの大文字/先頭0 local/resolver設定も、独立DNS/Linux AFTRと転送・期限失効が通った。

`make check`（Go/Python・Race Detector・vet）成功。`make build build-publisher`と
`make build-linux RELEASE_ID=peer-receive`でamd64/arm64の6静的Go ELFを生成した。
最後の参照正規化変更は関連packageのRace DetectorとIPv6 BFD/BGP実通信でも再確認した。
`gitleaks protect --staged --redact --no-banner`成功。ARM64の実行・物理機の受入は未実施。

これは全受信経路の監査完了ではない。DHCPv4 clientのsocket/BPF/decoderと旧実装の送信元port条件、
DHCPv6の高位送信元port受理、RAのRS/RA送信元規則、HealthとVRRPの設定/受信経路も読み比べたが、
残るDHCP server/relay、DNS、PPP、BGP/BFD、WireGuard等の全option・件数・寿命・失敗範囲は未監査。
大容量時の拒否範囲も残る。AGENTS.mdには出力/保存表記を入力拒否へ転用せず、入力と参照を
一緒に正規化する原則を一文だけ補足した。
HA-02のDHCP/PD同期、HA-03の残受入、Tailscaleと全34 packageの残作業は継続する。
raw診断・capture・一時署名catalogは試験終了後に削除する。物理NIC/ONU/負荷/電源断の資格確認ではない。

## 直前の変更: ha-conntrack / native-29

homertで使われるconntrackd FTFWを、IPv4 VRRP workerが所有するengineとして接続した。
独立した新しいcoordinator・registry・shell hookは追加していない。
旧routerdの`cmd/routerd-vrrp-vmac/main.go`と`pkg/render/conntrackd.go`を直接比較した。

- 昇格は`-c → -R → -B`を進めてから、既存control/network経路へelected状態を渡す。
  peer cacheのkernel commit完了を確認してからWAN identityとLAN VIPを公開する。
  rescan/broadcast要求の受付は、peerへの全転送完了とは区別する。
- 降格はwriterの撤去ACK後に`-t → -n`を要求する。engineは最終撤去まで維持する。
  engine異常終了はVRRPの撤去とworker交換へ結ぶ。
- `forwarding.conntrack_tcp_liberal=true`を明示し、writerが適用・元値の保存・撤去時の復元を行う。
  VRRPはこの設定、同期interfaceと宣言されたlocal addressへ依存する。
- conntrackd 1.4.9を固定Alpine入力からbuildする。実行ファイル・loader/library・protocol DB・licenseを
  regular-file imageへ収め、元source/APKBUILD/patch/build scriptを別artifactとして署名で結ぶ。
  FD5と空のprivate mount/chrootを使い、host binary/config/libraryへfallbackしない。
- 元のCLI exit0は部分commit失敗を表さない。GPLの小さなpatchで実commit終了時のconntrack/
  expectation失敗件数を返し、失敗または完了応答なしでは公開しない。stdoutと診断stderrは分ける。
- boot ABI1・worker ABI5・manifest v4。compilerはnative-29、network journalは10。
  旧開発journalのreader/migrationは作らない。既存`pppimage`は二つの実engineが使う`engineimage`へ移した。

実装・設定例・所有権の範囲は[conntrack同期](../internal/protocol/conntracksync/README.md)を参照。

## 実行した範囲

`ha-conntrack`のPVE Ubuntu Linux6.8/amd64、二つの署名済み実PID1 treeで確認した。
supervisor/control/network/management/logging/VRRPとconntrackdを実際に起動し、別Linux端末と
別Linux serverのTCP/UDPをNAT転送した。物理portの静的address、WAN VMAC/VIP、LAN VIP、
route/firewallとconntrack sysctlはproduct設定で作り、手動補修や残存addressへ依存させない。

- 一つのTCP socketと一つのUDP socketで連続送信し、A→B→Aの切替と旧worker/engine再起動後も
  同じTCP接続・NAT対応を維持した。`tcp_loose=0`で途中TCPの作り直しを防いだ。
- 稼働中engineを実SIGKILLし、撤去、peerへの切替、worker/engine再作成後の同じ通信を確認した。
- captureはclient SYNが一つで、FTFWの実packetと同じNAT tupleを確認した。
  最終cohortのUDP損失は0、先行cohortでは切替中に1 datagram失った。無損失保証ではない。
- 設定から機能を削除し、workerのreapと元のconntrack sysctl値0への復元を確認した。
- 同期だけを外した比較では、初期TCP/UDPの成立後、最初の切替で元TCPのreadがtimeoutした。
  最初の比較の観測処理はstderr混入でも失敗した。診断出力を分離した再実行で、このTCP失敗を確認した。
- `make check build-linux build build-publisher RELEASE_ID=ha-conntrack`成功。
  全Go/Python、Race Detector、vetとamd64/arm64の6静的Go ELFを含む。
- `make check-health-pbr RELEASE_ID=ha-conntrack`成功。実2 WANのTCP/UDP切替/復旧、
  Health/control/network再起動、古い通知、元期限/利用側鮮度、writer再起動時の撤去、confirmed rollbackを確認した。
  最終6 ELFの署名catalog/publisher/stageもRace Detector付きで成功した。ARM64 engine build/実行は行っていない。

部分commit応答のdecoderは局所試験で確認したが、実kernel tableの枯渇は未実施。
factory imageへの同梱、同期link分断/gap/古いpeer、認証付き参加、VRF/zone、大量状態、
物理NIC/負荷/電源断の受入は未完了。今回のTCP継続をHA-03全体やhomert置換完了に数えない。

## 直近の環境と再実行

`umask 022`でproject Makefileのbuild経路を使う。

- `make check-ha-state DOCKER='sudo -n docker' RELEASE_ID=<release>`
- `make conntrack-engine DOCKER='sudo -n docker' CONNTRACK_ARCH=<arch>`
- `make check-health-pbr RELEASE_ID=<release>`

通信fixtureはprivate network/mount/PID namespaceとprivate `/run/netns`を使い、独立Linux端末を置く。
必要なkernel moduleは`nf_conntrack`と`nf_conntrack_netlink`。TCP looseは隔離namespaceだけで0へ設定する。
期限とkernelのPID namespace終了で子processを止め、完了したログ・capture・catalogは削除する。
host管理NIC/address/route/clock、稼働中homertの設定・serviceは変更しない。
配布に必要な対応sourceとlicenseは残す。過去の診断出力を再保存しない。

## 現行homertで確認した条件

2026-10-27に両nodeの設定とControl APIを読み取り専用で確認した。
homert02はmaster/activation Ready、03はbackup/Standby、優先度150/100・preempt=false。
主系はPD取得とPD由来DS-Lite分散経路を使い、待機系は代替経路を選ぶ。PPPは設定にない。
両系のTailscaleはRunning/online、subnet/exit-node広告、acceptDNS/acceptRoutesはfalse。
conntrackdも両系activeで、FTFW・外部cache・起動時resync・expectation同期を使う。
DHCPv4/PD同期は主系から15秒間隔でSynced。DHCPv6 serverはstatelessで、PD由来DNS/domain-searchを配り、
待機系ではWhenFalse。表示上のRunning/Syncedを、同期内容や既存TCP継続の証明とは扱わない。

実上流からの重要な知見は引き続き適用する。DHCPv6 Replyの送信元49153を受け入れ、通常DADを待つ。
RAには現在のMACを使い、元のlease期限を受信時刻で付け直さない。DS-Liteは選択FIBとAFTR往復を確認する。
以前のnative-27約130分の実上流cohortは最新ABIの長時間試験・ONU直結・物理NIC資格判定ではない。
`f1ce267`までのcontrol交換と永続ext4のfactory reboot/PD/TCP/UDP/DNS確認も、今回のCtd factory受入とは別である。

## 残作業

次はHA-02のDHCPv4貸出・PD identity/期限の引継ぎと、homertの一台分の未接続機能を進める。
stateless DHCPv6/DNS配布、Tailscale、HA-01の全readiness/phase、複数DS-Lite/代替回線と管理経路を
現行設定へ接続し、二台切替・復帰で必要な状態を引き継ぐ。HA-03の分断/再同期/古い状態と負荷も残る。
全34 work packageは計画の範囲を維持する。BGP/BFD全profile、DHCP server全pool、VRF、cluster、
cloud/SAM、権限分離、全運用surface、SSH更新・trial/fallback、OS A/B・source配布、
ARM64と対象物理機の受入は未完了。独立して進められる実装・実通信を継続する。
