# routerd2 開発引き継ぎ

更新: 2026-11-03 11:27 UTC。全体ゴールは継続中。

## 実private DNSとNextDNSの単体通信

2026-11-03、基準main `c371753`、通常build `6e0f020bf76c`（製品Go不変）で、保存homertの実private DNS/NextDNS設定を接続した単体39-resource実HGW手順が終了0。実NextDNS/Cloudflareの設定済みDoH上流すべてのHealthyを確認。安定した5転送先について独立LAN端末から両IP・UDP/TCPの20照会を行い、3先のSOA、2先のNXDOMAINとcore履歴の選択sourceを照合した。残る1先は事前UDP/TCP無応答から、試験中UDPのSERVFAILと後続NOERRORへ変化し、WAN captureで指定peer自身のNOERROR/NODATA応答を確認した。この先の固定SERVFAIL期待は試験側の誤りで、安定した4方式の受入には数えない。試験用local zoneは実suffixを隠さない名前へ変更し、DHCP/search/照会側も合わせた。新規WAN・host経路は追加していない。

同じ実行で通常の両IP Internet/DNS、DHCP scopes/sticky、BGP、NTP（host時計変更なし）、Tailscale/管理/運用記録、confirmed rollbackが通過。11巡回/自然PD更新0回、PD SIGKILL2.01秒・通常再起動4.87秒、停止時RA撤去/上流Releaseなし。全daemon再起動、HAでの実private DNS、全110-resource intent・実機資格は未受入。NextDNSは実endpointのprobe疎通で、全LAN照会がNextDNSを選んだという主張ではない。

再実行手順は[試験計画](test-plan.md)の「実設定のprivate DNS peerとNextDNS」。site intent/case/NextDNS URLは公開しない。通常Makefile build・3実行物のstatic link、Race Detector/vet・Python補助36件とpy_compileが通過。

## 単体構成のarchive接続と実履歴の期限切れ

2026-11-03、基準main `6e0f020` + native設定/fixture変更、実行build `799377d12628`（製品Go不変）で、archiveを接続した単体39-resource実HGW/Tailscale手順が終了0。保存homertのLogRetention sinksを再確認し、event/DNS/flow/firewall/DHCP fingerprintの保持へ既存syslog peerを接続した。通常通信・confirmed rollback・PD復旧の後、同じowner内で一時intentの期限2秒/間隔1秒を直列実行。archive専用の到達不能先で4 datasetの元記録を保持したまま、独立端末のDNS UDP/TCPとIPv4/IPv6 HTTPSが成功した。正常な受信先へ戻し、DNS query10件、終了済みflow10件、NFLOG拒否2件、DHCP fingerprint5件について元の識別子/時刻のsyslog受信と期限削除を照合した。元の7日/daily/vacuum intentへ戻した後も通信が成功した。

同じ実行で通常/特別DHCP scope・sticky、二段/条件付きDNS/両DoH、BGP peer障害復旧、nDPI/計数/SNI、NTP、syslog/OTLP、Tailscale/exit、両IP公開通信が通過。PD worker SIGKILL/正常再起動から再確認まで2.01/4.99秒、停止時のzero-lifetime RA/kernel撤去・上流Releaseなしも通過した。短時間2巡回・自然PD更新0回、全daemon再起動は今回未実行。通常Makefile build（後続build ID `6e0f020bf76c`）・3 static実行物、`make race vet`、Python36件・py_compile、3設定例のschema検証が通過。

再実行: `make check-uplink-retention DOCKER='sudo -n docker' UPLINK_INTERFACE=ens18 UPLINK_WAN_MAC=02:72:6f:75:74:03 UPLINK_CONFIG=examples/single-router-dslite-dhcp-dns.yaml UPLINK_SECONDS=20 UPLINK_TAILNET=1`。通常巡回と設定変更を並列に競合させず、同じownerが最後に保持試験を実行する。DB行/時刻やhost時計は変更しない。

これは明示した短いpolicyの自然期限・送信error/復旧と27件の受信確認であり、7日経過、lossless UDP配送、全signal/物理機器の資格ではない。変更した二台設定例はschema検証済みで、このarchive条件のHA実通信は未実行。

