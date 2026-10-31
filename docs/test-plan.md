# routerd2 実通信・製品受入試験計画

更新: 2026-10-31。対象は [実装計画](implementation-plan.md) と同 JSON の全34 work package。
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
ゴール開始は2026-10-24 11:28:25 UTC。2026-10-31 11:56 JST時点で約6日15時間28分が経過した。
進捗判断では経過日数と、未完了の機能・実環境を意識する。

1. **対象と結果を決める。** 実際のhomert設定・旧 routerd・現行の本番経路を読み、端末、peer、必要な動作、
   成功を判定する端末側の結果とpacket/kernelの観測を明示する。短時間・自然更新・実機を区別する。
   実機・回線がない場合は未検証とし、別の独立作業を進める。
2. **試験の前提を現実と照合する。** 初期設定・接続条件・管理経路・復旧・所有権を確認する。
   DHCP poolの動的割当を固定値と決めつけない。固定予約を試す場合は予約を明示する。
   IPv6等は表記文字列でなく値として比較し、端末の実取得値を使う。試験の便宜の手動設定を隠さない。
3. **実物で一つの利用経路を通す。** 製品プロセス、kernel、独立client/peerで要求から実通信まで確認する。
   試験開始時に、実行版・環境・対象・判定と必要な観測を把握する。作り込んだ模擬実装を先行条件にしない。
   自然更新を待つ稼働コホートを、途中で故障注入へ転用しない。故障再現は別stageの専用短時間試験で行う。
4. **失敗のたびに観測し、次の行動を決める。** 失敗した時刻の端末状態、protocol packet、worker、kernelの
   address/route/conntrackと設定・期限を必要な範囲で照合し、製品・試験・環境のどれか不明なら不明と記録する。
   修正または新しい観測仮説なしに同じ失敗を繰り返さない。同じ原因が2回連続で再現したら、次の実行前に
   異なる観測か再現条件の切り分けを決める。原因不明のまま1時間調査して新たな事実が得られなければ、
   未解決のまま独立作業へ移り、次に調べる具体的な観測を残す。原因を解消したとは扱わず、受入まで戻る。
5. **結果と次を短く更新する。** 実行版、環境、実際の結果、未検証と次の作業を引き継ぎへ記録する。
   原因判定に不要になった生ログ・capture・一時環境は削除する。関連変更は実通信、適切な補助検査、
   Race Detector・vet・ビルド・秘密情報検査後にまとまりごとにcommit/pushする。Actionsを待って手を止めない。

直近は実HGWと独立端末での単体ルーターの利用経路を基準に、homertで必要な未完成機能を優先して組み込む。
実通信の失敗や実装に必要な不明点だけを切り分け、細かな試験項目や試験コードを増やすことを進捗としない。
自然更新を含む長時間試験の進行中も、単体の残機能、二WAN・HA、更新・対象実機、cloud等を進める。
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

- `health-ready-native105`: `make check-health-pbr` は独立二 WAN namespace peer と実 supervisor/control/network/Health worker で、Health→PBR の障害切替・復旧、worker/supervisor 再起動、古い通知・期限切れ、confirmed rollback、TCP/UDP と packet capture を確認した。対象回線・対象機器は未受入。

- `dhcp-ingress-native105`: 隔離 PVE（4 CPU/4 GiB）の実 appliance で、ハッシュ・release ID を照合した三実行物と20 Ready moduleを使い、cold boot/reboot 後の DNS UDP/TCP（IPv4/IPv6）、TCP/UDP、BGP、NTP、Tailscale、および通常・特殊端末それぞれの120秒 DHCPv4自然T1 unicast/同一XID ACK/option 54=`172.18.0.1` を確認した。PVE kernel 試験であり、12時間の元lease、ONU/HGW・対象機器、全34 package は引き続き未受入。

- `persistent-sysctl-ba46e40`: homert両機の永続conntrack設定を実CLIで取り込み、隔離Linux名前空間の実製品で設定確定・通信適用プロセス再起動・製品全体再起動からの再適用・撤去時の元値復元が成功。空の初期設定より保存済み設定が優先されたことを確認。カーネル初期値への戻しは隔離試験内の明示操作であり、物理再起動やHA通信の受入ではない。証跡は `dist/sysctl-acceptance/`。

