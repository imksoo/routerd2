# routerd2 開発引き継ぎ

2026-10-28。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)、
[試験計画](test-plan.md)、[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

## 現在のLAN実装: routerdの再利用（2026-10-28）

`reuse-routerd-lan` は独自DHCPv4割当器・wire処理・JSON台帳・私設状態APIと対応する旧試験を削除し、
旧 `RenderDnsmasqLines` と固定版dnsmasq/muslを接続した。配布archiveと対応sourceを同梱し、
supervisorが検証したFDを渡す。PID 1から管理し、host dnsmasqやsystemd/OpenRCには依存しない。
DNS・DHCPv6・RA・VRRP選出は既存Go worker、接続状態同期はconntrackdを使う。
外部依存が増えた事実とGo中心の目標は区別し、他protocolの一律外部化は決めていない。

貸出の正本は `dnsmasq.leases`。Go workerは旧routerdの実要求・解放履歴を一つの所有者として記録し、
保持hostをdnsmasqへ渡す。起動ファイルの再通知を再取得にしない。通常停止時は最後の通知を保存し、
script子プロセスを回収してから通知口を閉じる。古いpeer貸出が確定した解放を消す経路を除いた。
内部contractはnative-56。破棄したprototype状態の互換readerは作らない。

`lan-dnsmasq-history` の署名付きPID 1二台・独立BusyBox・kernel NAT・captureで、両方向切替と同じ
TCP/UDP、更新・別端末の異なる割当・同期断復旧、worker/engine交換、3日保持と再取得を確認した。
実RELEASE中に履歴workerを止め、同期回線断と製品停止を重ねても、通知の保存、古いpeer貸出の排除、
両側の元保持期限、第三者への割当防止、再起動と元端末の再取得が成立した。
先行版では自然貸出失効と古い所有者の再昇格も確認したが、最新版の全分断条件の資格にはしない。
1秒広告の範囲の結果であり、200msで旧ノードが再選出された事象は未解決。

同版の実HGW・独立BusyBox/dhcpcd二端末では、標準resolver hookによる自動取得、DNS/条件付き上流/DoH、
IPv4/IPv6 HTTPSとDHCPv4自然更新、PD強制終了1.89秒・通常交換3.79秒・全体再起動3.84秒での通信復旧が
成立した。通信区間75秒。ONU直結・対象物理NIC・長時間安定性・homert全体の置換は未受入。
`lan-reuse` は明示option 6を含む独立BusyBoxのnative/移行DHCP→DNS、元観測期限、復旧も通った。
`lan-reuse` の make check（Go/Python・Race Detector・vet）、両architectureのCGO_ENABLED=0ビルド、
実6 ELFの署名catalog/publisher/stage検査、DHCP engine/sourceを含む両architectureの実CLI pack/stageが成功。
初回publisher検査は開発worktree/build先のgroup書込権限で拒否され、所有build先を直して同じ検査が通った。
ARM64実行と、この変更を含むappliance実行は未検証。次はTailscale・homert全体二台、k8s/pve差分、
Cloud SAM、配備更新復旧を計画順に進める。新しい有料環境・本番切替の許可は推定しない。

旧 `hgwdns-e2364af` の7800秒試験は通信727回と自然PD更新1回まで進み、PD SIGKILL復旧も通ったが、
最後の通常PD交換後に自動端末の `curl(6)` で終了1。完走とは扱わない。旧resolver hookを使った実行であり、
新しい標準hookの上記75秒結果と混同しない。長時間の最新版受入と実機の資格は残る。必要なWAN識別子と
更新済みgrantだけをprivate stateへ保持し、解析後の生ログ・capture・一時環境は削除する。

## 単体 router・LAN 端末を優先

`lan-reuse` / native-56の新統合版は、独立Headscale0.29.6・公式Tailscale1.104.2による
署名PID 1の既存試験を完走した。固定WAN/LANでの両family subnet/exit、DERP、worker/control/全体交換、
identity、期限・取消・再認証、DNS、tags/shieldsUp、confirmed rollbackとcaptureの範囲である。
さらに実HGWの同一製品でWAN DHCPv4/RA/PD→LAN DHCPv4/SLAAC/DHCPv6情報/DNS/DS-LiteとTailscaleを
同時に使い、独立自動端末からのIPv4/IPv6 TCP/UDP、広告IPv4 LANへの着信、両familyのInternet出口を
通信75秒で確認した。PD強制交換1.27秒・通常交換4.09秒、全体再起動4.06秒で端末通信が戻り、
全体交換後のTailscale identityと上記通信も保持した。`make check`と両architectureの静的ビルドも成功した。
Headscale/公式peerの専用underlayは試験用固定192.0.2.0/24。IPv6 LANは広告せず、LAN発IPv6には
明示masqueradeを追加した。このNATは現行homert intentとの差分であり、同一設定の置換合格とはしない。
出口だけでは未広告LANへの着信・返信を許可しないupstream動作を、両端captureと
1.104.2の`ipn/ipnlocal/local.go`で確認した。未広告の委譲IPv6 LANへの着信を受入条件へ加えない。
19:44 UTC開始の同構成7800秒試験は進行中。観測T1は開始後約7180秒で、完走・自然更新は未確定。
PD専用captureを加え、traffic用captureの循環で更新packetを失わない。次は二台全体と残る配備・更新へ進む。

`hgwdns-e2364af` は実HGWと独立Linux端末で、委譲IPv6アドレスによるDNS配布、
DHCPv4/DHCPv6情報・SLAAC、条件付きDNS・DoH、IPv4/IPv6通信を確認した。
隔離WANの通信遮断からの復旧、daemon全体の再起動と保存状態を使った別実行でも通信・PD識別子が復旧した。
旧版の7800秒試験は自然更新後、最終通常交換の名前解決失敗で終了1。ONU直結・対象機器での受入は未実施。
細分化した試験を増やすことより、homertに必要な機能を端末から使える構成に組み込み、実通信の失敗を直す。

`homert-dns-reference` は稼働homertと同じ `dnsServerFrom: IPv6DelegatedAddress/...` の `address` 参照を
既存の `DNSDelegation` に接続した。旧routerdの `chain.go` のDHCPv6Server処理と `expandServerSources` を参照した。
独立ISC・Linux端末・実daemonで、変換した設定によるRA/DHCPv6の同じ委譲DNSアドレスとUDP/TCP名前解決を確認した。
既存試験の古い端末アドレス保持を修正し、元期限失効・再取得・worker交換・confirmed rollbackも含め完走した。
Race Detector、vet、amd64/arm64静的ビルドが成功。条件付きHAの設定変換とhomert全体の受入は残る。

`pd-prep-native53` の実 HGW 90秒試験で、独立 BusyBox 端末の DHCPv4による address・gateway・DNS取得と
自然更新、DHCPv6 Information-request/Reply、SLAAC、IPv4 NAT/IPv6 HTTPS、IPv4/IPv6 DNSを確認した。
公開名は実 Cloudflare DoH、条件付き名前は独立 dnsmasq へ送り、条件付き上流の停止時に公開上流へ
問い合わせを流さず SERVFAIL、復旧後に応答することを確認した。PD workerの強制終了・通常停止後も
同じDUID/IAIDと通信が復旧した。別の90秒試験では独立dhcpcd端末がRAからDHCPv6情報取得を自動起動し、
DHCPv4とSLAAC、取得DNSを隔離resolverへ反映して名前を手動指定しない両family HTTPS/DNSを使用した。
両端末のDHCPv4自然更新と管理hostのresolver設定不変も確認した。resolver hookは試験用であり、対象端末の
OS標準resolverとの統合は未検証。IPv6 DNSは固定ULAで、homertの委譲IPv6アドレス方式は未受入。
90秒は長時間安定性ではない。
初回試験はIPv6表記の文字列比較、固定IPv6アドレスを含めた撤去判定と条件付きDNS設定順の誤りで失敗し、
各試験を修正して再実行した。実ONU直結・対象物理機器での受入は未実施。

## 作業中: PD 引き継ぎ準備 / native-53

旧 routerd の DUID 明示値優先、DUID-LL、DUID/IAID と期限を照合したリース復元を参照した。
保存した自動 DUID はリースと別に保持し、HA では同じ DUID/IAID と元の grant 期限を使う。
昇格時、共有済みの生存 PD から B4 のみを先に準備し、WAN link-local と B4 の DAD を並行させる。
準備中は LAN RA、DHCP serving、AFTR を公開せず、B4 の有効期間は元の PD と選出期限の短い方に制限する。
実二台と独立 ISC DHCP・Linux AFTR・LAN client の試験では両 DAD の重複、準備中断時の B4 撤去、
元の TCP/UDP の復旧、および control 停止中の選出期限による B4 撤去を確認した。
通常の二台 HA 全体も、独立 peer との双方向切替、TCP/UDP、DHCPv4・PD の元期限失効、
予備 WAN と DS-Lite の復旧、設定撤去および packet capture を含めて完走した。同一 TCP の最長応答待ちは
この一試行で約6.8秒、観測された UDP 損失は0件だった。`make check` の Race Detector と vet、
amd64/arm64 の静的ビルドと実バイナリの署名・配置検証も成功した。arm64 実行は未検証。

同版を ens18 の隔離 namespace から実 HGW に接続し、30秒の IPv4/IPv6 HTTPS、学習 DNS、
PD worker の SIGKILL と通常再起動後の同じ DUID/IAID・prefix と通信復旧、寿命0 RA・kernel 撤去と
上流への Release がないことを確認した。自然 Renew は未到達。ONU 直結、対象物理 NIC と本番受入は未実施。
連続障害の一試行では TCP が15秒で失敗し、予備 WAN への元の主 WAN アドレスの誤送出は観測されなかった。
別試行では同じ TCP の双方向切替と PD の元期限失効・再取得まで到達したが、最後の HA 設定撤去で
control API が3秒以内に完了せず失敗した。連続障害と設定撤去の原因調査を継続する。
追加の連続障害試験では、一回は既存 TCP が約13.3秒で復旧し、別の一回は15秒で終了した。
別試行の昇格直後には既存 conntrack は復元済みだったが、主 WAN 用 route table はまだ存在せず、
mark 付き route lookup は予備 WAN に向いた。誤送出防止がその TCP を止め、主 WAN 復帰後の
再送で通信が回復した。再送間隔も長くなるため、VIP の復帰時刻だけでは TCP の復旧を示せない。

## 作業中: VRRP の ARP 応答と連続障害

旧 `routerd-vrrp-vmac` は Linux が親インターフェースの MAC で子の VIP に
ARP 応答することを防ぐため、親と VMAC の `arp_ignore=1` を設定していた。
routerd2 にこの処理がなく、実二台試験でクライアントが旧ノードの物理 MAC を
学習し、切替後も旧ノードに TCP を再送する問題を観測した。IPv4 を持つ管理対象
macvlan の親と子に `arp_ignore=1` を適用し、アドレス撤去後に元の設定へ戻す。
予備 WAN の親にも IPv4 を設定した試験では、親が子のアドレスを名乗る ARP 要求を
物理 MAC から送り、対向の近隣情報を上書きした。両側に `arp_announce=2` も適用し、
撤去後に復元する。
この変更は compiler native-51・network journal 13 とする。開発用の旧 journal は再作成する。
`arp-vip-native51` でも新しい記録を使い、実二台の新規 ARP 応答、同一 TCP/UDP の
切替と旧親インターフェースの設定復元を確認した。
`macvlan-arp-native51b` は独立 Linux client と予備 WAN の両側で新規 ARP を行い、
二台それぞれで仮想 MAC だけの応答と共有アドレスを名乗る要求、同じ TCP/UDP の
切替、旧ノードの両設定の復元を確認した。親に IPv4 のある予備 WAN を含む通常 HA
全体も通過し、同一 TCP の最長応答待ちは約13.32秒、観測した UDP 損失は0件だった。
PVE Linux 6.8/amd64、`arp-vip-final` の実 daemon 二台と独立 Linux client では、
両ノードへの切替前後に近隣キャッシュを消して ARP を再問い合わせし、共有仮想 MAC、
同一 TCP/UDP の継続と旧親インターフェースの設定復元を確認した。
同バイナリの通常 HA 全試験は再試験で完走し、同一 TCP の最長応答待ちは約13.45秒、
観測した UDP 損失は0件だった。これは一試行の値であり、無損失の保証ではない。

別の連続障害試験は変更後も TCP の15秒タイムアウトで失敗した。再送時には切替先に
conntrack と経路が存在し、クライアントは仮想 MAC を使用していたが、主 WAN の準備中に
予備 WAN が選択されていた。`97d761a` の再試験では、主 WAN の NAT 元アドレスを
保持したまま既存 TCP の再送が予備 WAN に3回出て応答がなく、主 WAN 復旧時には
client が15秒でタイムアウトしていた。旧 routerd の conntrack mark を参照し、IPv4
source NAT の `connection_mark` と対応する policy rule を使って、新規接続の WAN と
既存接続の WAN を分ける変更を実装中。`nat-affinity-probe` の実二台試験では、主 WAN と
予備 WAN の新規接続に各 mark が付き、既存接続の mark も切替先へ渡った。通常 HA 全体は
通過した。別の連続障害では予備 WAN への誤送信が消えたが、TCP は15秒でタイムアウトした。
この変更は compiler native-52・network journal 14 とする。旧開発用記録は再作成する。
`nat-affinity-native52` の独立 Linux AFTR・予備 WAN 試験では、主 WAN 障害中に既存 TCP が
予備 WAN に出ず、新規 TCP/UDP は予備 WAN の mark で通信し、主 WAN 復旧後に元の TCP が
回復した。通常 HA 全体も実 conntrack の mark、両方向切替と packet capture を含めて
通過し、同一 TCP の最長応答待ちは約6.82秒、観測した UDP 損失は0件だった。
連続障害での同 TCP は、予備 WAN へ誤送信されなかったものの15秒で失敗した。
診断用に既存 TCP を主 WAN に固定すると予備 WAN への誤送信は止まったが、連続する
三回の所有切替では同じ15秒期限に失敗した。WAN の link-local と B4 の DAD が直列に
進むことを確認したが、PD を先に公開する試作はインターフェース停止時の RA 撤去を
壊したため削除した。DAD を省かず、所有権と撤去順序を保つ設計が必要。
通常二台試験の別試行では、1秒広告でも A の VRRP 更新が2秒の応答期限を超えて
worker が再起動し、B でも古いネットワーク適用期限の失効を観測した。計測付きの
再試験では同じ再起動は再現せず、原因は未確定。応答期限を延ばしていない。
物理 ONU・対象機器での受入は未実施。

## 作業中: 旧 routerd の PD / DS-Lite を反映 / native-49

旧 routerd の DHCPv6 client は明示 DUID を優先し、それ以外は interface の MAC から
DUID-LL を生成する。保存リースは DUID・IAID・interface・期限が一致する場合に復元し、
通常停止では自動 Release しない。NTT 回線の旧調査でも DUID-LL の利用と識別子の維持が必要だった。
routerd2 は自動 DUID をリースと別に保持する。通常停止・切替ではローカルのアドレスと RA を撤去し、
上流への Release は送らず、元の期限を持つリースを再起動用に残すよう変更した。
PVE Linux 6.8/amd64 の実 HGW 30秒試験では、SIGKILL と通常停止後に同じ DUID・IAID、
通常停止後に同じ prefix と LAN IPv4/IPv6 HTTPS・DNS の復旧を確認した。寿命0 RA と kernel 撤去を
確認し、停止時の Release は観測されなかった。自然 Renew、ONU 直結と物理機器の受入は未検証。
明示 Release 操作は未実装。
独立 dnsmasq の IA_NA 試験でも通常停止後のローカル撤去、同じ DUID とアドレスでの再起動、
TCP/UDP の復旧を確認した。自然更新、失効と再取得、NotOnLink と DAD failure も同試験で通った。
独立 PPPoE・ISC DHCP による PAP / CHAP でも PD 更新、期限切れ、同じ識別子での復旧、
PPP 交換と rollback を確認し、RA 撤去が PPP 切断に先行し Release を送らないことを確認した。
QEMU appliance の同条件での停止・再起動は未再検証。

旧 routerd の専用 DS-Lite 送信元 `/128` は他の IPv6 通信に自動選択されないよう非優先化される。
routerd2 も同様にし、元の PD の優先期限と有効期限、アドレス所有、DAD 完了を確認してから使う。
旧設定の PD Bound と専用アドレス Applied の依存を取り込み、role 条件とともに適用する。
旧実装の待機系での事前アドレス準備は採用せず、昇格後に期限付き B4 を準備する。PVE 上の実プロセスと独立 ISC DHCP・Keepalived・
Linux AFTR では、実 kernel の deprecated 状態、切替・期限切れ後の復旧、IPv4 TCP/UDP を確認した。
capture では双方の IPv4-in-IPv6 を確認した。複数障害の重なりによる既存 TCP の失敗は未解決。
変更後の二台試験では、PD と主 WAN の Health を必須とした VIP 公開条件のため、PD 失効時に
予備 WAN が使えても VIP が撤去された。主 WAN または予備 WAN のどちらかが使える条件へ試験設定を
修正し、PD 失効後の予備 WAN TCP/UDP と再取得後の DS-Lite 復帰を確認した。旧 homert の PD 必須条件も
同じ意図の確認が必要。
予備 WAN の通信を止めた再試験では、最初の失敗で旧 HealthCheck 状態機械が返す `Failing` を、
routerd2 が無条件に利用可能と判定して VIP を誤公開した。十分な連続成功で Healthy になった後の
一時的な失敗だけを猶予するよう変更した。修正後、両 WAN の相手が停止した起動時は VIP を公開せず、
相手の復旧後は PD と IPv4 TCP/UDP が戻った。二台の既存 TCP・UDP は双方向切替と worker/engine
再起動後も継続し、この試行の TCP 最長応答待ちは約13.3秒、UDP 損失は0件だった。
変更後の `make check`、独立 Health→PBR と複数 WAN 試験、amd64/arm64 の静的ビルド、
実行ファイルの署名・publisher・配置検証も通った。

旧 dnsmasq のリース同期は別の定期処理で、同期先の停止は配布応答を止めない。作業中の native-50 は
ローカルに保存したリースを非同期で同期し、同期状態とエラーを表示する。相手不在の起動・配布と、
同期断中の更新・worker再起動・第三端末への配布・同期復旧を実 BusyBox で確認した。未同期の貸出を
持つ端末が休止中に所有系を失うと別端末へ同じアドレスが配られる問題も実際に再現した。
所有者の永続記録と相手への確認を導入し、未同期の待機系が昇格した場合は新規貸出を保留する。
実二台試験で保留中の既知リース更新、worker再起動後の保留維持、同期復旧後の照合と別アドレス配布、
休止端末の元アドレスでの更新を確認した。初期化時は一台だけに `ha.bootstrap` を指定する。
保持期限の修正を含む最終版でも、所有側 PID 1 の強制終了と同じ復旧手順を通した。通常二台運用の
別試験では同期断・復旧、60秒失効、PD 失効時の予備 WAN と DS-Lite 復帰を確認した。
`make check`、Race Detector・vet、amd64/arm64 静的ビルドと署名・配置検証も成功した。
永続状態の喪失、bootstrapの誤設定、広い分断、物理機器での受入は未検証。

## 現在の変更: HA・PD・DS-Lite / native-48 / network journal12

旧 routerd の DHCPv6 client と DUID・lease 保存を参照した。自動 DUID は lease と別に永続化し、
明示 DUID は自動 identity を上書きしない。HA では設定した同じ DUID/IAID と元の grant 期限を共有する。
PD の delegation 先を client 起動の依存から外し、待機系が未作成の WAN identity を待たずに同期できるようにした。
DHCPv6 server は受信待ち中に届いた最新の入力を処理してから、応答の authority と option を判定する。

PVE Linux 6.8/amd64、`ha-egress` の署名済み実 daemon 二台、独立 ISC DHCP、Linux AFTR、
IPv6-only の主 WAN、独立 IPv4 予備 WAN と LAN client で検証した。PD 由来 B4、Health→PBR の
障害・復旧、同じ DUID/IAID・prefix と期限を保つ双方向切替、conntrackd 強制終了後の復旧、
同期断、古い通知、PD 失効と新規 TCP/UDP の予備 WAN・主 WAN 復旧が通った。capture は既存 TCP/UDP の
双方向 IPv4-in-IPv6、直結 IPv4 経路への迂回なし、TCP SYN 一回と同期 packet を確認した。
既存 TCP の最長応答待ちは約13秒、UDP 損失はこの試行で0件だった。広告間隔は homert の既定1秒で、
主 WAN の Health 成立を VIP 公開条件とした。既存接続を維持できない別の WAN へ切り替えた後は新規通信を確認した。
20cs の統合構成では worker の再起動と復旧の不安定さを観測しており、原因と切替遅延の改善は残る。
1秒広告で control 停止からの復帰中に次の切替を重ねた試行では、既存 TCP が15秒で応答せず失敗した。
各切替で同じ TCP/UDP の復旧を確認してから次を起こした再試行は完走し、最長応答待ちは約8.4秒だった。
その再試行では個別切替の VIP 公開まで約6秒、TCP 応答まで公開後約1秒以内だった。
別途観測した昇格では、選出から PD 再開まで約1.4秒、続く B4 の DAD と AFTR 準備に約2.4秒、
Health と VIP 公開に約1.1秒を要した。DAD の待機を省かず、連続障害の失敗と改善余地を追う。
初期試行で対向 AFTR の link down が戻り経路を削除し、IPv4 の直結迂回が生じたため、その試行は合格から除外した。

`make check`、既存の Health→PBR、直接 WAN と PD の HA 試験、独立 DHCPv6 information client、
amd64/arm64 の CGO_ENABLED=0 build、六 ELF の署名 catalog/publisher/stage Race 検査が成功した。
同版の実 HGW 30秒試験は DUID/IAID を保つ worker 復旧、IPv4/IPv6 HTTPS、学習 DNS、RA/Release 撤去が成功し、
自然 Renew は観測していない。ONU 直結、物理 NIC、ARM64 実行、homert 全体の受入は未検証。

## 直前の確認: source-hash / native-47 / network journal12

`forwarding.ipv4_ecmp_hash: source` を既存の writer/setting owner へ接続した。
namespace 全体の IPv4 ECMP に Linux custom source-only hash を使い、別の nft/connmark 経路は追加しない。
元 policy/mask を記録し、有効化と解除の順序、writer 交換・rollback を既存 journal で処理する。
独立二 WAN・三宛先の実 TCP/UDP で、送信元ごとの経路固定、両 WAN 利用、障害/復旧、
kernel 元値の解除と rollback 再適用、writer 交換・元期限撤去が成功した。
初回は不要な反復通信が capture の4096 frame上限に達したため、上限を維持して必要な通信へ絞った。
`make check` の Go/Python/Race/vet、両 architecture の CGO_ENABLED=0 build と最終六 ELF の
署名 catalog/publisher/stage Race 検査も成功した。旧 journal11 の開発 fixture は12へ更新し、互換 reader は作らない。
新形式でも既存 Health→PBR の切替・古い通知・元期限・rollback 試験が成功した。
実 HGW の30秒試験も PD/両 family HTTPS/DNS、DUID/IAID を保つ crash/Rebind、RA/Release 撤去を通し、
host 管理 address/MAC/default route は不変だった。native-47 の自然 Renew は未到達。

## 直前の確認: egress-route / native-46

旧 routerd の advisory 選択と実 `policyroute` 適用、既存の native ECMP を参照した。
`egress_route` は条件が真の最高 preference 候補を選び、同順位を一つの kernel route にまとめる。
相対重みは既存 next hop の `weight`、条件と元期限は既存 fact/hold と writer を使う。
別の worker・選択状態・互換経路は追加しない。入力と境界は [ResourceProgram](resource-program.md) を読む。

PVE Linux6.8/amd64、`egress-route` の実 supervisor/control/network/Health、独立二 WAN・LAN と capture で、
kernel の重み1:2、新規 TCP/UDP の両 WAN 分散、障害除外・復旧、上位 preference の選択と
confirmed rollback が成功した。control/Health を止め、network writer を交換しても元期限で撤去し、
同じ producer PID/世代の復帰後に通信が戻った。candidate の `for: 300ms` も実経路で使っている。
初回 rollback 判定は試験が古い応答構造体へ decode して confirmation を残したため timeout した。
応答ごとに初期化する既存の方法へ直し、経路・設定の復旧まで通した。製品期限は延長していない。
`make check-health-pbr` の既存切替/古い通知/元期限/rollback と新しい分散試験、`make check` の
Go/Python/Race/vet、両 architecture の CGO_ENABLED=0 build、最終六 ELF の署名
catalog/publisher/stage Race 検査も成功した。network journal は既存11、worker ABI は5。
IPv6 動的選択、policy ごとの mark/hash、既存接続継続、旧 EgressRoutePolicy 全体と homert 全体の受入は残る。

旧入力の PD/Information は、同じ master 条件と自分の PD の Bound 依存を一 client/socket へ接続する。
四つの Information 要求も既存 ORO を使う。DUID-LL・IAID・永続 identity・期限の owner は変えない。
独立した別条件は消して適用しない。conditional server と残る準備条件は [import contract](../internal/legacyconfig/README.md) を参照。
ARM64 実行、ONU 直結と対象 NIC の資格認定は未実施であり、本番 homert は置き換えていない。

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

## 実上流: ens18 の実 HGW

`get-hgw-pd` の読取専用確認と homert の AFTR intent を使った。ens18 は管理経路でもあるため、
別 MAC・隔離 router/LAN namespace で試験する。host address/route/sysctl/clock は変更しない。
DHCPv6 Reply の送信元は49153。有効な DHCPv4 貸出も受けるが server identifier は HGW の IPv4 と異なる。

`3affd07` / `health-import` native-43 の7800秒試験が完走した。758回の LAN IPv4/IPv6 HTTPS と
学習 DNS UDP/TCP、再起動前の自然 Renew/対応 Reply 一組、最後の PD worker SIGKILL 後の
同じ DUID/IAID・Rebind/Reply・通信復旧、zero-lifetime RA・kernel 撤去・Release/Reply が成功した。
host 管理 address/MAC/default route は不変。private stage・capture は終了時に削除済みで、生存 owner は無い。
過去の長時間試験で起きた DNS 応答判定失敗の原因は未特定であり、今回の成功を修正済みとは扱わない。

最新 native-46 / `egress-route` でも30秒の `make check-uplink` が成功した。両 family HTTPS、
DNS UDP/TCP、DUID/IAID を保つ SIGKILL/Rebind 復旧、RA/Release 撤去と host 管理状態不変を確認した。
この短期試験では自然 Renew は未到達。7800秒の結果を最新実行版の結果に置き換えない。
再実行: `make check-uplink UPLINK_INTERFACE=ens18 UPLINK_AFTR=<確認したAFTR>
UPLINK_SECONDS=7800 RELEASE_ID=<実行版>`。
ONU 直結・物理 NIC・継続 TCP の資格認定は別である。ens20 の以前の無応答を上流全体へ外挿しない。

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

## 次の作業

1. homert02/03の現行設定・旧routerdを比較し、残る一台分の通信と二台の切替/復旧を完成させる。
   統合構成の20cs広告時の不安定さと既存 TCP の最大約13秒の応答待ちを調査する。
   Tailscaleの残項目、旧stateless DHCPv6入力の条件付きHA/import不足、VRRP/PDの分断・復旧、
   管理・更新・戻し方を実際の相手で確認する。本番切替は行わない。
2. 実HGWの完走結果と最新版の短期結果を区別し、変更に必要な実回線検証を続ける。
   ONU/HGW側のRA/DHCPv4/DHCPv6 PDからLAN・DNS・DS-Liteまで、実相手と対象版を区別して進める。
   仮想peerとの成功を、この回線や物理NICの合格に置き換えない。
3. その後k8s-rt/pve-rtの実設定を確認し、Cloud SAMへ進む。
   SAMは旧docsの再設計目標を読み、認証/取消、構成配布、/32所有とNAT無し転送、移動・復旧を完成させる。
   新しい有料環境やproduction操作の許可を推定しない。