## 失敗した署名候補からの実通信復旧

2026-11-03、通常build `30054e56fdf6` / `799377d12628`の全factory署名bundleと独立RP-PPPoE/ISC/Linux LANで、更新→明示的復帰→失敗候補からの自動選択復旧が終了0。確認済み版へ再度rollbackする候補の起動時に、router namespace内の独立UDP processでDNS port53を占有した。実DNS workerのEADDRINUSEを観測し、11:03:26 UTC開始の既定120秒trialが11:05:26 UTCに失敗して製品自身がrebootを要求した。読取専用CLIで失敗digest、以前のactive、pending/attempt解消を確認。競合process終了後、保存選択/設定を手修復せずPID 1を再起動し、旧runtimeとpaired startupのrecovered記録、revisionの採用、更新後に追加したDNS回答、DUID、DNS UDP/TCPと両IP TCP/UDPが復旧した。high watermark=2、正常poweroff、captureの上流PD Releaseなしも通過。

同sequenceのprepare再送はhigh watermarkで拒否される。確認済み保存版の選択には既存のrollback APIを使い、境界を緩めない。DNS競合時の最初の期限処理ではrelease store busyも記録され、その後の停止処理で失敗状態が保存された。上記の実保存状態と復旧を確認しており、busyを候補正常確認へ読み替えていない。製品Goの変更なし。通常Makefile build/3 static実行物、`make race vet`とPython補助36件が通過。再実行は試験計画の`make check-runtime-update`。

これはOS待受競合による候補失敗と、外部競合解消後の復旧の確認。両buildの製品Goは同一で、同じkernelのPID namespaceを再起動した。実装/ABI変更を跨ぐ更新、kernel/bootloader再起動・停電、競合が続いた状態でのDNS提供、全homert profile/ONU/NIC資格は残る。PVE/QEMUと追加PBR調査、共有host時計の変更は行っていない。

## 通常runtimeの実通信更新・復帰

2026-11-03、通常署名bundleを実control APIでupload/stage/prepareし、実PID 1のrebootと再起動で `b3a0053f24c6 → 30054e56fdf6 → b3a0053f24c6` の更新・明示的rollbackが終了0。独立RP-PPPoE/ISC DHCPv6/Linux LANを使用し、全workerのrelease ID、実health watcherの自動confirm、現在の設定/DUID保持、DNS UDP/TCPと両IP TCP/UDP、high watermark=2の保持を確認した。候補でDNSレコードを追加してから旧runtimeへ戻し、その追加設定の両transportでの実回答も維持した。startup採用時は設計どおりrevisionが進む。正常poweroffとcaptureの上流PD Releaseなしも通過。

再実行は`make check-runtime-update UPDATE_BASELINE_DIR=<以前の通常build> DOCKER='sudo -n docker'`。候補はsource `30054e5`の通常Makefile buildで3実行物のstatic linkを確認した。試験側の固定revision要求を除き、実startup採用記録とrevisionの対応へ修正した。大きなbundleの送信はstreaming uploadを使う。補助`make test-tools`36件、`make race vet`、通常Makefile buildと3実行物のstatic linkが通過。製品Goは変更せず、既存の更新/復旧処理を実peerへ接続した。

製品Goは両buildで同一で、異なる通常release IDの実行物を選択・execする経路の確認。PID namespaceを同じkernel上で再起動した試験であり、kernel/bootloader再起動、停電、失敗候補の自動fallback、異なるABI/実装間の更新、全homert profile/ONU/NIC資格は残る。全構成のappliance NTPには独立kernel/時計の実行機が必要で、共有host時計は変更していない。PVE/QEMUは実行していない。

## 通常署名bundleの実PID 1・PPP/PD受入

2026-11-03、基準main `b3a0053` + 本変更の通常build `b3a0053f24c6`で、全25役割・5 engineの署名bundleを実receiverでstage/initializeし、Linux namespaceの実PID 1から起動した。独立RP-PPPoE/ISC DHCPv6/Linux LAN端末とのPAP・CHAPが両方終了0。DNS UDP/TCP、両IP TCP/UDP、自然Renew、server停止によるPD期限切れ/再取得、worker交換/DUID保持/古い通知拒否、confirmed rollback、PD/RA削除復元、PPP MTU変更、手動/control再起動、正常poweroffが通過。captureでMTU変更・手動/control再起動のRA zero-lifetime通知がPADTより前であり、上流Releaseなしと確認した。

