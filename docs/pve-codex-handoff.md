# routerd2 開発引き継ぎ

更新: 2026-11-03 06:10 UTC。全体ゴールは継続中。

通常Makefile build ID `875dfa6f1f84` + 本変更で、実HGWへ接続した二台47-resource profileの拡張手順が終了0。条件付きDNS/DoH・BGP・NTP・両IP Internetに加え、独立Headscale/Tailscale端末とのLAN→peer IPv4/IPv6、peer→LAN IPv4のTCP/UDPを両方向の現用停止・再加入後に確認した。LAN subnet routeは独立端末で手動変更せず切り替わった。最後に現用nodeを明示選択したIPv4/IPv6 exit HTTPS/UDP DNSも通過（同一exit node IDの自動引継ぎの受入ではない）。両nodeへのSSH鍵認証・保持host key、現用側のloopback/管理DNS・Webの実DNS履歴、LANからの管理拒否も通過した。

全項目の復旧はa→b36.82秒（途中probe失敗1回）、b→a36.13秒（3回）。同一公開TCPはSYN1回、117応答、最大応答間隔11.68秒。同一UDPは126応答・17欠落・最大応答間隔9.52秒（1秒返信予算）。両方向でNAT tuple/mark、DUID/IAID/prefix、実DHCP更新、再加入/同期/非preemptを維持した。切替断の短縮と長時間安定性は引き続き必要。

TailscaleのLAN通信は、両側で常時広告していた旧二台例で実際に失敗した。現用からtailscale0へ出るSYNをpacketで確認し、広告を現用権に合わせた設定へ修正した。独立制御サーバーの明示policyは試験userのLAN/exit広告だけを事前許可し、切替後の手動承認を合格条件にしない。各nodeの保持鍵・名称は別のまま再利用する。

BGPは観測の元の5秒期限より短い2秒で適用待ちを切っていた。余分な`effect_timeout_milliseconds`設定をBGPのproducer/consumerから削除し、RIB走査に使った時間も差し引いた元の残り期限で待つ。期限を更新して待ち直す処理は追加しない。Race Detector・vet・Python補助検査36件・通常build・3実行物のstatic linkが通過。短時間の二台通信は確認済みだが、単体の長時間BGP安定性は再受入が必要。VRRPの適用待ち再起動と初期AFTR HTTP409も残る。

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

1. BGPの元の期限へ統一した変更を単体の長時間実通信で再受入し、残るVRRPの適用待ち再起動と二台の切替断を直す。初期AFTRのHTTP409/restartも残る。
2. 二台の特別DHCP scope、nDPI/通信履歴・firewall履歴・syslog/OTLP全経路、保持期限と再起動後を実peerで受け入れる。今回確認したTailscale/管理/DNS/DoH/BGP経路と長時間安定性を分ける。
3. 保存homert全意図と保持dataset、MTU/期限/更新/配布、残る34 work packageを完了する。対象ONU/NIC・実機性能・arm64・cloud等は未受入。各実通信の直前に[試験計画](test-plan.md)全体を読む。PVE/QEMUと追加PBR調査は実行しない。

全体開始から9日18時間41分、記録済み期限から4日13時間13分経過（06:10 UTC）。全体ゴールは継続中。終了したstage/key/capture/log/重複build outputを撤去し、次の観測で必要な実行物・pinned peerだけを残す。