- `local-redirect-native78`: 実製品内DNSでA/AAAA宛先集合を更新し、両IP/TCPUDPの該当・非該当、集合変更、全daemon再起動、撤去復帰が終了0。
  製品health成功後に通信を開始した静的端末の局所試験。起動直後の集合未解決中のWAN流出を修正済みとは扱わず、起動前からの自動取得・最初のDNSを次に確認する。
  全make check/Race/vet/両architecture build/署名六ELFは終了0。全homert通信・運用の代替とHA timeoutは未受入。
- `blackhole-native77`: 実homertのIPv4破棄経路を単一network writerへ追加。
  `make check-blackhole`で実daemon・独立LAN/WAN端末のTCP/UDP破棄、対象外通信、全プロセス再起動後の保持、
  LAN受信/WAN流出0、具体経路の到達、設定撤去後復帰・管理外経路保持が終了0。関連Race/vet/両architecture build/署名六ELFも終了0。
  全homert取込・IPv6破棄・実機資格と、未解決のHA timeoutの解消には数えない。
- `dns-input-native76`: 状態報告待ちからDNS入力を分離し、元期限を維持する。旧候補の08:12 TCP DNS SERVFAILとの因果は未断定。
  実Alpine/18秒PDの署名更新準備99.57秒・正常再起動92.94秒・確定・取得DNS/両IP通信が通過した。
  完全な次候補の設定取り込み後にHealth先だけを止め、90秒の判定で旧版/旧設定へ自動復旧した。
  管理・失敗記録・DUID/SSH保持・端末DNS/両IPを照合し、異常候補の起動から復旧253.49秒、全体終了0。
  全make checkは終了0。併行中の同候補の二台切替/再参加はVRRP適用要求timeoutで失敗し、原因未特定。関連Race/vet/両architecture build/署名六ELFは終了0。
  `sourceCIDRs`正規化は実daemonでTCP/UDPの許可・拒否とcaptureが通過。全homert取込は91件で拒否し、全体成立ではない。
  07:07:11開始の別診断版HGW観測は130分/724巡で終了0。09:07:18の自然Renew/Reply一組と全daemon再起動後10.23秒の両端末復帰を確認。全設定/対象機器・全34項目は未受入。
- `pd-authority-native74` はPD元期限と監視照会の1秒期限を分離した。実APKでcontrol交換後に端末のIPv6既定経路が戻り、
  18秒のISCリース一周期を越える20秒以上の両IP TCP/UDP/DNSと、RA再撤回なしが終了0。段階全体66.37秒は継続観測を含む。
  実HGW長期は02:18 JST頃のLAN IPv6 UDP DNS timeoutで終了1。自然更新前に停止し、LAN返信経路の修正後に再開する。
- `admission-native73` は、旧homertの公開準備を実network ACK・元期限内PD・現在のRA送信世代へ接続した。
  ISC/Keepalived/dhcpcdの条件付きLAN、実VIPをgatewayに使う独立5 WAN、署名PID 1二台の双方向PD/DHCPと
  同じTCP/UDPの引継ぎ・元期限・自然失効・保持・障害復旧が終了0。自己DNS失効時は空Replyを要求せず、
  実BusyBoxの取得timeoutと旧DNS停止・別prefixでの復帰を確認した。全homert取込は119 issueで拒否される。
  同じ最終版の実HGW+準備条件/VIPは90秒/9巡とPD交換・全体再起動後の両端末通信も終了0。単一出口の確認で、全34項目・全設定・対象実機・最新版の自然PD更新は未受入。
- `bgp-frr-e2364af` は隔離Linux上の独立FRRと、homertのLANプレフィックス広告に相当するIPv4 BGP交換を実施した。
  FRRの経路をLinux FIBに反映し、LAN端末のTCP/UDP転送、daemon停止時の広告撤回、再起動後の広告と通信復旧を
  確認し、BGP・端末通信のpacketを採取した。FRR側はzebraを使わず、端末への戻り経路を試験用に静的設定した。
  homert実対向、IPv6 BGP、性能と全BGP機能の受入ではない。