実通信で検出した2件を修正した。停止指示前のReady状態で消滅したPPP linkへ経路を追加する競合は、停止指示後に次のowner巡回で現在状態を取得してから適用する。手動再起動の一斉停止は既存の設定変更用dependency drainへ統一し、PD/RAをPPPより先に撤去する。待ち時間延長・追加retry・新しい状態は入れていない。試験専用catalog/不正bundle生成を削除し、通常配布入口を使用する。旧「通知期限で生きたPPP routeを消す」要求と、PD削除時に参照RAを残す試験設定も修正した。

最終`make check`（Go/Race Detector/vet/Python36件）、変更後のruntimeplan Race、通常Makefile buildと3実行物のstatic linkが通過。試験用process/namespace/key/展開storeは終了済み。次は同じ通常配布経路で全homert profileの更新・復旧を確認する。

実行物SHA256: `routerd`=`01b1ec3c511766b0ead2a363856901a17f8ad775364bfe3ea79e1f4c244f7271`、`routerd-modules`=`0fd2f8e8cf0c71825179bd79f48de37f861dfa9cef2807de1a8b64f279b6fb93`。再実行は試験計画の「通常署名bundleのPID 1起動とPPPoE/PD」。これは実kernel/PID 1と独立peerでの配布boot・PPP/PD復旧の受入。ISPのPPPoE、ONU/NIC実機、全homert profileの署名更新/rollback、全34 packageの完成は残る。homert上流候補へPPPoEや追加NICは導入していない。

終了0（2026-11-03 07:42〜09:46 UTC）: source/build `df49035`/`df490351c3ff`、単体38-resource profileで実HGWとの自然Renew/Reply1組と期限更新、623巡回の両IP HTTPS・UDP/TCP DNSを確認した。自然更新後の619巡回時点まで全28 worker generation1/Ready=trueを観測。PD SIGKILL2.90秒/通常再起動5.92秒、全daemon後の全項目再確認105.73秒も通過し、identityと両端末/Tailscale通信、停止時のRA撤去を確認した。105.73秒は抑制/samplingを含む全項目時間で、転送断時間ではない。owner/process/namespace/stageは撤去済み。二段DNS・候補設定修正の別受入、既存socket継続、全homert/実機資格とは範囲を区別する。

通常build `842c47e32300`で新しい`make runtime-bundle`が終了0。25役割・5 engine・15 artifacts、527,315,456 byteの署名配布物へsourceと303 license資料を同梱した。実receiverでstage/verify/initialize/statusと後続候補のprepare/cancelが通過し、元のactiveと増加済みwatermarkを維持。独立OpenSSLも署名を検証した。入力はprivate snapshotへ固定し、nDPIのsourceコピーをproduction/build入力へ限定してroot private probeとsite boot treeの非収録を確認した。再実行は[配布手順](runtime-bundle.md)。これは配布物と保存選択の受入であり、このbundleの実機起動・全profile更新は残る。

基準main `1fa7cf0` + 候補設定の観測保持修正、通常build `1fa7cf099ae0`で単体39-resourceの実HGW手順が終了0。30秒のconfirmed commitでcoreのAレコードとLAN cache設定を変更し、両IP/UDP/TCPの候補回答と公開通信、無確認期限切れとcontrol SIGKILL後の元intent全体・回答・公開通信への復帰を確認した。候補疎通確認後から元の疎通を再確認するまで33.42秒/9.30秒で、転送断時間ではない。DNS編集と期限切れrollbackではWAN DHCPv4/RA/PDのPIDを維持した。PD SIGKILL3.21秒/通常再起動6.72秒、全daemon後の全項目再確認103.04秒も通過した。

