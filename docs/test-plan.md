# routerd2 実通信・製品受入試験計画

更新: 2026-10-28。対象は [実装計画](implementation-plan.md) と同 JSON の全34 work package。
目的は、端末を接続しただけで必要な通信ができ、障害から復旧し、運用・更新できる最初の製品を完成させること。
旧 routerd と homert02/03 の実設定・実装を先に読み、必要な機能と実際の制約を特定する。
旧 routerd2 の試作 API・形式・過去のテストに互換性を設けない。

## 進め方

1. homert02/03 の実設定から、それぞれの WAN、LAN、DNS、DHCP、RA、NAT、経路と管理の構成を確定する。
   まず一台で、初期設定後の独立端末の接続からインターネットまでを通す。DHCP と DNS を別々に動かしただけでは合格にしない。
2. 同じ単体構成で、通常の更新、停止、再起動、障害と復旧を実施する。短時間の疎通と、自然更新を含む長時間の継続を別々に記録する。
3. その単体構成を二台にした上で、実際の利用者の既存接続と新規接続を分け、切替・分断・復旧を検証する。
4. homert と独立して進められる routing、管理、更新、他 engine の実装と試験を並行して進める。単一の不具合に全体を止めない。
5. 対象機器と cloud/Kubernetes の実環境をそれぞれ受け入れる。VM の合格を実機や別 provider の合格に置き換えない。

各試験は実装済み・実行済み・不合格・未実施を区別する。失敗時は、どの版・peer・設定で何が起きたかを特定し、
製品、試験、環境のどこに原因があるか判明するまで不明として扱う。原因を修正して影響する試験を再実行する。
時間を測らず同じ切替を反復しない。必要な独立作業は続ける。生 packet capture や一時実行環境は診断後に削除し、
版・条件・結果・残る限界だけを簡潔に残す。テスト件数、coverage、過去の成功は製品完成の根拠ではない。

## ゴールの実行手順: 各試験前に必ず確認

この節と対象機能の試験項目を、**各試験を開始する前に必ず読み直す**。長時間試験の開始時だけでなく、
失敗後に条件を変えて再実行する場合も同じとする。ユーザーの明示指示と `AGENTS.md` を優先する。
既存のゴールは全34項目を含む最初の製品完成であり、単体・HAの一部成功で完了にしない。
ゴール開始は2026-10-24 11:28:25 UTC。2026-10-28 15:15 UTC時点で約4日3時間47分が経過した。
進捗判断では経過日数と、未完了の機能・実環境を意識する。

1. **対象と結果を決める。** 実際のhomert設定・旧 routerd・現行の本番経路を読み、端末、peer、必要な動作、
   成功を判定する端末側の結果とpacket/kernelの観測を明示する。短時間・自然更新・実機を区別する。
   実機・回線がない場合は未検証とし、別の独立作業を進める。
2. **試験の前提を現実と照合する。** 初期設定・接続条件・管理経路・復旧・所有権を確認する。
   DHCP poolの動的割当を固定値と決めつけない。固定予約を試す場合は予約を明示する。
   IPv6等は表記文字列でなく値として比較し、端末の実取得値を使う。試験の便宜の手動設定を隠さない。
3. **実物で一つの利用経路を通す。** 製品プロセス、kernel、独立client/peerで要求から実通信まで確認する。
   試験開始時に、実行版・環境・対象・判定と必要な観測を把握する。作り込んだ模擬実装を先行条件にしない。
4. **失敗のたびに観測し、次の行動を決める。** 失敗した時刻の端末状態、protocol packet、worker、kernelの
   address/route/conntrackと設定・期限を必要な範囲で照合し、製品・試験・環境のどれか不明なら不明と記録する。
   修正または新しい観測仮説なしに同じ失敗を繰り返さない。同じ原因が2回連続で再現したら、次の実行前に
   異なる観測か再現条件の切り分けを決める。原因不明のまま1時間調査して新たな事実が得られなければ、
   未解決のまま独立作業へ移り、次に調べる具体的な観測を残す。原因を解消したとは扱わず、受入まで戻る。
