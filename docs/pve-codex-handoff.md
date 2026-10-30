# routerd2 開発引き継ぎ

2026-10-30 JST。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)、
[試験計画](test-plan.md)、[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

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

mainの公開済み基準は`f196880`。次の変更は統合・公開済みであり、全体受入の完了ではない。

- SSHの`update rollback`を既存の検証済み旧版予約処理へ接続。`4f22e00`で実SSH操作・予約・取消・設定revision/generation照合が38.86秒で成功。再起動後の旧版復帰はこの操作試験の対象外。
- `18f36d4`で旧BGPの`communities.send: standard`を受理し、旧実装と同じ送信設定へ変換。独立FRRの通常BGP、IPIP、DHCPv4由来IPIP、二peerの実通信が全て成功。関連Race、vet、両architecture buildと署名六ELF検査も終了0。
- `671038f`のDNS query履歴保持は旧`logstore.ApplyRetention`を既存resolverへ接続する。before-delete UDP archiveの失敗でDNS応答を止めない。`09a3f6a`のFirewallEventLogは旧NFLOG readerを再利用し、既存ログ所有者へ構造化packet記録を渡す。独立端末による各元版の障害・復旧・再起動・撤去は通過。統合版`history-native84`でも実peerは成功。関連Race、vet、CGO0両architecture buildと署名六ELF検査が終了0。最初の直接Race実行は`umask 022`付け忘れで一時保存先の権限検査に失敗し、指定条件の再実行で成功した。製品の権限検査は緩めていない。
- factoryのkernel moduleへNFLOG受信とsamplingに必要な`nfnetlink_log`/`nft_numgen`を明示した。新しい設定は契約84/network journal24。旧試作向けのreaderやmigrationは追加せず、使い捨て設定・stateを再生成する。既存の83で実行中の受入と版を混ぜない。

通常Make/APKの`history-native84`イメージ生成は終了0。配布kernelで既存の実通信fixtureへNFLOG group1・抽選divisor2を接続し、許可/破棄が抽選に左右されないことと、routes/ACL/NAT/DS-Lite/links/HA/RA期限を確認した。`make qemu-network IMAGE_DIR=dist/image-native84`は44.41秒で終了0。サンプリング比率の精度や全設定の起動を受け入れた結果ではない。

## 通常配布イメージの起動: 未解決

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