- `lan-dns-dad-native55` は、隔離Linux上の独立ISC DHCPv6と端末で、委譲LANアドレスをRA・DHCPv6情報で
  配布し、端末からUDP/TCPでDNSを利用できた。委譲期限切れで旧アドレスとDHCPv6のDNSが撤去され、
  異なるプレフィックスの再取得後に新アドレスで配布と名前解決が復旧した。変更直後にアドレスが
  重複検出中のまま配布され通信できなかった問題は、kernelの利用可能状態を観測してから配布するよう修正した。
  再取得後のRAを即時判定した試験は、直前の送信から約1.75秒で、変更通知の最短3秒より前だった。
  再試験では実際の新RA受信を待って確認した。追加IPv4端末が同じ名前空間で作った重複IPv6経路も試験側で除いた。
  同じ隔離構成ではWAN側からアドレスへのpingが届く一方、LAN限定DNSのIPv6 UDP/TCPは届かなかった。
  `hgwdns-e2364af` の実HGW試験では、同じ機能に加えて二つの端末の自動取得、IPv4/IPv6通信と
  条件付きDNSが初回通過し、7800秒試験は自然PD更新を越えたが、最終通常PD交換後の端末の名前解決失敗で終了1。完走ではなく、最新版の長時間・ONU直結は未検証。
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
  試験用DUID、MACとWANリースの現在のcheckpointを非公開ディレクトリに保存し、次回は `UPLINK_STATE_DIR` で
  読み戻せるようにした。存在しないuplinkでの起動失敗後も保存内容が変わらないことを確認したが、
  別の保存済み識別子では、実HGWとの別実行間でもDUID・IAIDと委譲prefixが維持された。
- 同版の別の実HGW試験で、隔離routerのWAN通信を遮断して復旧後、両端末のIPv4/IPv6通信が復帰した。
  daemon全体の停止・再起動後も同じPD識別子とprefixで両端末の通信が復旧した。物理リンク断や機器の再起動ではない。
  dhcpcdは隔離されたresolverファイルに標準hookでDNSを反映した。systemd-resolvedとの統合は未検証。
- 稼働中の実HGW構成では、独立dhcpcd端末からIPv4/IPv6それぞれ1 MiBのHTTPSダウンロードも完了した。
  `homert-dns-reference` は、homertが使う委譲アドレスの `address` 参照を変換し、独立ISCとLinux端末で
  RA/DHCPv6の同じDNSアドレスとUDP/TCP名前解決を確認した。旧設定の条件付きHA全体は未受入。
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

`lan-dnsmasq-history` は旧routerdを再利用したDHCPv4で、実HGW・自動取得する独立二端末の
DNS利用とIPv4/IPv6通信、自然DHCPv4更新、PD通常/強制交換と製品全体再起動後の復旧を確認した（通信75秒）。
同版の署名付き二台・独立端末では、同期断と解放・通常停止の重なり、元の3日保持期限、重複防止と再取得、
同じTCP/UDPを確認した。先行版の自然貸出失効・古い所有者の試験と区別する。200ms広告、最新版の長時間、
全体の二台実回線・物理NIC・後続環境の資格は残る。旧7800秒試験は自然PD更新後の最終復旧で終了1だった。

`lan-reuse`は独立Headscale/公式Tailscaleとの署名PID 1試験も完走した。さらに実HGWの同一製品で
自動WAN/PD・LAN配布・DNS/DS-LiteとTailscaleを同時に75秒使い、PD通常/強制交換と全体再起動後の
通信とidentityを確認した。専用対向underlayは固定値、LAN発IPv6には明示NATを追加したため、
現行homert設定と同一とはしない。広告IPv4 LAN着信・LAN発両family・両familyの出口が対象である。
19:44 UTC開始の7800秒版は21:31:52 UTCのDNS SERVFAILで終了1。自然PD更新・完走を合格に数えない。

直近候補`dynamic-dns-native79`は実HGWの起動前端末接続から90秒/9巡、初回両IP DNS/HTTPS、VIP/PD動的名、LAN更新と全daemon再起動を通過。動的記録のみの入力購読漏れも修正し、既存PD/kernel/dig試験で変更・失効・再取得を確認した。自然PD更新と二段DNSを含む全homert同時受入は残る。詳細・実行版差は[引き継ぎ](pve-codex-handoff.md)。

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