5. **結果と次を短く更新する。** 実行版、環境、実際の結果、未検証と次の作業を引き継ぎへ記録する。
   原因判定に不要になった生ログ・capture・一時環境は削除する。関連変更は実通信、適切な補助検査、
   Race Detector・vet・ビルド・秘密情報検査後にまとまりごとにcommit/pushする。Actionsを待って手を止めない。

直近の順序は、(a) RAを受けた一般的な端末の自動DHCPv4/DHCPv6・SLAAC・DNSと実通信を確認し、
(b) homertの委譲IPv6アドレスを使うDNS配布を実装・検証し、(c) 自然更新を含む実HGW長時間試験を開始して、
その待ち時間に単体の他機能と独立した障害修正を進める。その後に二WAN・HA、更新・対象実機、cloud等を進める。
20秒や90秒の合格を長時間・対象実機の合格に置き換えない。直近の単体試験の所要時間は起動・復旧処理を除き
20秒/90秒、以前の実HGW自然更新試験は7800秒だった。未実装機能・対象機器・cloudの受入工数と環境が確定して
いないため、全体の完成日時はまだ見積もれない。各機能の実装と実環境での所要時間を測り、見積り可能な単位で更新する。

## 経過の振り返りと、停滞を繰り返さないための対策

時刻はUTC。以下はcommitと引き継ぎに残った事実であり、commit数を成果や作業時間の正確な内訳とはみなさない。

- **10月24日11:28開始〜25日**: 状態期限、条件評価、event journal、BFD/VRRP、更新基盤などを細かい単位で
  実装・検査した。独立peerや実packetを使った試験も一部あるが、homert一台に端末を接続し、DHCP/DNSから
  IPv4/IPv6インターネットまでを一構成で受け入れることは優先されなかった。開始〜25日末の348件のcommitのうち、
  191件の件名はtest/reproduce/exercise/require/cover/specify/record/document/assertで始まる。
  件名だけでは実際の中身や所要時間を判定できないが、細分化した検査・記録への偏りを確認できる。
- **10月26日**: DHCPv6・DS-Lite等の実装を進め、14:24の `3fe9318` では実上流で判明したDHCPv6と
  通常のlink-local DAD起動の相互接続問題を修正した。旧 routerd の知見と実peerを早く突き合わせていれば
  発見を早められた。実通信は進んだが、単体の全LAN機能を組み合わせた受入は残った。
- **10月27日**: 実HGWでの長時間通信・自然更新の確認に加え、PPPoE、DNS、PD保存、HA同期、stateless DHCPv6、
  Tailscale等を進めた。個別機能とpeerの成功を積み上げた一方、端末接続からの単体全体の確認を完了しなかった。
- **10月28日**: HAのPD・VMAC・ARP・NAT・連続障害へ長く集中した。実packetから問題を発見・修正したが、
  15秒の既存TCP失敗は残った。15:11の `f260875` で初めて、今回の実HGW単体試験にLAN DHCPv4/DHCPv6、
  DNS、DoH、条件付き転送とIPv4/IPv6を同時に組み込んだ。短期成功はしたが、委譲IPv6のDNS、自動OS resolver、
  長時間と対象機器の受入は残る。commit履歴だけでは各作業に費やした正確な時間は分からない。

**私の判断の問題**: 試験や内部契約を細かく増やすことと、実際の端末が必要な機能を使えることを取り違え、
最初に完成すべき単体ルーターを縦に通さなかった。旧 routerd のコード・実設定の参照も各機能の着手時に徹底せず、
実環境の制限を確認した後の代替実環境への移行と、失敗時の原因観測が遅れた。ユーザーが `AGENTS.md` で示した
実相手優先の意図に従った進め方になっていなかった。さらに、HAの一つの失敗に長く集中し、単体の基本機能や
全体の残作業を同時に見なかった。最近の固定DHCP値、IPv6文字列表記、DNS設定順などは私自身が試験へ持ち込んだ
初歩的な誤りであり、製品の難しさのせいではない。

