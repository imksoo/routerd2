# routerd2 開発引き継ぎ

更新: 2026-10-31 11:52 JST。全34 work packageのゴールは継続中。
[実装計画](implementation-plan.md)、[機械可読台帳](implementation-plan.json)、[試験計画](test-plan.md)、[AGENTS.md](../AGENTS.md)を読む。
各試験前に試験計画の実行手順と対象項目を再確認する。対象本番の置換は実施していない。

## 現在の実装と通常配布物

主な実装は`61840d9`（DNS別名`e5b14d4`、送信元`27fbad2`、DHCP`b9fe727`）。NTP二台受入の通常配布物は`ntp-calibration-native99` / native92 / network27で、DNS別名修正を含む最終統合版の通信確認は別に行う。
全設定二台の最新実行版はnative99、継続中の元LAN自然更新はnative93、実HGW自然更新はnative95であり、実行版を区別する。
`make build image`のAlpine3.24.3/Linux6.18.59-ltsを実行した。routerd/loggingはCGO0、承認済みrouterd-modulesだけCGO1静的リンク。
旧routerdのdnsmasq配布と非同期同期を使用し、VRRPが配布役を選ぶ。DHCP独自の`.owner`・`bootstrap`・新規配布保留を除いた。
同期断も指定poolを維持する。復帰時はengineを停止して受信済みlease・削除・保持履歴を併合し、再開する。
相互認証、domain照合と同時配布中peerの拒否を保持する。poolの暗黙分割や別の所有者台帳はない。

BGPは確定したアドレス族を一度だけ構築し、GR capabilityを保持する。chronydは時計更新ごとに補正周波数・誤差を保存する。
networkは全interfaceの期限更新intentを一括保存し、所有が変わらないFirewall boolの再保存を省く。
AFTRのnetwork問い合わせは共有lockの外で行い、待機後に現在の設定と送信元を再確認する。
DHCPの状態通知失敗はengineを停止させない。元の適用・観測・通信の期限は延長していない。

## 現候補で確認できた通信

- `health-ready-native105`: `require_active_for_ready` を指定した health probe は自身を停止させず、現在の active 観測を設定 readiness に加える。`make check-health-pbr` は、独立二 WAN namespace peer と実 supervisor/control/network/Health worker で PBR の障害切替・復旧、worker と supervisor の再起動、古い通知と期限切れの拒否、confirmed rollback、TCP/UDP と packet capture を通過した。これは Linux kernel の隔離通信であり、対象機器・回線の受入には数えない。

- `dhcp-ingress-native105`: 隔離した PVE の 4 CPU/4 GiB VM へ、ハッシュを照合した host overlay の `routerd`、`routerd-modules`、`routerd-logging`（release ID `dhcp-ingress-native105`）を投入し、20 module の Ready を確認した。cold boot と reboot の各経路で DNS UDP/TCP（IPv4/IPv6）、端末 TCP/UDP、BGP、NTP、Tailscale が通過した。独立した通常・特殊 DHCPv4 client は、120 秒 lease の自然 T1 でそれぞれ `172.18.0.1:67` 宛て unicast REQUEST に同一 XID の ACK を受信し、server identifier（option 54）は `172.18.0.1` だった。判定後に生 pcap を削除した。これは PVE の kernel と実 daemon の相互通信結果であり、元設定の12時間 lease による自然更新、ONU/HGW・対象 NIC/機器、全34 work package の受入は未実施である。

- 全57resource、独立ISC・4 AFTR・2 FRR・IX代替・DNS/DoH・chrony・公式Tailscaleと独立LAN端末。通常Alpine二台、各4CPU/4GiB KVM。
  両方向の実電源断・再参加が終了0。旧主系不在中に新端末を接続し、DHCP/SLAAC・取得DNS UDP/TCP・両IP TCP/UDPが成功。
  再参加後の明示DHCP更新でも同じ割当と通信を保持した。二度目の断はbackupのBGP確立を待っていない。
  全経路37.401/25.390秒、同じTCP/SYN/NATを保持。最大TCP待ち14.945秒（判定15秒）、UDP1103送信/935受信/168期限切れ、最大無応答13.216秒、DNS659件中45失敗。
  無損失・性能保証・長期安定とはしない。先行する同候補の二台走行も18.912/21.606秒で通過した。
- 二台設定は実入力109resourceに明示appliance設定を加えた110→57。共有WAN IPv4をBGP next-hopに明示し、そのアドレスを既存WAN VMACへ置いた。
  通常CLI変換と実行設定が一致する。元設定を変更しない合格ではない。
