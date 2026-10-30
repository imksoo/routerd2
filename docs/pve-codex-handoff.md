# routerd2 開発引き継ぎ

2026-10-30 JST。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)、
[試験計画](test-plan.md)、[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

## 2026-10-30 18:31 JST: 明示的な接続計数と二台daemon試験

`accounting-native87`（契約87/network26）は旧`SysctlProfile/router-linux`の`nf_conntrack_acct`を`forwarding.conntrack_accounting`へ接続する。既存network writerが変更前値を保存し、撤去時に復元する。collection workerがsysctlを変更する経路は作らない。新しい接続だけにpacket/byte計数が付く。配布物から欠けていた`nf_conntrack_netlink`を通常bootとkernel試験の依存へ追加した。旧86 imageのままではnetlink dumpがEINVAL、module追加後の旧image使用もmodule不足で試験開始前に停止したため、87通常imageを生成して確認した。

配布Linux6.18.59の実TCPでIPv4の往路6packet/324byte・復路5packet/272byte、IPv6は両方向5packet/372byteを取得し、撤去後に元の0へ復元した。`make qemu-network IMAGE_DIR=dist/image-accounting-native87`は42.14秒/終了0。関連4 packageのRace、全vet、CGO0両arch・署名六ELF/catalog/publisherと通常Make/APK image生成も終了0。使い捨てnetwork stateを再生成し、旧試作形式readerは加えていない。

同じ87・明示accounting付き51resource/NAT26件ずつのLinux名前空間二台で、ISC/四AFTR/二FRR/IX代替/DoH/chrony/Headscale・公式TS/普通・特殊dhcpcdの初回同時通信、両方向のdaemon喪失切替と再参加を完走した。同じTCP sequence spaceとUDP NAT対応を保持。再参加後までのUDPは1344送信/1216受信/128期限切れ、成功間最大3.789秒、TCP最大3.736秒、DNS843件中84失敗。TSは各方向9件の初期失敗を記録した。損失ゼロではない。

これはhost名前空間のdaemon喪失であり、旧kernel/NICも停止する二guest KVM/物理機器の切替ではない。NTPは`-x`/時計権限無しの問い合わせ試験。native86単体KVMの時計補正・guest再起動結果と混同しない。IX既存session/TS出口/自然長期更新/実二回線/全homertと全34項目は未受入。小さい私有結果`/tmp/routerd-ha87-combined-success/`を残し、完了stageは削除する。次は計数を使う通信量記録とDNS/RA/OTLP変更を統合し、同時利用・二台受入へ進む。

## 2026-10-30 18:11 JST: 通常配布物の隔離KVM起動・通信・再起動

公開`2d9d4a0`相当の`route-read-flat-native86`通常Make/APK image（Alpine3.24.3/Linux6.18.59）を、PVE管理下の専用4CPU/4096MiB/KVM guestで完走した。独立ISC・四Linux AFTR・二FRR・IX代替Linux・DNS/DoH・chrony・Headscale/公式Tailscaleと、自動取得する普通/特殊dhcpcd端末を使用。製品はPID1。PVE既存VM/bridge/NICは変更せず、試験EthernetはQEMU hub/streamとSSHで隔離接続した。試験VMは停止・削除済み。

通常DAD、自動取得DNSのA/AAAA UDP/TCPと両IP TCP/UDP、条件付きDNS、両BGPの四prefix、特殊端末の取得gateway/resolver、TS三広告LANとの双方向TCP/UDP、配布NTP二台を確認した。guest全体再起動後に同じ経路とDUID/NodeIDを確認し終了0。起動後4.049秒の観測でsupervisor readyだったが、端末の最短接続時刻を測った値ではない。自然PD更新/既存TCP連続/物理NIC/ONU直結/全homert設定と全34項目の受入ではない。

NTPは同期flagだけではLAN端末が採用しなかった。初期skew1,000,000ppm/大きいroot dispersionの実packetと独立chronydの拒否を確認。既定poll/iburstの周波数推定後、最初の採用検査開始から40.942秒・5回目で製品NTPを採用し、再起動後は4.344秒・初回で採用した。もう一つの配布先も両起動で採用した。製品timeout、poll、client maxdistanceを緩めず、実採用を最大300秒観測した結果である。

先行KVM走行は試験SSHのpipe待ちと中継QEMUの`-S`指定で停止した。後者は独立Linux二端末のpingで、一時停止中失敗/通常実行成功を確認して修正した。製品未起動・未接続を起動成功に数えない。完走前のNTP初回8秒失敗も合格とはしない。大きいstageは削除し、現在必要な小さい私有結果は`/tmp/routerd-kvm-native86-success/`、実行logは`/tmp/routerd-kvm-native86-vm-fourth.log`。旧image5組と完了harness7組約1.7GiBも削除した。次は明示設定によるconntrack計数と履歴の接続を進め、同時構成の二台/実運用受入へ戻る。

## 2026-10-30 17:55 JST: native86統合・実HGWと起動環境

`90f9d96`の旧OTLP LogSink取込、DHCP由来DNS、Firewall履歴を統合した（契約86/network25）。旧OTLPSink.Emitは送信していなかったため既存Telemetry senderだけを使う。DHCP名は有限な貸出所有者へ渡し、更新・実RELEASEでA/PTRを変更/撤去する。Firewall履歴は旧logstore.RecordBatch/ApplyRetentionを使い、保存障害がpacket収集を待たせない。新しい外部daemonは追加していない。

`route-read-flat-native86`では、policy ruleの登録段階で一覧を一回、経路一覧を一操作で一回取得する。事前の所有確認は維持する。routeSlotはtableを含むため、重複したtable別mapと引数を削除した。アドレス/リンク変更後は新しい観測を取得し、所有・元期限・書込み前の検査を維持する。`make check`（Race/vet含む）、CGO0両arch、署名六ELF/catalog/publisher、通常Make/APK image生成が終了0。実5WAN選択、Health→PBR切替/期限/再起動/rollback、weighted ECMPの全三fixtureも通過。期限試験は両候補の2500ms max_ageを明示し、writer起動と実際の元期限撤去を分けた。製品の期限を変更していない。全体Raceの初回は大設定CLI確認の60秒期限超過で失敗（96.53秒）。同じ試験は単独49.76秒、再実行の全体checkも変更せず通過。負荷下での性能改善とはしない。

同じ最終実行物で、独立dhcpcd/BusyBoxと実HGWを90秒/9巡使い、自動DHCPv4・RA起動のDHCPv6情報・SLAAC、取得DNSの両IP UDP/TCP、条件付きDNS障害分離/復旧、両IP HTTPS、両端末の自然DHCPv4更新が通過した。PD SIGKILL後1.49秒、通常交換4.06秒、全daemon交換5.32秒で通信が復帰しDUID/IAIDを保持。停止時RAゼロ・kernel撤去も通過。自然PD Renewは0組（元T1まで約7187秒）であり、自然更新・ONU直結・物理NIC・連続TCPの受入ではない。保存identityは維持した。logは私有`/tmp/routerd-route-read-flat-native86-hgw.log`。

同じ実行物のDHCP名更新/実解放、Firewall履歴の両IP通信/保存/転送失敗と回復、OTLPの独立collector/重複配送防止も通過。OTLP中の並行UDPは69往復/失敗0。各fixtureのログは作業treeの`dist/route-read-flat-native86-*`に一時保持し、不要な生capture/VM stageは削除する。全homert同時構成・全34項目の完了ではない。

通常factory `route-read-native86`（最終map平坦化前）の512MiB/2CPU/TCG全profileは依然不合格。network revision1 applied、projection3 applyingで適用応答timeout、端末DHCP未取得。経路取得削減だけで起動解消とはしない。小さい私有記録`/tmp/routerd-route-read-native86-failure/`を残し停止stageを削除した。以前の診断付きnative85はApply9.589秒が正常帰還し、interface2.330/routes2.590/firewall1.616秒、保存3回840+485msを観測した。最終保存の停止や循環lockを確認した結果ではない。

実homertの読取りで両機は4CPU/約4GiB/KVMと確認した。512MiB/2CPU/TCGはsmoke fixtureの条件で最小要件ではない。rootから既存PVEへ信頼済み鍵で読取り接続し、KVMと空き約48GiBを確認した。本番VM/NICは変更していない。次は4CPU/4GiB/KVMの隔離guestへ同じ通常配布物と独立peer構成を接続し、起動・端末通信を判定する。単純な期限延長候補は採用しない。

## 2026-10-30 16:33 JST: native85統合候補

`80c5892`の設定ACK保存削減と、入口サービス`71e6be2`/`2c6c232`を統合。契約native85/network journal25。`make check-ingress RELEASE_ID=integrated-native85`は実HTTPS対向/端末・kernelによる転送先切替、VIP/port限定hairpin、全宛先不健全時のDNAT撤去とICMP拒否、VRRP優先度150→90→150を通過した。関連7 packageのRace、vet、CGO0 amd64/arm64ビルド、署名六ELF・catalog・publisher検査も終了0。`make check-ingress`へコマンドを集約した。静的端末の入口通信だけの検査であり、全k8s構成・二台・ARM実行の受入ではない。[入口の範囲](ingress-acceptance.md)。

通常配布イメージの全構成起動は直前の`setting-journal-native84`で不合格のまま。保存回数削減を起動改善とは報告しない。二台83/85の通信一巡と、別の83走行のeffects期限超過/TCP断を区別する。今回、最初のeffects失敗を捕捉する準備を付けた走行では失敗を再現しなかった。並行担当の容量制限で作業が停止したため主担当が引き継いだ。未採取のstackを採取済みにせず、診断用signal後の動作は受入に使わない。

## 最新85の二台通信profile: 両方向切替・再参加

`integrated-native85`（契約85/network25、保存削減・入口変更込み）の隔離Linux名前空間の二台profileは、独立ISC/四AFTR/二FRR/IX代替/DoH/chrony/Headscale・公式TS/普通と特殊のdhcpcdで両方向切替・再参加を一巡通過した。自動取得、初回DNS四種と条件付き24件、IPv4/IPv6 TCPUDP、二BGPの四prefix、IX特殊端末、両機NTP、TS三広告LANの双方向通信を同じ構成で確認した。

同じTCP接続とUDP NAT対応を両方向で保持した。TCP最大応答待ち4.366秒。再参加後を含む最終の独立50ms UDPは1351送信/1228受信/123期限切れ、成功間最大4.198秒。DNSは837試行/78失敗。TSのTCP/UDPは各切替後の初回成功が約11.1/12.6秒で、各方向9試行の失敗を記録。全経路再確認は各約17.7/17.5秒。損失ゼロや即時復旧ではない。通常DAD、共有DUID、再参加した両NodeID、non-preempt、実packetの一TCP sequence spaceを照合した。IX既存session、TS出口、自然長期更新、分断/全異常組合せ、物理機器は未受入。

完走走行は`/tmp/routerd-homert-ha85.xV01jJ`。最初のeffects失敗を捕捉する準備も付けたが失敗を再現せず、診断signalは送っていない。直前の83も一巡通過したが別走行ではeffects期限超過/TCP断を検出しており、今回の成功を間欠障害の解消とはしない。85の最初の二試行は私が実行版定数と旧`log_prefix`を私有fixtureへ残したため、製品起動前に停止した。現行compilerでzone全体を再生成し、51resource/NAT26件ずつを85で事前検証した後の実行である。旧形式readerは作らない。切替失敗時も観測threadを終了する私有パッチも反映済み。

83の小さい結果は`/tmp/routerd-ha83-combined-success/`、85は`/tmp/routerd-ha85-combined-success/`へ残し、完了した大きなstageは削除する。これは通常factoryの全構成起動を受け入れた結果ではない。次の起動診断は実際に期限を消費する処理の特定に絞り、同じ全体起動を修正/新たな観測なしに反復しない。

## 2026-10-30 15:30 JST: 単体の同時利用と全daemon再起動

`route-policy-native83`（契約83/network journal23）は、隔離Linuxの実supervisor/control/network/全workerと独立ISC・FRR二台・IX代替Linux・dnsmasq/dnsdist・chrony・Headscale/公式Tailscaleで、次の経路が通過した。

- 取得resolverによるA/AAAA UDP/TCP、IPv4/IPv6 TCP/UDP、両FRRの4接頭辞、条件付きDNS24件。
- 特殊DHCP端末の取得gateway/自動DNSによるIX通信、配布NTP二台、Tailscale三広告LANとの双方向TCP/UDP。
- 全daemon再起動後の同じ経路、DUID/NodeID保持。DNSは55件中12件失敗を記録し、全4種の復旧は8.932秒。VIP ACTIVE/PD/取得resolverのlistener反映は8.701秒。BACKUPのnode-readyを通信復旧と同一視しない。

私有の最初の実行は`/tmp/routerd-homert-host.jPYng9`、logは`/tmp/routerd-homert-native83-host.log`。別の同版走行`/tmp/routerd-homert-host.XvDqsv`では停止前からDNS4種と両IP TCP/UDPを独立測定し、9.540秒で全8種が復旧した。DNSは67件中12失敗、新規TCP/UDP transactionは69件中27失敗を保持。IPv4 TCP/UDPの成功間最大間隔は6.510/7.944秒、IPv6は6.417/6.399秒。同じ既存socketの保持試験ではない。再起動後の全経路・取得値・DUID/NodeID、IPv4の意図したSNATとIPv6送信元維持も通過。最初の8.932秒の走行とは別結果である。NTPは両世代ともhost profileの`-x`で時計を変更しない。独立TS peerとの経路広告とLAN通信までで、同時構成のTS出口、長期自然更新、実二回線、ONU直結、物理NIC、Alpine時計補正の受入ではない。全homertの未実装履歴等と全34項目も残る。

**実装の修正**: 実送信時の1 packet/60 bytesで先行table52へのSYNを捨てた`flow_path_guard`を削除。候補WAN表の静的blackholeと設定所有ruleへ移した。空bucketでもPREROUTINGで捨てず、packetだけのNoPath markとmain直前のblackhole ruleを使う。全候補障害・期限失効時も先行経路を維持し、mainへの誤転送を止める。NoPathをconntrackへ固定しない。独立Linuxの実daemon経路試験86.89秒で、全5候補断中の先行TCP/UDP、到達可能なmain宛への識別payload漏出0、同じUDP socketの復帰、選択/60秒復帰hold/confirmed rollback/元期限失効/network再起動が成立。これは単独で公式TS全障害や物理機器を受入した証拠ではない。

DHCPでは`891af85`のVirtualAddress参照と既存HA owner、`292e5ee`のdnsmasq shared-networkによる仮想待受/二次範囲を再利用し、`6836afb`の非管理interface状態を取り込んだ。旧詳細ログも接続。各元版の独立端末/二台試験は成功したが、今回の83単体を全設定二台へ拡張しない。設定intentからの必要な所有を保ち、アドレス複製・別daemon・systemdは追加しない。

試験側の起動前NIC UP、IX対向の共通Internet宛先不足、取得RDNSS経由だけ異なる試験名、手動移植で欠けたlan-ha↔tailscale0のtrust-zone許可も修正した。製品のweight変更、resolver固定、IPv6無効化、NoDADや固定待ちによる補修は行わない。

83の全`make check`（Race/vetを含む）、CGO0両architecture build、署名六ELF/catalog/publisher検査、通常Make/APK factory image生成は終了0。通常factory83の512MiB/2CPU/TCG VMは起動不合格。networkはappliedになったが全projectionはapplying、短いVRRP等の適用要求timeoutとworker交換を繰り返し、端末通信へ到達しない。私有log`/tmp/routerd-route-policy-native83-vm.log`。循環lockや性能原因の特定・解消とはしない。以前の一時計測の初回Apply4443ms中、保存27回/2755ms、次10777ms中保存1637msだった。計測コードは撤去済み。停止済み旧VM4組約665MiBと専用作業tree等を削除し、現在必要な小さい診断だけ残した。

OTLPログworker `874bc22`、ManagementAccess/CLI `341c5cd`、履歴保持 `d824c8a` は統合済み。履歴保持は統合版実peerで保存先障害/復旧・control再起動・元記録照合、端末UDP342往復失敗0。対象はoperational eventのみで他の履歴・native DPIは未実装。全34項目と固定期限達成は未完了。

## 2026-10-30 16:10 JST: 統合済みの管理・観測機能

直前の公開済み基準は`c158998`。次の変更は統合・公開済みであり、全体受入の完了ではない。

- SSHの`update rollback`を既存の検証済み旧版予約処理へ接続。`4f22e00`で実SSH操作・予約・取消・設定revision/generation照合が38.86秒で成功。再起動後の旧版復帰はこの操作試験の対象外。
- `18f36d4`で旧BGPの`communities.send: standard`を受理し、旧実装と同じ送信設定へ変換。独立FRRの通常BGP、IPIP、DHCPv4由来IPIP、二peerの実通信が全て成功。関連Race、vet、両architecture buildと署名六ELF検査も終了0。
- `671038f`のDNS query履歴保持は旧`logstore.ApplyRetention`を既存resolverへ接続する。before-delete UDP archiveの失敗でDNS応答を止めない。`09a3f6a`のFirewallEventLogは旧NFLOG readerを再利用し、既存ログ所有者へ構造化packet記録を渡す。独立端末による各元版の障害・復旧・再起動・撤去は通過。統合版`history-native84`でも実peerは成功。関連Race、vet、CGO0両architecture buildと署名六ELF検査が終了0。最初の直接Race実行は`umask 022`付け忘れで一時保存先の権限検査に失敗し、指定条件の再実行で成功した。製品の権限検査は緩めていない。
- factoryのkernel moduleへNFLOG受信とsamplingに必要な`nfnetlink_log`/`nft_numgen`を明示した。新しい設定は契約84/network journal24。旧試作向けのreaderやmigrationは追加せず、使い捨て設定・stateを再生成する。既存の83で実行中の受入と版を混ぜない。

通常Make/APKの`history-native84`イメージ生成は終了0。配布kernelで既存の実通信fixtureへNFLOG group1・抽選divisor2を接続し、許可/破棄が抽選に左右されないことと、routes/ACL/NAT/DS-Lite/links/HA/RA期限を確認した。`make qemu-network IMAGE_DIR=dist/image-native84`は44.41秒で終了0。サンプリング比率の精度や全設定の起動を受け入れた結果ではない。

## 512MiB/2CPU/TCGで残る起動失敗（KVMの結果と区別）

`dist/image-native83`は通常のMake/APK経路で生成したAlpine3.24.3/Linux6.18.59、製品`route-policy-native83`。512MiB/2CPU/TCGで全構成起動が失敗し、まだクライアント通信を受け入れていない。このCPU/メモリー指定は私有fixtureの条件で、製品の最小要件ではない。hostには`/dev/kvm`と仮想化CPU拡張がなく、勝手にhost設定を変更しない。

通常の83実行物のスタックを私有起動wrapperから採取した。実routerdはPID1のまま。worker起動約27秒時点で、controlはnetworkのHTTP応答待ち、networkは適用後の最終journal JSON符号化中だった。CPU時間は起動以来累計でcontrol7.32秒/network5.11秒、RSS約36.3/31.6MiB、major faultは両方0。単一標本であり、CPU飽和・メモリー不足・循環lock・総遅延の主因の確定とはしない。meminfo/圧迫指標は未採取。SIGSTOP/QUIT後の動作は受入には使わない。重い診断VMと一時ELFを削除し、私有の小さいsplit stackと測定値だけ残した。

Tailscaleはイメージ展開からsocket生成までの5秒期限、VRRP/BGPは適用応答3秒期限を持つ。期限超過で再起動が反復する観測と照合中。期限延長だけで合格にしない。同じ通常83・512MiBでCPUだけ2→4へ変更した一回も終了1。controlの元60秒期限超過、networkはgeneration1 appliedだが全projection applying、両端末はDISCOVER12件に対し応答0/lease無しで、DNS/TCPUDPへ未到達。MemAvailable最小153704KiB、major fault0、PSIはguestでENOENT。CPU増加では解消しておらず、空きメモリーだけで原因も断定しない。observerの付加負荷と退場worker/helperの未採取も区別する。重い停止済stageを削除し、小さい測定値だけ保持した。

## 2026-10-30 16:27 JST: 設定の重複保存削減と、残る起動失敗

`setting-journal-native84`では、カーネル設定のBefore/Index/Attemptedを変更前に保存し、成功ACKだけの全journal再保存を次checkpointまたはApplyの最終保存へまとめた。他資源とrestoreの保存は維持した。新しい状態形式・移行reader・猶予期間はない。成功直後・次保存前の停止から元値を復元する検査、network Race、vet、両architecture CGO0 build、署名六ELFと通常Make/APKイメージ生成が終了0。実Alpine kernelの既存通信fixtureは51.63秒で終了0。

ただし同じ全構成・512MiB/2CPU/TCGでは依然起動不合格。networkはrevision1 applied、全projectionはapplying、controlは第2世代・serviceは第8世代へ再起動し、両端末のDHCP request12件に応答0でDNS/両IP通信へ未到達。複数workerの起動期限とTS socket5秒の超過が残る。保存時間の改善値は測っておらず、保存回数を減らしたことから起動改善を推定しない。私有の小さい原因記録は`/tmp/routerd-setting-native84-startup-failure/`、重い停止済VMは削除した。

二台の83通信profileは初回全DNS/両IP/二FRR/特殊IX/NTP/TSを通り、片方向切替では同じTCP/UDP対応も継続したが、切替直後のTS通信失敗を検出した。別走行ではBのeffects応答が繰り返し期限切れとなり同じTCPが失敗。全HA受入ではない。旧compilerによるzone全組と旧ignoreIPv4全項目へ私有設定を揃えた後の結果である。次は受入反復を止め、最初のeffects失敗直後のcontrol/network同時stack採取を準備してから一度だけ診断する。担当者がidleのときはmessageだけでは作業が起動しないため、捕捉準備完了を確認してから開始する。

## 実上流と自然更新: 版を区別する

`d525792` / `dynamic-dns-bounded-native79`は、ens18上の別MAC・隔離router/LAN namespaceから実HGWへ接続し、7800秒/724巡の取得DNS UDP/TCP・IPv4/IPv6 HTTPSを終了0で通過した。両LAN端末の自然DHCP更新と自然PD Renew/対応Reply一組を観測。Reply送信元UDP49153、自然Renewは元T1から0.091秒後、Replyは24.623ms後。返答のT1/T2/接頭辞寿命と更新後BOOTTIME期限を照合した。

PD worker SIGKILL後1.52秒、通常交換10.99秒、全daemon再起動11.08秒で端末通信が復帰。DUID/IAID保持、WANへのLAN DNS返信漏出0、停止時RAゼロ・kernel撤去を確認した。最後は上流へReleaseしていないためRelease応答を確認済みとしない。最小の私有記録は`/tmp/routerd-pd-native80-natural/`。保存identity `~/.local/state/routerd2-homert-four`は消さない。これはnative83/84同時構成の自然更新、ONU直結、物理NIC、既存TCP保持の受入ではない。

再実行は`make check-uplink UPLINK_INTERFACE=ens18 UPLINK_AFTR=<確認したAFTR> UPLINK_SECONDS=7800 RELEASE_ID=<実行版>`を基準に対応scriptと試験計画を読む。管理経路ens18のhost address/route/sysctl/clockを変えない。

## 二台構成と残る受入

- native80の二段DNS・共有VMAC/PD/DHCP・conntrack同期では、独立ISC/Linux AFTR/端末と両方向切替・復旧・同一TCP sequence space・同じUDP対応を確認した。独立50ms UDPは985送信/810受信/175期限切れ、成功間最大間隔4.543秒。TCP最大応答待ち4.575秒。DNSは独立UDP上流へ置換した版であり、実DoHや全設定の成功へ拡張しない。
- 現在は単体83のISC/四AFTR/二FRR/IX代替/DoH/chrony/Headscale・公式TS/二DHCP端末を二台構成へ拡張中。初回はhost上でもAのeffects期限超過で起動不合格。追加観測ではA ACTIVE/PD/四AFTRまで進み、初回の原因未確定のまま端末通信を確認中。originalのWAN/LAN VMAC、共通DUID/IAID、物理LAN上の認証同期、priority150/100・preempt falseを使う。旧intentのNAT同期対象はDS-Lite四本だけでIXを含まない。IXの物理送信元を変更して既存session継続を成立させない。
- 通常のfactoryイメージで単体起動と全クライアント通信を完成させ、二台全設定の既存/新規通信・回線断・分断・再参加を受け入れる。独立Linux対向は実二回線/対象物理NICの代わりではない。本番homertの置換は未実施。
- 全homert取込（84の元設定は両機とも75件で拒否。必要な明示置換policyも未指定なので純粋な未実装数とは異なる）、残る履歴・native DPI、管理surface、Tailscale残機能、権限分離、IPsec、kernel/rootfs A/B、installer、ARM実行、対象実機、k8s/pve、Cloud SAM等が残る。全34項目の状態はJSONを参照。部分成功で完了にしない。
- k8s-rt/pve-rtの実設定は読取り済みで、SAMNodeSet/Transport/Enrollment/MobilityPoolも使われる。原本は秘密を含むため公開しない。取得・解析は変換/通信受入ではない。旧SAM目標文書を読み、認証/取消・配布・/32所有・NAT無し転送・移動/復旧へ進む。CLIの存在や旧版の受入を現行製品の成功へ置換しない。課金環境の新規作成許可を推定しない。

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