**具体的な改善**: 上の各試験前の読み直しを必ず実施する。まず同一構成の実端末の利用経路を通し、
旧実装・現実のpeer・実設定に基づいて必要な修正を行う。各再試行は新たな観測または修正を伴わせる。
上記の2回・1時間の切り分け条件で視野狭窄を止め、未解決を残したまま独立作業へ進む。自然更新など
長時間待ちが本質的に必要な試験は実時間で動かし、その間は安全に独立作業を進める。成果は試験数・commit数ではなく、
端末の必要動作、実障害からの復旧、未検証の範囲で判断する。ゴールは全範囲の受入まで継続する。

## 現在の状況

- `lan-dns-dad-native55` は、隔離Linux上の独立ISC DHCPv6と端末で、委譲LANアドレスをRA・DHCPv6情報で
  配布し、端末からUDP/TCPでDNSを利用できた。委譲期限切れで旧アドレスとDHCPv6のDNSが撤去され、
  異なるプレフィックスの再取得後に新アドレスで配布と名前解決が復旧した。変更直後にアドレスが
  重複検出中のまま配布され通信できなかった問題は、kernelの利用可能状態を観測してから配布するよう修正した。
  再取得後のRAを即時判定した試験は、直前の送信から約1.75秒で、変更通知の最短3秒より前だった。
  再試験では実際の新RA受信を待って確認した。追加IPv4端末が同じ名前空間で作った重複IPv6経路も試験側で除いた。
  同じ隔離構成ではWAN側からアドレスへのpingが届く一方、LAN限定DNSのIPv6 UDP/TCPは届かなかった。
  `hgwdns-e2364af` の実HGW試験では、同じ機能に加えて二つの端末の自動取得、IPv4/IPv6通信と
  条件付きDNSが初回通過し、7800秒の自然更新試験を実行中。長時間結果、ONU直結、実回線からのDNS公開範囲は未検証。
- `nat-direction-native54` では、送信元NATの条件を `0.0.0.0/0` とした場合、修正前に独立端末へのTCP応答が
  LANではなくDS-Lite側へ再送出されることをpacketで確認した。接続追跡の往路だけに経路固定を適用する修正後、
  同じ構成で端末のTCP/UDP、主・予備WANの切替と、同一回線を引き継ぐ二台間の同じTCP/UDPの継続を確認し、
  二台構成の試験が完了した。外部アドレスが変わる回線間で既存TCPが継続することは意味しない。
- `delegated-dns-native54` の実HGW・隔離Linux端末による20秒試験では、RAが委譲LANのルーターIPv6アドレスを
  DNSとして配布し、端末からそのアドレスへの名前解決、IPv4/IPv6 HTTPS、PD workerの再起動後の通信を確認した。
  DNSはワイルドカードで待ち受け、試験構成のWAN入力を遮断した。この版のDHCPv6情報のDNSは固定ULAだった。
  続く7800秒試験はHGWが新たなPD要求に
  `NoPrefixAvail` を返し起動待ちで失敗した。短時間試験で取得したリースと別の識別子を使った影響が候補だが、
  確定していない。直前の成功時DUIDは残っていないが、以前PD取得できた試験機のDUID-LL・IAID 1に一致する
  有効な貸出をHGWの読取専用一覧で確認し、その識別子を再利用して最新版の長時間試験を開始した。
- `0f27e29` / `pd-prep-native53` は、独立 ISC DHCP、Linux AFTR、二台の実 daemon、LAN端末と二WANで通常の切替・復旧を完走した。
  同じTCPの最長応答待ちは一試行で約6.8秒、UDP損失0件。連続した切替では15秒のTCPタイムアウトを再現しており未解決。