- `make check-ha-state`終了0。独立BusyBox、実kernel、両方向切替、同じTCP/UDP、worker/engine停止、同期断中の更新・新規配布・再起動、復帰、元期限失効、sticky保持と再取得。
  TCP最大3.297秒、UDP2920送信/2829受信/91期限切れ、最大無応答1.875秒。
- `make check-ha-dhcp-initial`終了0。独立dnsmasqを停止後、その最終leaseを両初期状態へ取り込み、旧主系不在中に新端末の自動取得・取得DNS・WAN IPv4 TCP/UDPが成功。
  固定した同版三実行物の独立受入でも、再参加後の新旧両端末更新・両側lease保持・通信が成功した。
- DNSの通常1秒/監視3秒は独立して動く。実RA/DHCPv6で学んだWANリンクローカルDNSを別LANの独立UDP/TCP端末から使用し、期限失効とRenew復帰も確認した。
- 実設定適用→ログ→OTLPは独立Collectorの停止・復帰・重複なしと並行UDP1246件/失敗0を確認した。

`make check`はRace Detector・vetを含め終了0。保持時間を調べるBFD補助試験では実行遅延で元packetが失効したため、
そのケースだけ有効な遅いremote送信間隔を使用した。期限失効の別ケースと製品の期限は維持する。
通常ビルド/image、amd64/arm64静的六ELF、署名catalog/publisher、Python検査、秘密情報検査も成功した。

## 移行と復旧

旧leaseとsticky履歴の明示取込、元期限・別MACへの重複防止・元MAC再取得を通常製品で確認した。
`59c996e`は独立OpenSSHの旧host keyを初回から固定し、全製品再起動後も同じ鍵で接続した。
`17222da`は外部公式Tailscaleを停止して保存したidentityだけを使用し、初回起動前から認証鍵なしで同じnode ID/IP、3経路の双方向TCP/UDP、両IP出口、再起動後の管理SSHが92.045秒で成功した。
`6716b17`は未実行候補の1byte破損を拒否し、確認済み版・設定本文/revision・DUID・固定SSH identity・端末の自動DNS/両IP通信へ復帰した。
この破損試験はknown-good `full-homert-loopback-native92`と候補native93の同ABI・限定構成であり、全57設定の更新や既実行trial破損の受入ではない。
`d2bb7ce`はruntime削除中断時にmanifestを最後まで残し、SSHでの削除再開と次版保存が成功した。`460a405`は既存の型付きBGP観測を運用画面へ接続し、`ad35015`は直接退避経路で1MiB双方向・両SYN MSS1460を確認した。
`e9f0f96`はstage書込中断で不完全なmanifestだけが残った場合の削除を可能にし、未知のpayloadは削除前に拒否する。
`ee99734`は各DNS端末のUDP受信サイズに応じてTCを返し、TCPと完全なcacheを保持する。
`12962f2`は既存の型付きページAPIからkernel経路・規則・multipathを運用画面へ表示する。
統合版native96の実DNS二試験・管理/SSH/再起動、関連4packageのRace、vet、amd64/arm64静的六ELFと署名publisherは終了0。
`421d126`は正回答のminTTL60秒でnegativeTTL30秒を延長する不具合を修正した。独立dnsmasqへ名前を登録した比較版は31秒後もNXDOMAIN、修正版では両段が31秒後にAを取得した。統合版native97でも同じ実daemon通信検査・DNS Race・vetが終了0。
対象homertの実鍵・保存状態・配備条件の受入は残る。実稼働二台は4CPU/4GiB、管理NIC名は二台で異なる。PVEの読み取りで現在interfaceを確認したが、既存鍵でのroot SSHは拒否され、実秘密状態は未取得。本番の設定・稼働は変更していない。

## 経路欠落の原因修正と未解決

- 未同期の休止端末の割当を持つ主系を失うと、古い生存機がそのIPを別端末へ再配布する失敗を確認した。
  旧routerdの15秒非同期転送にも未受信の事実を復元する保証はない。既知leaseの保護と区別し、完全保護とは扱わない。
- 旧`8cc420d`の元LAN lease43200秒の観測は約5時間17分/593巡でTCP応答待ちが失敗し、自然T1前に終了した。
  packet上では相手が要求をACKしてから約30.43秒後に応答した。相手の直列accept/recvは除いたが、元試行の遅延原因とIX経路へ変わった理由は未確定。
- 実HGWの`bgp-gr-families-native92`は約100分でIPv6 host route/source欠落を検出して終了した。RAのsource/routerは有効、network Applyは期限超過していた。
  `27fbad2`は適用ACK喪失で`r.applied.network`を消すことと、送信元候補の選択を分離した。現在意図のIPv6候補を既存network ownerに渡して実在・所有・DADを確認し、元期限を保持する。再試行・期限延長・台帳は追加していない。