候補準備で全ての観測を空にしていた処理を除き、同じworker設定のproducerと、同じ設定・依存関係の適用済みresourceだけを元の有限期限のまま評価する。準備段階は現在の条件履歴・イベント/derived ownerを進めない。小さな回帰確認では旧関数が無関係なMTU編集で稼働中consumerを停止することを再現し、修正後は維持、WAN自体の変更では旧観測を流用しないことを確認した。世代番号は他の退役workerからも引き継ぐため、番号だけで個々の再起動回数を推定しない。 `make check`（Race Detector/vet/補助36件）、最終のruntimeplan Race、通常Makefile buildと3実行物のstatic linkが通過。これは設定transactionと実通信の受入であり、署名runtimeの更新/boot、HA中の設定変更、全homert private peer、全34 packageの完成には数えない。

今回の補助確認: `make test-tools race vet`、3設定例の検証、通常`make build DOCKER='sudo -n docker'`が終了0。後続build IDは`9fe72fede476`で、3実行物のstatic linkを確認した。実通信で実行したbuild IDは上記`df490351c3ff`であり、製品Go sourceは同一。

基準main `9fe72fe` + 二段DNS設定、製品Go不変の通常build `df490351c3ff`で単体39-resource実HGW手順が終了0。LAN resolver（cache1000・queryLogなし）からcore resolver（cache10000・queryLogあり）へloopback UDP53で転送し、coreがzone/条件付き/学習DNS/DoHを所有する。実端末からの新しいqueryと同じIDのloopback往復・回答をcaptureで照合した。両IPのDNS/Internet、条件付き上流の停止/cache/SERVFAIL/復旧、coreのcache hit/履歴API/Web、DHCP各scope・sticky、BGP/NTP/Tailscale/運用記録が通過。PD SIGKILL3.16秒/通常再起動7.06秒、全daemon再起動後の全項目再確認103.15秒。後者は抑制/samplingを含み転送断時間ではない。自然PD更新0回・通信round2の短時間実行であり、長時間安定性や全homert private peerの受入には数えない。 同じ二段DNSの各48-resource二台profileも実HGWで終了0。両方向のDNS/DoH・履歴API/Web、DHCP更新/Release/再取得・sticky/fingerprint、PD identity、BGP/NTP/Tailscale/exitと運用記録を確認。同一TCPはSYN1回・209応答・最大応答間隔10.89秒、UDP239応答・19期限超過・最大応答間隔10.52秒（1秒返信予算）。全項目確認58.91/73.57秒はsampling/再取得を含み、転送断時間ではない。二台のprocess/namespace/stageは撤去済み。短時間の両方向受入であり、長時間安定性・全private DNS peer・実機資格は残る。