- 同版を実 HGW に接続した90秒の単体試験は、独立 BusyBox 端末の DHCPv4取得・自然更新、DHCPv6情報応答、
  SLAAC、IPv4 NAT/IPv6 HTTPS、IPv4/IPv6 DNS、実Cloudflare DoH、独立dnsmasqへの条件付き転送・
  上流障害時の分離・復旧とPD worker再起動を一構成で通した。DHCPv6クライアントは明示起動した。
- 同版の90秒実HGW試験では独立dhcpcd端末がRAを受け、自動的にDHCPv4・DHCPv6情報・SLAACを取得した。
  取得したDNSを端末の隔離resolverに反映し、名前を手動指定せずIPv4/IPv6 HTTPSに接続した。
  BusyBox端末とdhcpcd端末の両方でDHCPv4自然更新を確認し、管理hostのresolver設定は不変だった。
  使用したresolver hookは試験用であり、対象端末のOS標準resolverとの統合は未検証。
- 初回は試験側のIPv6表記比較、固定IPv6アドレスの撤去判定、条件付きDNSの設定順、動的DHCP poolの固定値前提、
  dhcpcdのオプション要求不足が誤って失敗した。試験を修正して再実行した。長時間安定性は未検証。
- 以前の版で実 HGW の7800秒試験と自然 DHCPv6 Renew を確認している。最新版での長時間安定性には数えない。
  旧版のIPv6 DNS試験は固定ULAを使用していた。
- ONU直結、対象物理NIC・対象機器、arm64実行、homert全体、本番置換、各cloudは未受入。
  DHCPv6のstatefulアドレス配布は現在のstateless情報提供とは別機能で、未実装・未検証。

## 1. 単体の基本ルーター: homert02/03

**環境**: 実HGWまたはNTT ONUと隔離された実routerd2、独立した有線端末またはLinux/BusyBox端末。
同じ試験をhomert02/03の実intentに対応する構成で実施する。管理経路と復旧手段を先に確認し、hostの管理設定を変えない。
実ONUの直結試験には、その回線と対象NICが必要。実HGW経由の結果はONU直結として扱わない。

- 端末を初期状態で接続し、DHCPv4のDiscover/Offer/Request/ACK、アドレス、prefix、gateway、DNS、domain、leaseを受信し、
  人手による端末側アドレス・経路・DNS設定なしにIPv4へ到達する。複数端末・予約・アドレス競合・pool枯渇も確認する。
- WANのRA・DHCPv6 PDの取得と、LANへのprefix/RA、通常のDAD、SLAAC、default route、RDNSS、O/Mフラグを実packetと端末状態で確認する。
  DHCPv6 Information-request/ReplyのDNS/searchとSLAACを併用する。実設定で必要なstateful DHCPv6は実装し独立端末で取得・更新・撤去を確認する。
- 端末が配布されたDNSを使い、IPv4/IPv6、UDP/TCPで公開名とLAN内の必要な名前を引く。
  静的record、DHCP由来の名前・PTR、条件付き転送、設定順、上流障害時の問い合わせ先分離と復帰を確認する。
  実DoHのTLS検証・bootstrap・応答、設定に存在する他の転送方式を独立の上流で確認する。WANからの不要なDNS公開を確認する。
- IPv4 NAT/DS-Lite、IPv6 native forwardingをそれぞれ実TCP・UDP・HTTPSで確認する。NAT前後の送信元、実WANと戻りpacket、
  IPv6のsource/route、DNS結果とFIBを照合する。MSS、MTU、DF/PTBとfragmentを実packetで確認する。
- 端末の自然DHCPv4更新、DHCPv6情報更新、WAN DHCPv4/PDの自然T1/T2更新、RA/DNS期限を待つ。
  worker・control・daemonの個別再起動とrouter全体再起動を別々に行い、識別子、配布内容、端末通信と必要な永続状態を確認する。