2026-10-31 12:53 JST: 基点コミット `a58f894`、実行版 `ops-dhcp-long-native108` の通常起動イメージで、
独立した Alpine/KVM 二台（各4 CPU・4 GiB）による全設定の通信試験が終了0で完走した。
採取済みの各109資源を最新CLIで変換し、通信分類も有効にした。試験ではNICの対応付け、明示した管理アドレス、
共有WANアドレスとBGP次ホップ、独立通信相手の認証設定を使用した。対象実機へは配備していない。
自動DHCP・SLAAC・取得DNSによる両IP通信、特殊端末のIX経路、PDと4系統AFTR、BGP、NTP、
Tailscaleの三拠点経路が成立した。両方向で実VMを停止し、同じTCP接続（最初のSYN一回）とUDP変換先、
元のリース・DUID・DNSを保持した。旧主系が不在の新規端末接続と、両機の再参加後の通信も成功した。
切替後の全通信確認は26.356秒と15.925秒。TCPの最大応答待ちは7.166秒、独立送信UDPの最大無応答は6.855秒だった。
UDPは1577送信・1348受信・229損失、DNSは918照会中16回失敗であり、無停止の結果ではない。
自然更新、実回線・対象NIC、実運用の二台同時出口利用、更新・切戻しの受入はこの結果に含めない。
旧設定が同期を要求していないIX側の既存SNAT接続も、継続成功とは扱わない。専用の二台は試験終了後に撤去した。

2026-10-31: current `ops-native106` 配布物と実 fixture で、隔離 Linux namespace の二台 HA DHCP を実行した。`scripts/ha-state-peer.py --dhcp-scopes --binary-dir dist/bin` は通常・特殊端末の配布、両方向切替、同一 TCP 接続と UDP 変換先の維持を通過した。fixture は physical `ingress_interface: lan0` を明示する。`--initial-leases` は独立 dnsmasq の有限 lease と元期限の取込、片系停止中の新規取得、取得 DNS の UDP/TCP、再参加後の IPv4 TCP/UDP、両端末の更新と同期を通過した。更新は SIGUSR1 で促しており、43,200 秒 lease の自然更新、IPv6/PD/DS-Lite 全体、対象機器の受入には数えない。

2026-10-30訂正: TCP応答後にだけUDPを送る旧試験の「UDP損失0」は、TCP停止中の連続UDPを測定していない。native80二段DNSの両方向切替では独立50ms送信を使い、985送信・810受信・175期限切れ、最大無応答4.543秒、同じTCP最大応答待ち4.575秒だった。新規通信と同じsocketの継続、損失と復旧時間を分けて判定する。

`handoff-stop` / native-56は、同期断中のDHCP再起動が停止時の通信待ちでVRRP適用を止める経路を修正し、
署名PID 1二台・独立ISC/AFTR・二WANで、両方向切替、元PD期限失効と回復、解放・保持・再取得まで完走した。
同じTCPはSYN一回、UDP損失0、最長応答待ち6.82秒。最終設定撤去は待機系。1秒広告の一構成であり、
旧15秒失敗の全条件、200ms広告、現行homert全設定・実機の受入に拡張しない。

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

2026-10-31: 基点 `1a27177` の修正版 `ops-tailnet-defaults-native111` で、旧実装が使う
Tailscale標準の転送・送信元変換を復元しました。設定は `NetfilterMode=2` と `NoSNAT=false` です。
独自のNAT管理処理は追加せず、固定した公式エンジンが規則を所有します。
両現行homertの読み取りで確認した、管理NICのDHCP既定経路（優先度100）も移行条件へ含めました。
旧設定ファイルの外にあるOSの経路は自動取り込みの対象ではありません。

