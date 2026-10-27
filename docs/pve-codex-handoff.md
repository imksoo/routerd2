# routerd2 開発引き継ぎ

2026-10-27。全計画のゴールは継続中。対象実機で利用可能とは判断していない。
機能範囲と未完了項目は[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)に従う。[AGENTS.md](../AGENTS.md)を優先する。
過去のAPI・保存形式・試験結果を守るための実装は残さない。

## 現在の変更: dhcp6-release / native-28

正常停止では、DHCPv6の最終観測とその完了待ちを同じoperationへ結ぶ。network writerの
アドレス撤去と、生成RAの寿命0送信を確認してから、元のlink-local socketでReleaseを送る。
選択Server IDと有効なIAだけを使い、同じclient/transaction/serverのReplyで完了する。
最大4送信・cleanup全体20秒。停止中もcontrol/network/APIを維持し、DHCPv6→他protocol→
control→network→loggingの順で終える。PPPとPDを同時削除する設定変更でも先にPDを解放する。
旧routerdの`pkg/pdclient.Client.Release/send`と
[RFC9915のRelease手順](https://www.rfc-editor.org/rfc/rfc9915.html#section-18.2.7)を比較した。

`dhcp6-release`のPVE Linux6.8/amd64で確認済み:

- 独立ISC DHCP4.4.3-P1/RP-PPPoE/別LAN端末で、PDだけの削除、PPP/PD同時削除、PID1正常停止の
  それぞれに寿命0のRA→Release→同じtransactionのReplyがあった。管理LANアドレスも撤去した。
  再取得は同じDUIDでIPv6 TCP/UDPを復旧した。既存のRenew、worker再作成、古い通知の拒否、
  元期限の失効、server復旧、5秒confirmed rollbackも通った。
  `python3 scripts/pppoe-peer.py --auth chap --dhcp6-server <ISC-dhcpd>`で再実行する。
- 独立dnsmasq2.90のIA_NAで、実Renew/Rebind・失効/復旧・source付きTCP/UDPに加え、
  daemon停止後の/128撤去とlink-local Release/Replyを確認した。`make check-dhcp6`が対象。
- 実上流の新規起動で、実RA/DHCPv4/PD→LAN SLAAC、IPv4/IPv6 HTTPS、UDP/TCP DNS、
  選択FIB `dslite0`と実AFTR戻りpacketを確認した。停止時にも寿命0のRA、管理アドレス撤去、
  Release/Replyがあり、Reply送信元は49153だった。全workerは世代1、1回の短い通信確認である。
  最初のcapture判定は失敗し、その原因は確定していない。診断出力を追加した実行と、
  `--immediate-mode`でpacketの受信バッファをなくした実行は成功した。前者の失敗を成功へ数えない。
- PID namespaceの電源断は、Linux `reboot(POWER_OFF)`によりSIGINTとして親へ返る。
  試験の終了コード0/130という期待を、実際の`unshare`のsignal伝播である`-SIGINT`へ修正した。

- `make check build-publisher build-linux RELEASE_ID=dhcp6-release`成功。全Go/Python、
  Race Detector、vet、amd64/arm64の6静的ELFを含む。実6ELFの署名catalog/publisher検査も成功した。
  停止順序変更の回帰確認として、実daemonの
  二WAN Health/PBR、実TCP/UDP、worker再起動、古い通知、期限切れ、confirmed rollbackも通った。
  `python3 scripts/daemon-tests.py health-pbr`で再実行する。

未検証: 無応答Releaseの全retry/停止時間、WAN設定変更での全依存関係、VRF・実suspend/reboot、
最新appliance・物理機/ONU直結配線の受入。全実装計画の完了・実機利用可能という判断ではない。
完了済みのlog/captureは削除し、結果と再実行手順だけを残す。

## 直前版: dns-bootstrap / native-28

`25d54e9`までmainへpush済み。この変更は、WANから学習したDNSを`bootstrapResolverFrom`で
名前指定のupstreamの解決に使う経路である。forwarding用とbootstrap用は別のtyped inputで渡し、
元のboot期限・リンクローカルscopeを維持する。宣言したbootstrapが不在・失効した場合、
新しい名前解決をhost resolverへ流さない。`viaInterface`はbootstrapのsocketにも適用する。
既存HTTP/TLS接続をDNS情報の失効だけで切断する設計ではない。

設定時にprovider/targetを一度結び、同じproviderの観測を一度だけdecodeして、sourceごとの
宣言順で配る。coordinatorで入力を作って再clone/再検証する重複を除き、workerのIPC境界では
全inputの認可と期限を検証してから適用する。poolとresolverはworkerと同じ寿命を持つ。
boot ABI1・worker ABI4・manifest v4・network journal9は維持し、compiler contractはnative-28。

確認済み:

- 独立dnsmasqのRA/DHCPv4/DHCPv6情報取得と別LAN端末で、hostname upstreamのbootstrap、
  UDP/TCP DNS、異なるprovider優先順、30,000 UDP欠落なし・同じTCP、元のRDNSS期限での
  forwarding/bootstrap失効、host DNSへのfallbackなし、情報再取得後の復旧を確認した。
  `make check-ra-route-refresh RELEASE_ID=dns-bootstrap`で再実行する。
- 追加実装の期限付きconnectionが`net.PacketConn`を隠し、Go resolverがUDPにもTCPの長さprefixを
  付ける不具合を実socket/captureで発見した。実際のdatagram契約を保つよう修正し、同じ相手で再確認した。
- 新規stateの実上流native-28起動で、実RA・DHCPv4・PD、LAN kernel SLAAC、IPv4/IPv6 HTTPS、
  LAN UDP/TCP DNS、選択FIB `dslite0`とAFTRのカプセル化された戻りpacketを確認した。全worker世代1。
  WAN-learned bootstrapで`https://cloudflare-dns.com/dns-query`を使う別の新規起動でも同じ経路が通った。
  どちらも短い通信確認で、自然T1・長時間・実機性能の合格ではない。DoT/QUICのdynamic bootstrap、
  VRF・suspend/時刻変更、最新applianceは未検証。
- 最初の短期試験はprocess readyを見た直後のIPv4 curlがexit7だった。
  [設定例](../examples/wan-dslite-pd.yaml)にもAFTRの`ready_when`を明示し、実FIBとDNS poolの反映を
  確認してから端末の通信を始める。通信失敗をretryで成功扱いにしていない。
- `make check build-publisher build-linux RELEASE_ID=dns-bootstrap`成功。全Go/Python、Race Detector、
  vet、6静的ELFを含む。実6ELFの署名catalog/publisher検査も成功した。

実上流の再実行は`make check-uplink UPLINK_INTERFACE=<authorized-interface> UPLINK_AFTR=<provider-name>`。
専用MACのmacvlanと別LAN namespaceを作り、管理address/route/clockは変えない。
`UPLINK_SECONDS`は1..10800秒、既定7800秒。`UPLINK_DNS_UPSTREAM`を指定すると、WAN-learned
bootstrapでそのhostname upstreamを解決する。ownerは時間上限と終了時cleanupを持ち、captureは最大8MB。
失敗したcaptureは成功とせず、終了した試験用state/captureは削除する。

別cohortのnative-27長時間試験は継続中。実上流RA/DHCPv4/PDと両family HTTPS・UDP/TCP DNSを
定期実行し、選択FIB `dslite0`と全worker世代1を観測している。自然T1は未確認。
初期captureはdown状態のWANへbindして失敗したため、稼働中の隔離namespaceで診断captureを開始した。
手順は起動前から使える`any`へ修正した。後続の新規native-28起動でその手順と実戻りpacketを確認した。
このcohortを最新版native-28の長時間受入や、ONU直結配線・物理NICの資格判定へ拡大しない。

## 直前版: wan-dns / native-27

`7769bc5`はmainへpush済み。その版でPPP学習DNSをresolverへ接続し、設定write/file reload、
raw lease注入、dnsmasq lease file/sticky sidecar、`dryRun`経路と不要な試験を削除した。
coordinatorがimmutable設定を一度準備し、`dnsconfig.Input`で学習値を別に渡す。
設定の所有者はcontrol transactionだけである。

現在の追加変更は、`dnsServers`の参照先をDHCPv4・DHCPv6・RAへ広げるもの。
参照形式は`PPPoESession/NAME`、`DHCPv4Client/NAME`、`DHCPv6Address/NAME`、
`DHCPv6PrefixDelegation/NAME`、`DHCPv6Information/NAME`、`IPv6RAAddress/NAME`。
NAMEは宣言済みservice名で、kindに対応する実worker roleを検証する。
一つのendpointにtyped addressと元のboot期限を持たせ、RAのサーバー別期限、
DHCPv4のACK期限、DHCPv6の情報期限と割当期限の交差を保つ。
IPv6リンクローカルDNSは取得元interfaceでscopeを付ける。受信時刻へ期限を付け直さない。
明示したprovider順と元のDNS address順を保ち、静的upstreamとの併用もできる。
旧routerdの`pkg/controller/dnsresolver:expandUpstreams`と既存transportを比較・再利用した。

DNS workerは同じlistener/poolを維持する。元期限はquery/probeの前後で確認する。
`GET /v1/services/NAME/dns-input`が唯一の学習値入力で、module socketは参照用APIのみ。
boot ABI1・worker ABI4・manifest v4・network journal9は変えず、compiler contractはnative-27。
[WAN/LAN設定例](../examples/wan-dslite-pd.yaml)にも学習DNSを追加した。

### 実施した確認と現在の限界

- `7769bc5` / native-26: PVE Ubuntu Linux6.8/amd64の署名PID1 catalog、独立RP-PPPoE3.15/
  pppd2.4.9、dnsmasq2.90、ISC DHCP4.4.3-P1でPAP/CHAP、PD→LAN RA/SLAAC、両family TCP/UDP、
  Renew、worker/session再作成、元期限、古い通知、server失効復旧、5秒confirmed rollbackを確認した。
  PPPと相手DNSへ直接到達できる状態で、resolverは失効したDNSを使わない。
  IPCPで別DNSを取得して旧listenerを停止しても、同じDNS workerとactiveなdig TCP接続で復旧した。
  DNSの通常8秒idle timeoutを、接続切断の不具合とは扱わない。
- native-26: 実DHCP→journal/ACK→coordinator→DNS、source失効と復旧は、native intentと旧設定importで成功。
  独立PPP/AFTR二WANのHealth/PBR切替・復旧、実TCP/UDPとworker再起動も成功した。
- native-26最終appliance: APK由来linux-lts6.18.58-0-lts、実PID1、同梱engine、独立AC/ISC/dnsmasq、
  別Linux LAN端末で学習DNSのUDP/TCP、両family TCP/UDP、PD、Renew、PPP再起動後の復旧を確認した。
  `make qemu-pppoe DHCP6_SERVER=/path/to/dhcpd`で再実行する。
- native-26: `make check build build-publisher build-linux RELEASE_ID=learned-dns`と実6ELFの署名catalog検査、
  staged secrets scanが成功した。arm64はbuild/catalog検査のみで実行していない。
- 現在のnative-27: 独立dnsmasqの実DHCPv4 ACK・DHCPv6 Information Reply・RAからDNSを学習し、
  三つのsourceそれぞれでLAN UDP/TCP問い合わせが成功した。RA/DHCPv6はリンクローカルDNSを実使用した。
  DHCPv6 IA_NAの実worker試験でも、取得・SIGKILL後の再接続・自然Renew・失効後の再取得に
  合わせたresolver UDP/TCP問い合わせが成功した。全体Go/Python/Race/vet・6静的ELFと
  その署名catalog/publisher検査も成功した。
  `make check-ra-route-refresh RELEASE_ID=wan-dns`の実worker経路で、30,000 UDP欠落なし・同じTCP、
  kernel route timerの更新・元期限失効と復旧も成功した。相手DNSへ直接到達できるまま、
  RDNSSの元期限後にはresolverがSERVFAILを返し、captureにも上流への当該問い合わせがなかった。
  dnsmasqのrouter lifetimeは90秒、RDNSSは120秒だった。経路失効とDNS失効を同一視せず、
  受信したDNS期限で待つ。追加した期限確認のため外側fixture時間だけを200→240秒にした。
  既存TCP/UDPの期限、packet数、元のprotocol期限は変えていない。
  最後のPPP PAP試験でも学習DNS変更・同じactive TCP・worker世代と元期限の検査が成功した。
  最新native-27のappliance・実上流長時間受入は未実施。
  初回peer設定の`ra-only`ではDHCPv6 serverが
  動かなかったため、情報要求を受ける`ra-stateless`へ修正した。DNS input公開直後はconsumer pollが
  未反映だったため、試験はconsumerの実pool反映を確認してから問い合わせる。
- build/Raceと同時実行した30,000 UDP試験は外側60秒で打ち切られた。通信成功にも、
  routerのpacket lossにも数えない。同じ60秒の単独実行では30,000 UDPと同じTCPが成功した。
  先行native-26でも負荷中のcommit HTTP clientが3秒で失敗した。単独実行は通ったが、
  負荷時の遅延原因や3秒以内の性能受入を解決したとはしない。

## 配布と再実行の前提

PPPは固定Alpine3.24.3のpppd/pppoe2.5.3-r0、musl、libpcap、OpenSSL provider閉包を配る。
manifest v4がengine/sourceを署名bindingで結び、supervisorはFD5を固定する。
helperは空のprivate chrootへ展開し、host pppd・設定・hookへのfallbackを持たない。
実接続で必要だったOpenSSL `legacy.so`を含む。対応source/APKBUILD/patchはSHA512検証する。
`make pppoe-engine`、`make check-pppoe`、`make check-pppoe-pd DHCP6_SERVER=/path/to/dhcpd`、
`make check-dual-wan`が既存の実peer経路である。ISC binaryは現在
`dist/peer-env/isc-dhcp/root/usr/sbin/dhcpd`にある。特定binaryがなくても黙ってskipしない。
Alpine linux-virtはPPPoE moduleを持たないため、factoryはlinux-ltsの必要module閉包を使う。
containerはPID1/control/shutdownまで。PPPの実通信・mount/device権限は未受入。

## 実上流で得た運用上の事実

既存uplinkの専用macvlanと別LAN namespaceで、実上流RA・DHCPv4・PD /60、
Linux SLAAC端末のIPv4 NAT/IPv6 HTTPS・UDP DNS、実AFTRのカプセル化往復を確認した。
ONU直結の配線確認や物理NICのqualificationを意味しない。

- 実上流DHCPv6 Replyの送信元は49153。547だけを許す独自制約は除いた。
- 通常のリンクローカルDAD完了を待つ。NoDADを前提にしない。広告には現在のinterface MACを使う。
- PPPにはEthernet MACがないため、明示DUIDまたは永続DUID-UUIDを使う。
- 実AFTRの受入ではDHCPv4 defaultを無効にし、選択routeとカプセル化往復を確認する。
  単に外へ到達できるだけではDS-Lite成功としない。
- RA route期限更新はkernel timerをin-place更新する。NetlinkのREPLACEでもREQUEST/ACKを維持する。
  更新前後のphysical timerをjournalに保持し、元のprotocol期限を延ばさない。
- native-24の別cohortは130回、約134分の定期新規HTTPS780回・UDP DNS3900回を終了コード0で完了した。
  自然T1のRenew/Replyと期限更新、最後のready・全protocol世代1を確認した。cohortは停止済み。
  先行同一TCPは上流FINで終了したため、この定期試験を同一TCP継続の合格にはしない。
  最新native-27の長時間受入は未実施。

## 出力の扱い

容量整理の指示に従い、過去のlog/capture・停止済みfixture・旧archive・不要な開発imageと
Go/Docker cacheを削除した。直近の整理で旧QA記録と一時bundle約2.1GiB、再生成可能なcacheを削除した。
過去の証跡は再保存しない。現在の診断に使う出力も完了後に削除し、ソース・実行手順と
結果・未検証範囲だけを簡潔に残す。対応sourceやlicenseは配布に必要なので保持する。

## 未完了と次の作業

1. 全実装計画、全更新復旧経路、外部engine、配備先と対象実機の受入は未完了。
   旧routerdの各モジュールとの比較を続け、任意optionの不良で有効な割当・経路まで捨てる受信処理を修正する。
   複数IA_NA/PDのDAD共存・resource readiness/RA連携も未完了。
   graceful Releaseの正常経路は上記で確認した。次は無応答時のretry/停止時間と、
   WAN設定変更時の依存順序を実peerで確認する。新しいDUIDの実回線試験を無用に増やさない。
   NET-06はrelayのHA・動的VRF/interface変更の受入、DHCPv6 server、dynamic pool/options・DNS・sticky/lease importが残る。
   今回の試験成功を製品全体・実機利用可能の判定には使わない。
2. NET-07の旧resource/profile、動的source・VRF経路変更、大規模RIBを実装・受入する。
   IPv4/IPv6のmultihop BFD/BGPは実通信したが、routed/VRFでの通常DAD起動、IPv6 single-hop VRF・link-local next-hop・ECMP等は未受入。
   HA-01の残るresource種別・phase tracking、外部secret参照、peer-aware移行、cluster、
   二native nodeの追加identity・RA・DHCP/PD/NAT session継続を実装・受入する。
3. 過去のBFD再接続timeoutの失敗request、IPv6 multicast未受信、初回IPv6 VIP TCP/DAD timeoutは
   原因未確定。IPv6 unicastの初回TCP timeoutと同時の選挙authority期限切れは以前の試験で一度再現した。
   syscall trace付き実行では再現せず、要求前後の選挙snapshotを
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