- 端末を切断・再接続し、更新・再取得・期限切れ後に別端末へ重複配布しないことを確認する。
  WAN停止・復帰、PD変更、DNS上流停止・復帰、RA撤去時の新旧アドレスと通信も確認する。
- 少なくとも自然のWAN PD更新と複数回のLAN貸出更新を含む長時間、複数端末で定期IPv4/IPv6通信とDNSを続ける。
  長さは実際のlease/T1を観測して設定し、開始直後の90秒を長時間合格にしない。途中停止・部分成功・実行版を明記する。

## 2. 二WAN、firewall、経路と相互接続

- 独立二WANと端末で、HealthのTCP/ICMP/DNS/HTTP probeの実送信先・source・interface・mark、閾値と鮮度を確認する。
  障害、連続失敗、復帰、worker再起動、古い通知と元期限失効でPBR/FIBと実packetが一致する。
- 対象WANへ固定された既存NAT接続、新規TCP、UDPを分け、主・予備・両断・復帰を試す。
  元WANのNATアドレスを異なるWANへ流さない。時間を測り、繰り返した障害による実際の通信停止を記録する。
- IPv4/IPv6のfilter入力・転送・出力、管理アクセス、DNS公開範囲、許可/拒否、foreign nft/rule/route保護を実packetで確認する。
  MTU/MSS、fragment、PMTU、ECMP、VRF、回線別policyを必要な構成で確認する。
- 独立PPPoE ACでPAP/CHAP、IPCP/IPv6CP、DHCPv6 PD、NAT、切断・再認証とDS-Lite/PPPoE間の切替を実通信で確認する。
  実回線が要求する接続方式を対象回線でも検証し、仮想ACの結果を実回線の合格としない。

## 3. HA: 単体の機能が動く構成を二台にする

- 独立clientと独立VRRP peerまたは実二台で、選出、非preempt/preempt、track、GARP/NA、VIPとWAN/LAN VMAC、
  アドレス/DAD、RA公開と撤去、切替後の端末経路をpacketとkernelで確認する。
- DHCPv4の取得・更新・予約・期限切れ、DHCPv6情報、WAN DUID/IAID・PDと元期限を両方向へ引き継ぐ。
  待機側が未選出で配布せず、競合配布・不正Release・期限延長がないことを独立端末で確認する。
- 既存TCPの同一socket、UDP NAT mapping、新規通信を区別し、独立二WANとpacket captureで両方向の引き継ぎ・障害・復帰を確認する。
  接続追跡同期の準備状態、mark、実egress、VIP公開、Health、経路、各packetの時刻を照合する。
- worker/control/network/conntrackd/daemonの個別停止、同期断・復帰、古い通知、重複・逆順、snapshot転送中の切替、期限切れ、
  長い分断と双方停止後の復旧を試す。連続障害の15秒失敗は再現・原因修正・再試験まで未解決として扱う。
- 単体のDHCP/DNS/SLAAC/DoHとHAを組み合わせ、切替時に利用者から見える動作を確認する。新規接続だけで既存接続維持を合格にしない。

## 4. routing・接続機能

- BGP/BFD: 独立実装とIPv4/IPv6、single/multihop、VRF、recursive route、ECMP、BFD Down/復帰、撤回、再起動を確認。
  経路数・更新速度・収束は目標を設定して実測し、部分RIBを完全な状態として扱わない。
- WireGuard: 独立peerと双方向通信、endpoint再解決・roaming、key変更・取消、再起動、owned/foreign peer・routeを確認。
- IPsec/IKE: 独立peerと認証、rekey、DPD、NAT-T、切断・再起動、変更/削除、別接続の非干渉を確認。
- Tailscale: 独立tailnet peerとsubnet/exit-node、direct/relay、DNS、認証取消、identity再起動、route撤去を確認。
  Tailscale SSHとrouterd管理SSHは別々に検証する。

## 5. 設定、管理、運用と復旧