隔離した4 CPU・4 GiBの二台に全設定を読み込み、管理アドレスと既定経路をDHCPで自動取得しました。
各機を出口に指定したIPv4/IPv6のTCP/UDPが成立し、相手が受け取った送信元と各NICの実アドレスが一致しました。
両機を順に停止・再起動し、同じ出口指定での復旧を既存の共通60秒枠内で確認しました。
復旧確認の開始から全通信成立までは6.386秒と4.938秒で、途中の失敗3回も記録しています。
先行試験ではIPv6の重複確認要求から約0.786秒後のTCPだけが別アドレスへ変換され、後続UDPは正常でした。
重複確認の完了時刻は未採取であり、サービスの起動完了を全WAN通信の即時成立とは扱いません。

LANの既存TCP接続とUDP変換先は両方向の切替で維持しました。TCPの最大応答待ちは7.390秒です。
UDPの200ミリ秒応答期限超過は205/1750回、最大無応答は5.815秒、DNS失敗は20/1012回でした。
元の長時間自然更新、対象機器の回線・負荷、別の出口識別子への自動変更は、この試験の受入範囲に含めません。
公開手順の `scripts/tailscale-peer.py` から試験用NAT資源を除き、同版で標準モードも完走しました。
直接接続・中継、両IPの転送、条件失効、認証取消、設定復帰、タグ付き再登録を確認しています。

## 5. 設定、管理、運用と復旧

2026-10-30: UDP syslog、OTLP/gRPCログ、operational event保持、DNS query保持、NFLOG packet記録を実製品と独立receiver/clientで確認した。転送先障害・復旧、並行通信、元の保持記録、worker再起動と撤去を機能ごとに確認している。DNS保持/NFLOGは統合版native84でも通過。OTLP metrics/traces、残る履歴datasetとDPIは未完成。各機能の成功を全homertや通常factory全設定起動の受入へ読み替えない。現在版・範囲と未解決は[引き継ぎ](pve-codex-handoff.md)を読む。

2026-10-30 追加: homertの実設定にある旧OTLP LogSinkとDHCP由来DNSの取込を修正した。旧OTLPSink.Emitは送信せず、Telemetryだけが送信を所有する。独立Collectorで送信先変更・停止復旧・重複なしと並行UDP通信を確認した。旧DDNS表記はDNS UPDATEを有効にしない。名前付きDHCP生成元と明示した有限リース方針から、端末の名前変更、A/PTR、実DHCPRELEASEによる消失を確認した。BusyBox 1.36.1の`-q -R`で解放パケットを送信せず失敗する試験前提を修正し、ACKで取得したアドレスとマスクだけを端末へ適用した。固定の返却アドレスや追加経路に依存しない。

FirewallEventLogは旧logstore.RecordBatch/ApplyRetentionを再利用して、同じ収集処理が保存と期限削除を所有する。独立端末の両IP通信、TCPフラグ、製品再起動後の保持、実送信エラー中の期限切れ行保持・新規記録、復帰後の削除前UDP転送を確認した。試験では7日保持/日次巡回を8秒/1秒へ縮めた。7日間の経過、過負荷時の無損失保存、実機受入を示すものではない。新たな外部デーモンは追加していない。

- API/CLI/UIから設定、表示、削除、認可、失敗応答、secret非表示を確認。表示された状態を実worker・kernel・通信と照合する。
- 設定を再適用し重複なく収束すること、複数operator/旧revision/重複request/遅延通知、適用途中の中断を確認する。
  confirmed commitの期限切れ・明示確認・rollbackと、復旧中のkernel実状態を確認する。
- logging/event/retention/export、時刻同期、clock step、disk full、worker停止、slow exporterを実環境で試し、
  forwardingを維持しながら欠落・失効・エラーを正しく表示する。DPIは実engineの分類結果で確認する。
- NTPは配布イメージの仮想RTCを意図的に遅らせ、DHCPv6学習→実時計補正→独立LAN client→
  再起動後のdrift読込と再取得を通す。同期済みflagと、端末が採用できる時刻品質を区別する。
  初期周波数推定は既定のpollと標本数で実測し、clientのmaxdistanceや採用条件を緩めない。
  二度目の起動のNTP packet、trackingのskew/root distanceと端末採用時刻も残す。
- 権限境界、credential失効、管理外のkernel資源の保護、watchdog、停止順序、バックアップと復元を確認する。