2026-11-03、製品code `df49035`・通常build `df490351c3ff`で `make check-doh-outage` が終了0。実Cloudflare/GoogleへTLS通信し、片側ずつTCP443を遮断して各16packetのdropとDown、解除後の両Healthyを確認した。cache無効の独立dig端末は全段階でUDP/TCPのNOERROR/A回答を取得。Cloudflare停止直後の初回回答1.043秒、以後の確認回答0.025〜0.063秒。新DNS node/client/process/stageは撤去済み。元の38-resource自然PD試験への障害注入は行っていない。公開DoH IPv4 transportの独立受入であり、DS-Lite/HA全体の障害受入には転用しない。 再実行条件は[試験計画](test-plan.md#公開dohの片側停止復旧)に記載。Go製品codeは変更していない。今回の補助チェック `make test-tools` は36件通過。

同じ稼働中の実DHCP割当を読み、その送信元へbindしてMTU/MSSを確認した。IPv4合計1500byte・DF=1の実pingが公開peerと往復し、WANでAFTRからの同一IDのIPv6断片2個（offset0/752、payload752/748）を観測した。TCPは同じ実tuple/sequenceでLAN SYN MSS1460→DS-Lite送信1414、受信SYN/ACK1420→LAN1414となり、HTTPSが成功。ここで確認したのは選択されたDS-Lite経路の往復・受信側外側IPv6再構成と双方向MSS調整。B4側送信断片化、実IPv6 PTB/外側MTU縮小の全条件や性能は未受入。[RFC6333 section5.3](https://www.rfc-editor.org/rfc/rfc6333.html#section-5.3)に照らし、1500byte DFを一律のICMP拒否条件にしない。

最新修正（基準main `a0ec3ac` + 本変更）: BGP/VRRPの元の期限より前に送信を打ち切る共通IPCの3秒制限を外した。AFTRは一つのループでtyped Input取得→snapshot通知を順に行い、入力世代を並列更新するgoroutine/channelを削除した。周期/取得budgetと元の依存期限は維持する。旧routerdのDS-Lite単一reconcileとcallerを照合した。supervisorの状態APIも管理ループへ要求して応答し、登録より古い状態を返すcache/mutexを削除した。producer未登録とAFTR入力世代不一致には、元のErrStaleを保ったまま原因を示すcontextを付けた。互換経路・retry・新しいepoch/stateは追加していない。

最終通常build `a0ec3ac167e8` + 本変更で実HGW二台47-resource手順が終了0。同一TCPはSYN1回・245応答・最大応答間隔11.54秒、UDPは260応答・19期限超過・最大応答間隔10.53秒（1秒返信予算）。全項目確認61.87/76.33秒はsampling/再取得を含む。両方向のDHCP更新/Release/再取得/sticky/fingerprint、PD identity、DNS/DoH、BGP/NTP/Tailscale/exit、管理、fresh nDPI/履歴/syslog/OTLPが通過。最終の再加入前まで両nodeのworker停止理由を照合し、AFTR409/BGP/VRRP timeoutは未観測。短時間の未再発であり、自然更新を含む長時間安定性はこれから確認する。

先行単体38-resource実行（状態API変更前）も実HGWで終了0。通常/予約/特別端末、4 DS-Lite、DNS/DoH、BGP片側停止、NTP/Tailscale/運用経路、PD SIGKILL2.07秒/通常再起動6.02秒、全daemon復旧102.79秒を確認した。ただし初期AFTR409は残り、登録観測の修正につなげた。この先行結果を最終状態API変更後の単体長時間受入へ流用しない。

AFTRの実ソケット/独立DNS試験と専用daemon手順が終了0。前者の500ms入力は1秒の取得周期より前に失効し、利用不可となった。fixtureは取得後のliteral選択と元の期限での撤去を確認できる2秒の明示grantへ更新し、製品の期限は変更していない。更新された入力callbackと既存検証を併せて確認した。最終`make check`（Race/vet/Python36件を含む）と通常static buildが通過。補助成功を通信/安定性の代用にしない。

以下は前checkpointの結果。

今回の修正: supervisorが停止中workerの期限切れheartbeatを観測全体の期限へ混ぜ、正常な兄弟workerまで失効させる処理を除いた。停止対象のReady=false、正常workerの元のheartbeat期限、全体の未準備状態は維持する。新しい状態・タイマー・再試行は追加していない。実fork/execのworker一つをSIGSTOPし、heartbeat timeoutから停止中へ入ると正常な兄弟を含むExpiresNanos=ObservedNanosになることを旧コードで再現し、修正後は同じ手順と既存heartbeat検出が通過した。Race Detector/vet/Python補助36件、通常Makefile build、3実行物のstatic linkも通過。

修正後の実通信: 基準main `85259c1` + 本変更、通常Makefile build ID `85259c1a892c`で `python3 scripts/uplink-ha.py --uplink ens18` が終了0。実HGWと独立端末/peerを使う二台47-resource profileで、初期・両方向の停止/再加入後の通常/特別DHCP更新・Release/再取得・sticky割当/fingerprint、両IP公開通信、条件付きDNS/両DoH、BGP、NTP、Tailscale/exit、管理、fresh nDPI/履歴/syslog/OTLPが通過。同一TCPはSYN1回・228応答・最大応答間隔11.50秒、UDPは246応答・17期限超過・最大応答間隔9.52秒。全項目確認60.75/73.57秒はsampling/再取得を含む。cold startでAFTR HTTP409、WAN停止、BGP/VRRP適用timeoutを再観測したため、安定起動/長時間安定性は未受入。

起動診断では、通常kernel適用0.2～0.4秒、負荷/停止時には1.7秒程度まで伸び、依存worker停止中の全体適用見送りも観測した。ただしBGP/VRRP effect timeoutはまだ再発しており、上記の巻き込み修正だけで原因全体を解消したとは扱わない。時間計測コードはすべて除去済み。別の先行実行は特別端末→独立gateway DNSの接続拒否、別実行は初期公開TCPのNo route to hostで終了した。DNS拒否の正確な原因は未確定で、次の実行では独立gatewayの待受/実応答を確認し、障害時のpeerログと100packetに限定したcaptureを既存owner内で取得する。これは合格への読み替えではない。

以下は前checkpointの結果。

最新実行: 基準main `c144132` + 本変更、通常Makefile build ID `c14413250ebd`、各47-resource profileで `python3 scripts/uplink-ha.py --uplink ens18` が終了0。実HGW、独立BusyBox端末4台、FRR2台、chronyd、Headscale/Tailscale、syslog受信機、公式Collectorを使用。両方向の停止・再加入で、通常/特別scopeの実Renew/ACK・DNS/公開HTTPS、特別3端末のRelease/再取得とsticky同一割当、再取得した現用機のfingerprintを確認した。新規公開通信の双方向nDPI計数/TLS SNI、実管理拒否の履歴、元の記録時刻とnode識別を照合したfresh syslog/OTLP logs/metrics/tracesも初期・両方向の切替後に通過した。

同一TCPはSYN1回、249応答、最大応答間隔10.82秒。同一UDPは287応答・15期限超過・最大応答間隔8.52秒（1秒返信予算）。両方向のNAT tuple/mark、DUID/IAID/prefix、再加入/同期/非preempt、DNS/DoH/BGP/NTP/Tailscale/管理を維持した。全項目確認まで80.40/73.30秒には30秒sampling待ちと特別端末のRelease/再取得を含み、転送断時間とは区別する。

VRRPの余分な2秒適用待ち設定を削除し、元の選挙期限の残り時間へ統一した。期限切れ判定とwithdrawalは維持する。ただし今回のcold startでもBGP/VRRPの適用待ちtimeoutと依存workerの再起動を観測したため、これだけで起動安定性が解決したとは扱わない。次はcoordinatorの実適用待ちと依存worker停止の時間関係を観測し、待ち時間を延ばす対処ではなく原因を直す。

切替後fingerprint待ちの先行失敗はrunnerの前提誤り。配布用dnsmasq2.92_p2と独立BusyBoxの実割当→再起動→Renewで、ACK後も`DNSMASQ_DATA_MISSING=1`・要求optionsなしの通知を再現した。upstream helper/lease/rfc2131実装も照合し、通常Renewには取得時metadataがないことを確認した。データのない通知からfresh記録を捏造せず、実Renew/通信と実Release/再取得/fingerprintを別に確認する。fingerprintはlease同期の対象ではない。保存設定も再読し、ens20（homert02）/eth2（homert03）は管理用、上流順序はDS-Lite群120→別DS-Lite80→LAN静的gateway60、PPPoE候補なしと確認した。

通常Makefile build ID `875dfa6f1f84` + 本変更で、実HGWへ接続した二台47-resource profileの拡張手順が終了0。条件付きDNS/DoH・BGP・NTP・両IP Internetに加え、独立Headscale/Tailscale端末とのLAN→peer IPv4/IPv6、peer→LAN IPv4のTCP/UDPを両方向の現用停止・再加入後に確認した。LAN subnet routeは独立端末で手動変更せず切り替わった。最後に現用nodeを明示選択したIPv4/IPv6 exit HTTPS/UDP DNSも通過（同一exit node IDの自動引継ぎの受入ではない）。両nodeへのSSH鍵認証・保持host key、現用側のloopback/管理DNS・Webの実DNS履歴、LANからの管理拒否も通過した。

全項目の復旧はa→b36.82秒（途中probe失敗1回）、b→a36.13秒（3回）。同一公開TCPはSYN1回、117応答、最大応答間隔11.68秒。同一UDPは126応答・17欠落・最大応答間隔9.52秒（1秒返信予算）。両方向でNAT tuple/mark、DUID/IAID/prefix、実DHCP更新、再加入/同期/非preemptを維持した。切替断の短縮と長時間安定性は引き続き必要。

TailscaleのLAN通信は、両側で常時広告していた旧二台例で実際に失敗した。現用からtailscale0へ出るSYNをpacketで確認し、広告を現用権に合わせた設定へ修正した。独立制御サーバーの明示policyは試験userのLAN/exit広告だけを事前許可し、切替後の手動承認を合格条件にしない。各nodeの保持鍵・名称は別のまま再利用する。

BGPは観測の元の5秒期限より短い2秒で適用待ちを切っていた。余分な`effect_timeout_milliseconds`設定をBGPのproducer/consumerから削除し、RIB走査に使った時間も差し引いた元の残り期限で待つ。期限を更新して待ち直す処理は追加しない。Race Detector・vet・Python補助検査36件・通常build・3実行物のstatic linkが通過。短時間の二台通信は確認済みだが、単体の長時間BGP安定性は再受入が必要。VRRPの適用待ち再起動と初期AFTR HTTP409も残る。

今回の補助検査: `make check`（Race Detector/vetを含む）、通常Makefile build、3実行物のstatic link確認が通過。最初の同時実行ではstorage guestの150ms診断待ちが一度失敗し、実通信終了後の無変更再実行は終了0。期限や期待値は変更していない。終了した実通信ownerはnamespace/process/key/stageを撤去済み。

## 今回の自然更新とHA修正

自然PD更新の長時間試験は終了0。通常build `b03debf15982`、単体38-resource profile/runner `5861b7e`で、実HGW source port49153から05:24:16 UTCのRenew/Replyと期限更新、678回の両IP HTTPS/DNS確認を通過した。PD SIGKILL後2.22秒、通常再起動後7.05秒、全daemon再起動後112.21秒で復旧し、DUID/IAID/prefix・両端末/Tailscale通信を維持した。正常停止のzero-lifetime RA、kernel撤去、上流Releaseなしも確認した。BGPはgeneration17まで再起動したため、これは自然更新と復旧の受入であり長時間安定性の合格ではない。最新のRA寿命/SNAT/HA修正を含む全体の資格にも転用しない。stage/process/namespaceは撤去済み。

二台用設定の両側常時LAN広告では、独立FRRの戻りが待機側へ入りSYN/ACKをINVALIDとして破棄する実障害を確認した。保存中homertのBGPのexport/connected intentと旧routerdのrender/route同期を参照し、このstateful二台profileのBGPは現用権で有効化する。新しい通信手順は両FRRの実経路が現用側だけを向くこと、端末TCP/UDPの非NAT往復、条件付きDNS/子suffixと両DoH上流を切替前後に確認する。

製品の修正は、経路適用に不要なkernel route dumpのsort削除と、VIPが未公開の待機/準備通知を既に適用したVIP不在で応答する処理。これにより、準備を進める通知がその準備の完了を待ってtimeoutする依存を外す。準備条件が満たされた現用化とVIP撤去は実適用を待つ。診断用のstack timerは除去し、通常Makefile buildで上記の最終確認を実施した。

## 前checkpointの単体結果

38-resource native profile、基準main `b03debf` + LAN配布設定、build ID `b03debf15982`で実HGW統合gateが終了0。通常・予約・OUI・特定MACの独立端末5台で、通常gateway/DNS .1と特別gateway/DNS .253を実ACKから設定し、公開HTTPS/UDP DNSが往復した。通常/予約端末はNTP2アドレスとdomain-searchも取得。実Releaseで3日分の保持期限が保存され、端末IP撤去後も別MACの同アドレス要求には別leaseを配り、元MACには保持アドレスを再配布した。全daemon restart後も特別端末3台のRenew/ACK・通信とfingerprint APIが復旧した。LAN RAのhigh優先度/MTU1500/valid最大7200秒/preferred最大3600秒を実packetで確認。上流選択、4 DS-Lite、DNS/DoH、nDPI/計数/SNI、fresh syslog/OTLP、BGP/NTP/Tailscaleも通過。PD worker SIGKILL/通常再起動後の復旧2.96/5.85秒、全daemon restartから全確認102.76秒（60秒抑制と30秒sampling等を含む）。2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。自然T1・既存socket継続・3日/7日経過削除・全homert/HA/実機資格は今回の範囲外。

- 製品Goは`b1a3e48`から不変。今回Race/vet、Python checks/static build/設定検証が通過。7日/daily/vacuumをoperational event・DNS/flow/firewallへ設定した。全datasetの保持/期限削除を今回実証したことにはしない。
- backupをDHCPとした誤った追加は削除済み。実homertは静的IPv4。実設定の低優先候補は既存LAN上の静的gateway。ens20追加WANと新規static IPを前提にした依頼は撤回済み。4 DS-Liteの個別疎通に続き、既存優先順位の選択と両LAN端末の通信も受入済み。
- 旧routerdのBGP広告callerとTailscaleのrender/chainを参照した。専用CA必須という制限を除き、OS標準CAを専用rootへ渡す。upstream既定のsubnet SNATを維持する。
- homert02/03の実設定は`chatty.lain.local:~/homert02/hosts/{homert02,homert03}/router.yaml`に各110 resources。秘密は転記しない。native単体例38 resources、各homert例47 resourcesへ拡張し、実HGWで二台の初期通信/片方向復旧を確認。全設定受入ではない。
- 自然PD T1、WAN遮断、Health→PBR、appliance/updateの別gateと未受入範囲は[試験計画](test-plan.md)に簡潔に残した。raw log/pcapは保持しない。

HA基盤の最新確認: 基準main `9fb64f4`の通常実行物、build ID `9fb64f4267e9`で`make check-ha-state`が終了0。独立Linux namespace/PID1の2 product processとclient/serverで、A→B/B→AのVRRP/NAT引継ぎ、同一TCP socketとUDP mapping、同期済みDHCP leaseの実Renew/ACK、worker/conntrack engine再起動を確認した。TCP再接続なし（captureのSYNは1回）、最大応答待ち1.665秒。UDPは50ms送信/200ms返信予算で3009送信・2942受信・67期限超過、最大無応答1.110秒（このfixture条件での測定）。同期断中の独立割当/更新と復帰、未設定証明書の拒否、実lease期限での失効、3日sticky期限を変えない再起動/引継ぎ、保持中の別client割当と元client再取得も通過。設定削除で同期workerを回収し、元のconntrack sysctlへ復元した。実HGWの38-resource全体を二台化した結果や、14-resource例・実homert二台の資格とは分ける。

## 二台profileの初期通信

2026-11-03 04:12 UTC: 通常Makefile build ID `a3444d519fb0`（製品Goはwildcard NTP listener修正`19682da`を含む）と各47-resource二台profileで、実HGWの共有WAN RA/PD、共有LAN IPv6 identity、VRRP準備後公開、実DHCP端末のIPv4/IPv6 HTTPS・UDP/TCP DNS・独立chronyd `-Q`によるVIP NTP応答が通過した。現用aの正常停止後、同じ端末設定のままbで全項目が復旧し、停止開始から再確認まで17.01秒、途中の試行失敗3回。RA/NA packetとkernelも観測した。無停止・既存socket継続・逆方向復帰・全機能安定性の受入ではない。初期のVIP DNS拒否は`accept_mode: true`で修正した。単発失敗で試験を止める条件は廃し、断と復旧を記録する。

NTPのワイルドカード待受を許可する製品修正`19682da`はmainへpush済み。Race/vet・通常build・3実行物のstatic linkとPython checksが通過。二台profileの全サービス/両方向復帰は残作業。

## 次の作業

1. cold startで再現したBGP/VRRP適用待ち・依存worker再起動の原因を直し、同じ単体/二台profileで起動と長時間安定性を受け入れる。初期AFTRのHTTP409/restartと切替断も残る。
2. 今回通過した二台の特別scope・nDPI/履歴・syslog/OTLPに続き、未確認の保持期限・archive障害/復旧と長時間動作を実peerで受け入れる。短時間通信の通過と安定性を分ける。
3. 保存homert全意図と保持dataset、MTU/期限/更新/配布、残る34 work packageを完了する。対象ONU/NIC・実機性能・arm64・cloud等は未受入。各実通信の直前に[試験計画](test-plan.md)全体を読む。PVE/QEMUと追加PBR調査は実行しない。

全体開始から9日20時間6分、記録済み期限から4日14時間38分経過（07:35 UTC）。全体ゴールは継続中。終了したstage/key/capture/log/重複build outputを撤去し、次の観測で必要な実行物・pinned peerだけを残す。
