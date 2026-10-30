# routerd2 開発引き継ぎ

2026-10-30 JST。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)、
[試験計画](test-plan.md)、[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

## 2026-10-30 18:56 JST: DNS・観測・通信履歴の統合と計数の所有順序

`9c6c53d` は `22d1d7f` を統合した専用作業ツリーの実装です。契約は native88 / network26 です。
この段階では対象の homert 実機受入と設定全体の移行は未完了です。期限の2026-10-30 01:56:38 JSTは超過しています。

- DNSは旧 `DNSUpstream.when` の選出条件を設定変換時に結び付け、学習元と選出元の元期限を保持します。待機系では該当上流だけを撤去します。RAの受信処理は旧 `raobserver` と同じ自己広告の除外を復元しました。
- Healthの実測時刻・結果を既存の非同期記録へ渡し、公式SDKでOTLPの計測値とトレースへ送ります。配送先の停止が現在の監視を止めない構成です。他の旧計測機能を移行済みとはしません。
- 通信履歴は旧 `logstore.FlowKey`・保存形式と `dpi.Classify` の実処理を再利用します。既存接続の計数、TLS接続先名、保存障害からの回復、再起動後の保持、期限削除と収集撤去を接続しました。新規の外部デーモンは追加していません。nDPI全分類とDHCP端末識別は別の専用ツリーで実装中です。
- 統合試験で、Linuxの `nft_ct_get_init` が `NFT_CT_PKTS` 規則の作成時に計数を有効化し、元値の保存を飛ばす欠陥を発見しました。`network.apply.reconcile` が規則作成前に計数を所有し、規則撤去後に元値を戻す順序へ修正しました。暗黙の必要設定も同じ所有者が扱います。明示的な無効化との矛盾は設定検査で拒否します。

版別の実通信証拠は次のとおりです。異なる実行物の結果を一つの全体合格にはしません。

- `a2c2248` / `homert-native88-a2c2248`: DNS主従・独立keepalived・元期限切れと復旧が46.45秒で合格。`dist/native88-dns-peer/additional-import.log`。同版のOTLPは独立TCPの成功・断・復帰、実測時刻と計測値、配送先断中の監視継続を確認しました。UDPは949受信・失敗0。`dist/native88-telemetry-peer/peer.log` と受信JSONL、`binaries.json`。
- `a2c2248` に `9c6c53d` の計数順序修正を加えた `homert-native88-accounting-order`: 通信履歴試験が24.04秒で合格。計数0から製品設定で有効化し、実TLS/SNI・TCP/UDP・保存先障害とロック・製品再起動・元の記録保持を確認しました。明示計数設定だけを撤去しても観測は継続し、観測を撤去すると元0へ復元、新規TLSも成功しました。`dist/native88-flow-order-peer/TestDaemonTrafficFlowHistory.log` と同ディレクトリの実行物ハッシュ。
- `9c6c53d`: networkの競合検査、全vet、amd64/arm64の静的ビルド、両形式の署名付き配布物検査、秘密情報検査が成功しました。`/tmp/routerd-native88-final-checks.log`。SQL変数上限を超える履歴の整合性検査は通常約1.8秒、競合検査付き約56.6秒でした。試験だけの30秒制限を外し、製品の期限は変えていません。

次は実設定全体の未対応部分、二台での現行版の受入、自然更新・管理・更新復旧です。k8s-rt/pve-rt、Cloud SAMの順序は維持します。nDPIの静的配布依存と対象環境の観測条件が未確定のため、完成日時は見積もれません。

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

## 2026-10-30: 実設定の所有情報と実行物の前提を取り込む

`owner-package-native88`はdb12658上の取り込み修正。旧routerdのlifecycle/statusとchain/phase2を読み、`ownerRefs`を一律の起動・削除条件として拒否していた処理を除去した。DHCPv6Informationは旧実装と同じPD owner参照またはliteral interfaceで、既存の一つのUDP546 clientを選ぶ。旧Interface decoderが読まない`role`はFirewallZoneの代わりに採用せず、取り込み注記へ残す。SAM所有BGPの固有policyは未実装として拒否する。

実際のhomertにあるPackage/conntrackdは、設定済みNAT同期の署名engineが実行物を供給する前提へ対応させた。hostのaptや任意package管理は実装しない。出力は同じ既存のNAT同期設定で、新しいruntime形式や互換readerはない。

`owner-import-native88`の実supervisor/control/network/workerと独立ISC・keepalived・Linux clientで、所有情報を持つ取込fixtureが71.61秒で終了0。SLAAC/default/DNS取得、DNS UDP/TCP、IPv6 TCP/UDP、役割変更・本来の権限期限による撤去、PD喪失/再取得を確認した。最初はfixture指定のISC実行物がhostの既定pathに無く、製品起動前に失敗。既存の独立ISCを明示した再実行で成功した。最終Package追加後のlegacyconfig Race、全vet、CGO0両architecture build、署名配布物検査は終了0。Package追加はcompilerの前提解決だけで、実通信はその直前の版である。全homert設定の取込・実通信、SAM BGP、管理画面、対象物理機器の受入は未完了。
### 2026-10-30 19:12 JST: DHCP端末識別の統合と未使用プラグイン履歴

- `232f23f`（native89/network26、製品識別子 `homert-native89-dhcp-fingerprints`）で、本体と通知エンジンを再作成した。
  `dist/native89-dhcp-fingerprint-peer/` に実行物のSHA-256と実通信ログを保持している。
  独立したBusyBox端末のアドレス・経路取得と疎通、実通知の端末識別情報保存、保存先の書込みロック中の別端末の取得・疎通、解除後の保存復帰、保持期限での削除、設定撤去後の配布継続を確認した（内側28.85秒、全体29.90秒）。
- 履歴の生成元は実際のdnsmasq通知で、旧routerdの端末推定・保存処理を再利用した。
  DHCP通知の受付から履歴保存を分離し、エンジンと通知受付が終了してから履歴を停止する。
  関連する取込・DHCP・設定・形式契約の競合検査も通過した。対象実機、二台切替、削除前の外部配送はこの試験の対象ではない。
- `pluginRuns` は、保存済みの両homert設定に `Plugin` と `EventSubscription` がないことを確認した。
  旧実装の実生成元は手動プラグイン実行と購読からの実行だけであり、この入力には生成元がない。
  その場合だけ非稼働の保持分類として明示し、不要な保存処理は生成しない。宣言がある場合は未対応診断を維持する。
  過去のデータベースが空とは仮定せず、既存内容を移行しないことも通知する。この取込変更は上記の実通信試験後に追加し、取込試験を通過した。
- nDPI分類と通常配布は独立担当で継続中。上記を全設定の起動、自然更新、同一版の二台運用や本番置換の合格として扱わない。

## 2026-10-30 19:26 JST: homert全入力を妨げた参照解決を修正

`homert-import-native89`（7e4bf94上）では、観測用InterfaceとDSLiteTunnelが同じkernel名を指す宣言を一つのaliasとして解決する。異なるkernel名への同名aliasは拒否する。委譲先DNS listener/AAAAのPD参照も、既存の参照解決でKind/nameへ揃える。元設定の短い`wan-pd`名を直接内部indexで探して失敗していた。unconditionalなIPv4StaticAddressのAppliedを要するIPv4Routeは、既存network writerのaddress→routeの適用順序へ対応する。新しい待機workerや重複状態は作らない。

実際の両homert入力へ、VRRPのlocal/VMAC、停止したTailscaleの保存identity利用、有限lease由来DNS、stateless DHCPv6と相互TLS同期の置換policyを私有ファイルで明示した。両方の実CLIは、WebConsoleとincludeApplicationLayerの未対応2件だけで拒否する。出力は原子的に空で、設定を削って変換成功と扱っていない。policyの証明書pathと保存identityはまだ配備しておらず、本番適用やtailnet再登録もしていない。これは変換診断の結果で、全設定の起動・通信の合格ではない。

同じ版の実supervisor/control/network/worker・独立ISC/keepalived/Linux clientによる既存の取込LAN試験は71.71秒で終了0。短いPD名、ownerRefs、roleを持つ入力からSLAAC/default/DNS、DNS UDP/TCP・IPv6 TCP/UDP、役割変更・元権限期限の撤去、PD喪失/再取得、既存DNS TCPを確認した。静的route前提と同名tunnel aliasは関連compiler検査で確認した範囲であり、両homertの全経路の通信結果へ拡張しない。関連Race、全vet、CGO0両architecture build、署名配布物検査が終了0。

## 2026-10-30 19:41 JST: 最初のnative WebConsole実経路

`web-console-native91`は1def991上のnative91/network26。既存Go module内の設定されたworkerがHTTP listenerを所有する。旧routerdのcmd_serve/webconsoleで実際の待受・停止、読み取り専用method、base path、title/linksを照合し、現行の型付きcontrol APIへ接続した。旧resource storeを再現するadapter、別HTTPサーバー実行物、JavaScript buildは作らない。現在は稼働状態、実kernelのinterface/route、設定資源の条件、設定確定/rollback期限の4ページ。旧履歴・client/connection・VPN/SAM等の全画面は未実装で、WebConsole全体の同等性を受け入れた結果ではない。

通常ビルドの実supervisor/control/network/worker、別network namespaceの独立HTTP/SSH client、実mgmt FirewallZoneで最終版が終了0。HTTPの4ページ、title escape、POST405、private API404、SSH編集・確定・5秒rollback、logging/control/console workerのSIGKILLと復帰、DNS継続を確認した。最初の試験はsupervisor.Ready直後のcontrol未準備を同一視してHTTP503で失敗し、fixtureを実control.Healthy待ちへ修正した。製品は不健康な観測を503と異常表示のまま維持した。初回の署名publisherは新作業treeの親directoryが775だったため拒否。作業tree rootを755に直し、製品側の権限条件を緩めず最終両architecture検査で成功した。関連Race、全vet、CGO0 amd64/arm64 build、署名六ELF/publisher/stagingが終了0。

両homertの全入力と明示policyによる実CLIは、includeApplicationLayerの1件だけを拒否し出力は空。管理画面の全機能を満たしたという意味ではない。別担当のnative90/network27のnDPI・接続時刻所有と、native91/network27として統合する必要がある。実設定全体の単体試験開始には、その統合実行物、通常engine archive、既存の独立ISC/4 AFTR/2 FRR/IX/DNS-DoH/NTP/Headscale、私有のpeer証明書と独立tailnet用identityを揃え、元入力の各外部endpoint/物理interface/addressを隔離環境の対応先へ明示的に割り当てる。生成されたservice名・role参照をfixtureの固定名と混同しない。元の稼働identity・本番endpointへは接続しない。既存51resource profileを全109resource入力の受入へ置き換えない。

準備としてapplication分類の宣言1件だけをfalseにした私有コピーは、両機とも実CLIで109→55resourceに変換できた。4 AFTR、2 DNS、6 Health、WebConsole、DHCPv4/v6、PD、RA、BGP、NTP、Tailscaleと各履歴/配送workerを含む。元入力の合格ではなく、未統合機能を除いた構成を調べるためのコピーである。元入力は変更していない。

全設定単体の前提に追加の差がある。元homertの物理WAN/LANと管理interfaceは外部OS管理で、管理用IP自体の宣言はRouter入力にない。最小Alpineでは外部設定の残存を前提にできないため、3本目の管理リンク、物理linkのUP所有、読取りで確かめた管理address/prefixをnative構成の明示的な追加intentとして用意する。元のunmanagedを黙ってUPへ変えたり、試験手順の手動修復を隠して合格にしない。既存二link fixtureをそのまま全入力へ使うことはできない。

## 2026-10-30 20:04 JST: 通信履歴・nDPI・管理画面の統合候補

`96d7d3f` の通常ビルド `supervisor-native91`（native91/network27）で、独立端末による管理画面・設定復旧が8.84秒、内蔵分類が21.86秒、nDPI分類が21.90秒で成功した。同じUDP端点の自然失効後の再利用、同じTCP端点のTLSからHTTPへの再利用、DNSへの誤ったTLS情報の混入防止、保存障害中の転送、復旧・再起動・保持と撤去後の元のカーネル設定への復元を確認した。STUNの分類は内蔵処理の結果で、nDPIによる分類とはしない。接続開始時刻はネットワーク適用担当が収集前に有効化し、観測の撤去後に元へ戻す。既存の設定所有・保存処理を使い、新たな管理処理や外部デーモンは追加していない。

同じ製品コードのAMD64・ARM64向け通常ビルド、静的ELF検査、署名付き一覧・展開・公開処理の隔離試験、関連する競合検出検査と全体の `go vet` も成功した。再現手順は `make build RELEASE_ID=<識別子>`、`make check-management`、`make check-traffic-flow`、`make build-linux` と `make release-built-test`。同じ `RELEASE_ID` を指定し、Docker実行権限と通常の同梱エンジンを用意する。既存の `scripts/daemon-tests.py` で生成済みの `dist/bin` を指定すれば、同一実行物を再ビルドせず検査できる。

この候補の `routerd-modules` はnDPI 4.2と依存ライブラリーを静的リンクするためCGOを有効にしている。現行のCGO無効という配布条件を満たしたとは扱わない。方針の変更案は取り下げ、主担当が利用者へ送った確認の回答を待つ。機能の成功、配布方針への適合、本番置換の受入は別である。全homert入力の同時起動・自然更新・二台運用、対象機器、ARM64での実行はこの結果に含まれない。


## 2026-10-30 20:28 JST: 許可された配布条件で接続履歴を統合

利用者が主担当の確認に回答し、`routerd-modules` に限るCGO有効・静的リンクを承認した。ほかの二つのGo実行物はCGO無効を維持する。20:04の確認待ちは解消し、規則と通常配布経路を同じ条件に揃えた。公開済みの設定形式91と区別するため、統合候補 `eb66441` は設定形式92・ネットワーク保存形式27とした。

同候補の通常配布物 `supervisor-native92` で、内蔵分類22.86秒・nDPI分類22.89秒の実端末通信が成功した。履歴保存障害中の転送、DNS情報、同じUDP端点の再利用、TLSからHTTPへの同じTCP端点の再利用、再起動・保持・撤去を既存の実通信試験で確認した。AMD64・ARM64の静的ビルド、署名付き配布物の展開・公開処理、形式契約検査も終了0。関連する競合検出と全体の静的検査は20:04の同じ通信実装で成功済みで、その後の製品変更は形式識別子だけである。証跡は `dist/supervisor-native92-flow/`、配布検査は `/tmp/routerd-supervisor-native92-final.log`。再現は前節の通常経路で同一の `RELEASE_ID` を指定する。

`440a674` の既存カーネル試験は24.90秒で、両IPの実TCP開始時刻、DS-Lite転送、撤去後の元値復元を確認した。`dist/supervisor-native91-kernel/kernel.log` に保存した。この試験は形式宣言へ依存せず、`eb66441` とのネットワーク実装差分もない。タグを指定しなかった別試行の「no tests to run」は検証結果に含めない。

更新・明示旧版復帰の受入変更 `bf06f26` も取り込んだ。実起動の成功は `1def991` の署名付き三版による結果で、今回の設定形式92の全構成を更新・復旧した証拠ではない。時間・条件・未受入範囲と再現方法は `docs/test-plan.md` と `docs/releases.md` に記載した。家庭用ルーターの全設定、自然更新、二台運用、対象実機とARM64実行の受入は別に追う。

## 2026-10-30 20:32 JST: 全入力から起動した単体の端末通信と再起動

`96d7d3f`の通常実行物`supervisor-native91`（native91/network27）を隔離Linuxの実supervisor/control/network/全workerとして起動し、全設定の単体利用経路が終了0。元homert02/03の各109resourceを削らず取り込み、application分類も有効のままとした。外部OSが所有していた物理3interfaceのUPと管理IPv4を、最小appliance向けの明示intentとして追加したため、入力110→native57resource。今回実行したのは02側で、03側の変換成功を起動成功とはしない。物理名だけを隔離3linkへ対応させ、IPv4の元prefix/pool/除外規則は維持した。独立Headscaleの新規identity、試験用HA証明書、DNS/DoHの隔離endpointを明示し、本番peer・保存identityへ接続しなかった。

独立ISC DHCPv6、radvd、Linux AFTR四本、FRR二台、IX相当Linux、dnsmasq/dnsdist、chrony、Headscale/公式Tailscaleと、起動前から接続した普通/特殊dhcpcd端末を使用した。端末の実取得値で次を確認した。

- DHCPv4の動的割当とgateway/DNS/NTP配布、SLAACとstateless DHCPv6、取得resolverによるUDP/TCPのA/AAAA、IPv4/IPv6 TCP/UDP。
- 条件付きDNS24問い合わせ、両FRRの四prefixへの実TCP/UDP、特殊MAC scopeの自動DNS・IX経由HTTP、配布された両NTPサーバーへの独立時刻問い合わせ。
- 設定されたTailscale三prefixの広告と、実際にDHCP取得したLAN端末・独立Tailscale端末間の双方向TCP/UDP。三prefixすべてに端末を配置した結果ではない。
- 製品全体の停止・起動後の同じ通信、DUID/NodeID保持。新規DNS/両IP TCP/UDPが全部再成立するまで12.690秒。DNS58件中20件失敗、両IP TCP/UDPの失敗後の再成立は11.376〜12.277秒。再起動中の損失を除外せず記録した。既存の同一TCP socket継続を主張しない。

DoH対向のTLSとDNS応答は製品起動前に独立curlで確認した。製品のdefault sourceは二つのHTTPS URLのみなので、端末のdefault名前解決成功は少なくとも一つの実DoH応答を通っている。各URLを個別に使った成功は未分離で、ClientHelloの二hostname観測だけから両方の応答成功とはしない。host platformのchronydは`-x`であり、ホスト時計を変更しない。時刻採用の実guest試験とは区別する。

最初の失敗は対向dnsmasqの旧bind address、次はdnsdistの旧subnet ACL、次はlistener起動前の対向問い合わせ。修正後、AFTRの確認と再読取りの間の観測変化でもfixtureが停止した。一つの有効endpoint観測を用いるよう直して全経路を再実行した。これらを製品の成功や製品修正として数えない。通常DADと製品の不健康表示・期限は緩めていない。

実行物SHA256は`routerd=deb5324e3cdd4e036ccea9487199040a671907414a2aa6db500fcda6d0fb3fb4`、`routerd-modules=b05c3b09233c3bf6d7ec65e7885238de70790888443c98a327a2235048361cac`、`routerd-logging=e09a546f31e7e6296ef90777126e66976d2986f6a7f8c603201b0c0fa16a1604`。実行手順は、元入力と明示policyを`routerd config migrate --policy <policy> <source>`で変換し、上記独立peerと3linkを隔離名前空間へ配置、試験CAを専用trust bundleへ追加、通常3binaryと同梱engineから`platform=host`で起動する。端末の取得hookからresolverを実際に設定し、dig・echo TCP/UDP・chrony・公式Tailscaleで確認した後、同じstate directoryで製品を停止・起動する。私有の準備・実行scriptは`/tmp/routerd-full-homert-{prepare,host-prepare,natural-prepare}.py`。入力・鍵は公開せず、再使用が終わるまで必要な小さいfixtureを保持する。

同じ91実行物で元のDHCPv4リース43200秒/T1=21600秒を変えず22000秒の自然更新観測を開始したが、観測約154秒の取得resolverへのUDP DNS timeoutで終了1。自然更新前に停止しており、短時間の通信成立を安定稼働とは扱わない。captureは一interfaceあたり16MB×2本。失敗時はRA workerが同じIPv6 destination/table/metricの重複をHTTP422で拒否されて再起動を繰り返し、後にBGP/VRRPのeffects期限超過とVIP/PD側の不準備を観測した。因果は切り分け中で、二つの独立RA interfaceの扱いを旧routerdと実kernelに照合してから修正・再開する。両方の全入力による二台切替も未受入。

mainは`8a85fdb`へ進めてpush済み（native92/network27、許可済modulesのみCGO1静的リンク、nDPI/接続開始時刻、更新受入手順）。単体全入力と失敗した自然更新観測の実行版は91で、92・通常appliance image・03側単体・二台停止/切替・物理ONU/NIC・全34項目へ合格を移さない。