## 6. 配布・対象機器・更新

2026-10-31 13:15〜13:22 JST: コミット `1a27177` の同一保存形式・同一ソースから作った
`ops-dhcp-pdscope-native109` と `ops-ha-update-native110` を署名し、homertの実設定を取り込んだ
隔離Alpine/KVM二台（各4 CPU・4 GiB）で、待機機更新→再参加→装置切替→現設定を保持した旧版復帰が終了0で完走した。
更新後の再参加は19.788秒、旧版復帰は20.350秒。署名した実行版の起動・正常確定と、設定本文・SSHの身元・Tailscaleの識別子を照合した。
独立端末の自動取得DNS、両IPのTCP/UDP、BGP宛先、特殊端末・新規端末、TailscaleのLAN宛通信、両台のNTP採用も確認した。
同じTCP接続とNAT対応を保持し、最大応答間隔は7.103秒。UDPは2483送信中225欠落、最大無応答5.577秒、DNSは1456回中16失敗で、無停止とはしない。
署名準備は既存の一台更新手順を再利用した `scripts/homert-update-artifacts.py` を用い、各台の永続ディスクを分離した。
私有の再実行手順は監督作業場へ保持し、専用VM900/901は削除した。異なる保存形式間の互換性、Tailscale出口通信、
元の長いリースの自然更新、対象homert実機・実回線での受入をこの結果に含めない。

2026-10-30 20:12〜20:16 JST: `1def991` の署名付き三版を使い、隔離した4 CPU・4 GiBのKVMで更新と復旧を確認した。
独立ISC・PPPoE対向・Linux端末で、DHCP取得値による標準resolver、両IPのTCP/UDP、18秒リースの自然PD更新、
アップロード中断と並行通信、正常更新、明示した旧版への復帰20.68秒、Health障害による自動復旧112.14秒が通過した。
旧版へ戻した後も現設定・DUID・SSHの身元を保持した。候補の試行開始から90秒で失敗を記録し、既知正常版へ戻った。
先行試行は必須WANの`ready_when`指定がなく、条件付きPD/Healthの起動前に正常確定して失敗した。
既存の準備条件を明示し、90秒の期限と2秒の継続条件は変更していない。512MiB・2CPU・TCGでは短いDNS観測期限切れと
端末のSERVFAILが残り、解消扱いにしない。同じソースの別識別子による受入であり、実homert全設定、異なる製品版間、
対象機器、長時間自然更新、破損した実行ファイルの復旧は未受入。[再現手順](releases.md)を残し、専用VMは削除した。

`handoff-stop`はAlpine3.24.3/Linux6.18.59-0-ltsの実QEMU/amd64起動で、同梱DHCP engine→独立端末の
自動IPv4/gateway/DNS→PPPoE/PD/SLAAC→両family TCP/UDPとDNS、PPP/control交換、通常電源断まで通った。
端末のDNS問い合わせ先は受信値を使用するが、OS標準resolver統合の試験ではない。署名更新・失敗復旧、
homert全engineのイメージ、対象実機の受入は別に残る。

- amd64/arm64の通常配布ビルドと署名付き一覧を検査し、実起動で必要な担当プロセスとエンジンを起動する。
  `routerd` と `routerd-logging` は `CGO_ENABLED=0`、nDPIを組み込む `routerd-modules` は
  `CGO_ENABLED=1` とし、いずれも静的リンクを確認する。
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

## OTLPログ転送の独立受入

2026-10-30 13:54〜13:55 JST、`ba46e407`を基にした`otlp-ba46e407`と
OpenTelemetry Collector 0.151.0を隔離した名前空間で起動しました。
製品自身が発した記録の本文、重要度、元時刻、世代、読み取り位置、属性の受信を確認しました。
収集器の停止中もローカル保存と管理処理は継続しました。宛先変更、停止中の設定置換・撤去、
収集器復帰後の新規受信が成立し、並行したUDP通信は71往復、失敗0でした。

再現手順は`make check-otlp`です。実行版のハッシュと受信記録は`dist/otlp-peer/`にあります。
これはログのみの受入です。計測値、トレース、TLSの実相手機での受入は未確認です。