- native93のnetwork fsyncを22秒遅らせると同じ欠落を実daemon/kernelで再現し、473回中11回欠落、kernelのtable120削除を記録した。
  この注入は再開した7800秒コホートを誤って転用しており、自然再発・自然更新の結果には数えない。以後は別stageの180秒専用試験に限定した。
  native94の専用180秒試験では同じ注入後も38秒706回の欠落0。取得DNS・両IP通信、PD SIGKILL2.07秒/通常交換4.63秒、全daemon交換4.33秒と両端末復帰まで終了0。自然PD Renewは0件であり短時間の結果だけとする。
  元100分走行で適用応答が遅れた発生契機は未採取。短い再現で経路を消す製品側の原因は修正したが、長時間安定の受入は残る。
- native99は同じ全57設定・通常Alpine二台で両方向電源断と再参加まで終了0。全経路30.107/18.288秒、NTP採用4.314/4.269秒。BGP確立待ちなしの追加断、別の新端末の取得DNS・両IP TCP/UDP、同じTCP/SYN/NATを保持した。
  TCP最大7.713秒、UDP1690/1495/195期限切れ・最大無応答5.047秒、DNS16/933失敗。停止ディスクのコピーでjournalを再生し、保存済み誤差291.39/306.48ppmを確認した。元60秒・15秒・採用条件は変更していない。
  `driftfile interval 0`はchrony4.8の時計更新時だけ保存する。60秒制限で校正直後の値を保存し損ねる窓を除いた。別writer・周期処理・source poll変更はない。
  関連Race・vet・通常Make build/imageと、amd64/arm64静的六ELF・署名catalog/publisher（22.856秒）は終了0。配布物検査の統合ソースはDNS別名修正を含み、二台image99はその前の版である。
  比較native97は断前55.94ppm、再起動時1000000ppm、NTP採用130.537秒で元60秒を失敗した。自然更新・物理性能の受入に拡張しない。
- 通常native95の全57設定二台試験では、両方向の端末DHCP/SLAAC・DNS・TCP/UDPは復帰したが、二度目のNTP採用が約58秒を要し、合計60秒を超えて不合格。
  再起動時にchronydは`Frequency 0.000 +/- 1000000.000 ppm`を読んだ。packetのroot dispersionは約53秒から参照元の更新後に約0.0036秒へ低下し、その後端末が採用した。
  未校正値を保存・読込した事実であり、同期済みの精度を破壊したとは未確認。同期flagだけで品質を判断せず、次は電源断前後のtrackingも採取する。元60秒と端末の採用条件は維持する。
- 注入なしの元7800秒をnative95で新規開始した。現在logは管理端末の`/tmp/routerd-integrated-host-source-hgw95.log`、保存DUIDは`.local/state/routerd2-homert-four`。
  実PDの自然T1は2026-10-30 21:45:46 UTC（10月31日06:45:46 JST）。標準観測と終了時復旧は約06:57 JSTまでの見込み。長時間コホートを停止・交換・故障注入しない。
  起動時にAFTRの旧観測HTTP409で1回再起動した。運用観測開始時はgeneration2、以後の変化を監視し、再起動0とは記録しない。
- 元LAN43200秒はnative93・元単体設定・独立端末で再観測中。私有手順は管理端末の`/tmp/routerd-dhcp-reconcile-kvm-natural93.py`と対応log。
  相手ごとのPIDファイル、並行TCP処理と処理時刻を明示した。期限を短くしていない。普通/特殊端末の実ACKからT1は2026-10-31 01:20:47 UTC（10:20:47 JST）。その後まで観測し、以前の04:49終了予定を流用しない。

## 次の作業と保護

修正を統合した通常Alpine二台と自然更新を受け入れる。原始の100分走行のI/O遅延契機は未特定として扱う。
その後、同じ契約の移行・更新復旧と対象homertの配備受入、k8s-rt/pve-rt、Cloud SAMの順に残作業を進める。
全34項目、対象ハードウェア/NIC/ONU、ARM実行と性能、Cloud SAMは未完了。

稼働中の専用VM・stage・保存DUIDを停止・削除・差替えしない。VM操作前に所有名を照合し、既存bridge/NICには接続しない。
秘密を含む実入力・鍵・identity・pcap・インフラ詳細は公開しない。完了したstage・重複実行物・生captureは、必要な診断と簡潔な記録を終えたら削除する。