- API/CLI/UIから設定、表示、削除、認可、失敗応答、secret非表示を確認。表示された状態を実worker・kernel・通信と照合する。
- 設定を再適用し重複なく収束すること、複数operator/旧revision/重複request/遅延通知、適用途中の中断を確認する。
  confirmed commitの期限切れ・明示確認・rollbackと、復旧中のkernel実状態を確認する。
- logging/event/retention/export、時刻同期、clock step、disk full、worker停止、slow exporterを実環境で試し、
  forwardingを維持しながら欠落・失効・エラーを正しく表示する。DPIは実engineの分類結果で確認する。
- 権限境界、credential失効、管理外のkernel資源の保護、watchdog、停止順序、バックアップと復元を確認する。

## 6. 配布・対象機器・更新

- `CGO_ENABLED=0` のamd64/arm64製品ビルドと署名catalogを検査し、実bootで必要worker・engineを起動する。
  コンパイル・署名成功と対象architectureでの実行は別に記録する。
- 実SSH upload、検証、stage、明示reboot、health確認、更新失敗とfallback、電源断、disk full、署名改竄、
  kernel/module不一致、read-only rootfs、persistent stateと再起動後の管理到達を確認する。
- 対象物理機器・NICでONU/HGW、端末、障害と復旧を試す。性能・並列接続・pps・帯域・遅延の受入値は対象機器と運用条件に基づいて定め、実測する。
- 初回公開以降の対応製品版間の更新・戻し方を試す。未公開routerd2試作の格式・API互換は要求しない。

## 7. Cloud・SAM・Kubernetes

- cloud providerごとに許可済みの使い捨て環境でinventory/action、権限不足・遅延・重複・owner競合、実通信、
  停止・復旧・cleanupを確認する。費用や本番変更を自動的に許可されたと扱わない。
- federationのproducer/consumer、順序逆転、重複、停止、revokeと復旧を実node間で確認する。
- SAMのenrollment・claim・revoke・identity・placement、RR/leaf構成、WG/BGP、/32所有、NATなし双方向通信、
  移動・分断・古い通知・計画drainを実peerで確認する。各providerのlive受入を別々に記録する。
- KubernetesはPod netnsとhostNetworkを別々に、実traffic、reschedule、node障害、旧owner撤去、管理、update/rollbackとCNI資源保護を確認する。

## 8. 全34 work packageとの対応

| 試験範囲 | 対応 work package |
| --- | --- |
| 実設定・単体LAN/WAN・基本DNS/DHCP | CORE-01, NET-05, NET-06, QUAL-01 |
| Health・二WAN・firewall・PPPoE | NET-01, NET-02, NET-03, NET-04 |
| HA・lease/PD・既存接続 | HA-01, HA-02, HA-03 |
| BGP/BFD・WireGuard | NET-07, NET-08 |
| 設定transaction・依存・event・plugin・runtime | CORE-02, CORE-03, CORE-04, CORE-05, CORE-06 |
| OS・時刻・権限 | SYS-01, SYS-02, SYS-03 |
| 管理・観測 | OPS-01, OPS-02, OPS-03 |
| IPsec・Tailscale | EXT-01, EXT-02 |
| 更新・実機・cloud image・Kubernetes | DIST-01, DIST-02, DIST-03, DIST-04 |
| provider・federation・SAM | CLOUD-01, CLOUD-02, CLOUD-03, CLOUD-04 |

各項目の詳細な機能範囲は実装計画の各work packageを参照する。`make check`、Race Detector、vet、静的ビルドは
変更ごとの補助確認であり、上記の実通信や対象機器受入を代替しない。GitHub Actionsの完了はローカル開発の待ち条件にしない。
実機・回線・cloudが利用できない場合は必要な接続先と未実証範囲を明示し、独立して進められる実装・試験を続ける。
全項目の見積りに必要な実環境と未実装範囲が確定していないため、現時点で完成日時は約束しない。
