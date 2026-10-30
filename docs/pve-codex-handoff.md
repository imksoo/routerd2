# routerd2 開発引き継ぎ

更新: 2026-10-31 03:14 JST。全34 work packageのゴールは継続中。
[実装計画](implementation-plan.md)、[機械可読台帳](implementation-plan.json)、[試験計画](test-plan.md)、[AGENTS.md](../AGENTS.md)を読む。
各試験前に試験計画の実行手順と対象項目を再確認する。対象本番の置換は実施していない。
過去の成功や試作形式を設計制約にしない。ここには現在の実行条件・結果・残作業だけを残す。

## 現在のコード

main実装は`3033181` / native92 / network27。直近二台受入は通常`ntp-drift-native92`、実HGW長期試験は`bgp-gr-families-native92`。
routerd/loggingはCGO0、ユーザー承認済みのrouterd-modulesだけCGO1静的リンク。
旧routerdの実装・実際の失敗を参照して取り込み、単一network writerと明示した設定・状態の所有へ接続する。

- `8cc420d` は実PID1で必要なloopback起動を復元した。`3944a77` は配布libnetfilter_conntrackのevent timestampコピー欠落を修正した。通常Alpineでの実障害に基づく変更。
- `bc3d2a7` はAFTRInputが共有lockを保持してnetwork IPCを待つ原因を除去した。待機後は現在の設定と送信元を再確認する。旧routerdと同じく、状態通知失敗で稼働中のDHCP engineを停止しない。元2秒通知・5秒観測期限と周期は維持する。
- `9a57c8a` は全interfaceの期限更新intentをkernel更新前に一括保存する。Firewallの所有が変わらなければ同じboolの保存を省く。所有確認・atomic置換・完了保存は維持する。観測したfsync頻度は22.84/sから13.36/sへ減ったが、対象機器の性能保証ではない。

- `b6c774e` は初回起動前に旧dnsmasqの有効IPv4 leaseを明示取込する。既存HAのlease結合を共用し、元期限と割当を保持する。通常製品`lease-import-native92`と独立サーバー/端末で、再DHCP前のDNS、旧端末の割当保持、別端末への重複防止、取得DNS UDP/TCP・両端末IPv4 gatewayが9.42秒で成功。sticky履歴・二台初期移行は別の残作業。
- `551c00c` は旧routerdの情報取得呼出しを照合し、PD ReplyにDNSがあっても独立Information-requestを完了させる。部分PDで省略された情報を元期限より延命しない。独立ISCの分割応答で変更前の不送信を再現し、専用通常版でLAN実DNS/両IP TCPUDP・更新・再起動・元期限失効まで67.22秒で成功。組合せ通常版`combined-information-native92`も分割ISC実通信と関連Race/vet・両arch静的六ELF/署名検査が成功した。

## 実行済みと範囲

| 実行版・環境 | 結果 |
| --- | --- |
| 9a57c8a通常製品、隔離Linux、全57resource | 初回と全daemon再起動後に独立端末の取得DNS UDP/TCP、両IP TCP/UDP、条件付きDNS/DoH、BGP/IX、NTP、Tailscale、保持identityが成功。 |
| 9a57c8a相当の専用計測版、同構成600秒/20巡 | Apply最大0.828秒、transport最大1.795秒、effects timeoutなし。短時間診断であり長期安定の受入ではない。先行アドレス一括化だけの版はApply3.386秒で失敗した。 |
| アドレス一括化までの通常Alpine3.24.3/Linux6.18.59、4CPU/4GiB KVM | 全57resourceの初回・実guest再起動後に同じクライアント経路が成功。kernel起動2回、reboot marker1回。最終Firewall保存削減入りの二台試験は次。 |
| bc3d2a7確定三ELF、独立WSL | network/controlを別々に3.25秒停止中もDHCP/DNS PIDを保持。BusyBoxが既存leaseを実再取得し、取得DNS UDP/TCPが停止中・復帰後とも成功。修正前は同条件でDHCPが終了した。恒久回帰はmake check-dhcp-dnsの既存suiteへ統合済み。 |
| 8cc420d通常Alpine二台、各4CPU/4GiB KVM | 両方向の実電源断と再参加を23.621/21.087秒で通過。同TCP/NAT保持。最大TCP待ち14.957秒、UDP4228件中289期限切れ、DNS2528件中16失敗。双方BGP確立が前提。復帰途上のbackupへの追加障害は元60秒内に復旧せず未受入。 |
| 8cc420d→00969ca通常Alpine、全57resource | 504MiB署名catalogを実SSH転送し、更新後36.536秒、明示rollback後35.723秒で全経路復帰。別試行ではHealth対向停止により元90秒trialが失敗し自動復帰、全経路再確認まで136.339秒。設定/DUID/NodeID/SSH身元を保持。最新9aの版間試験ではない。 |
| 3944a77通常製品、実HGW、標準7800秒 | 両IP HTTPS/取得DNS683巡、自然PD Renew/Reply1組24.714ms。WAN断復帰、PD交換、全daemon交換、Tailscale出口が成功。一方DHCP通知timeoutによる再起動2回を観測。bc修正後のHGW長時間試験を実施する。 |
| 独立a0b091b通常製品 | 公式Tailscale経由の管理SSHがcontrol3.035秒停止中も同じ接続を保持し、製品再起動後に同じhost keyで復帰。NTP全上流30.041秒遮断後、57.905秒で新しい上流sample・独立LAN採用が復帰。f470989/7fc1e1dで再利用可能手順を統合。 |

9a57c8aではnetwork Race32.815秒、全vet、実隔離kernelのboot lifetime/forwarding/NAT/DS-LiteのRace、amd64/arm64静的六ELF、署名catalog/publisherが成功した。
計測用変更は通常製品へ入れない。変更した本番経路の所有・保存・失敗処理を確認し、期限延長で成功に変えない。

二台のBGP広告は共有WAN IPv4をnext-hopに明示指定し、その共有アドレスを既存WAN VMACに配置した。
`43a6487`の`bgp_next_hops`方針で通常CLI変換し、両方110→57resourceが実行設定と一致した。
個別WAN next-hopのECMP応答が待機系へ入る問題と、共有アドレスの二重ARP応答を取り除いた変更であり、元設定の無変更合格ではない。

## 直近の二台・運用受入

`9436850` はBGPの確定したアドレス族を一度だけ組み立て、各族のGR capabilityを保持する。実OPENはIPv4 AFI1/SAFI1、転送状態保持flagなし。旧版では空のGR capabilityのため独立FRRが古い接続を維持し、新規接続をCease/Connection Collision Resolutionで拒否していた。復帰途中の予備系への追加断で、新規DNS/両IP TCPUDPは61.200秒から7.914/9.568秒に改善した。ただしこの走行はNTPの再採用待ち58.427秒を含めた全体60秒を超えた。

`3033181` はchronydの補正周波数を最短1分ごとに保存し、起動直後の電源断でも学習値を使えるようにした。読み込まない測定履歴のdumpdirは撤去した。問い合わせ周期、時刻品質の判定、元60秒/端末15秒は変えていない。通常`ntp-drift-native92`はcd62d89と同じ本体にこのNTP変更を加えたimageである。後述する新しいDNS/OTLP変更を含む全体受入には読み替えない。

同じ全57resource・独立peer群・4CPU/4GiB Alpine二台で、両方向の実電源断と再参加が終了0。二度目は復帰途中のbackupがBGP未確立のまま現用を停止し、BGP/NTP準備を待っていない。実再起動後に`Frequency 6.423 +/- 377.327 ppm read from /state/drift`を確認し、独立LAN端末のNTP採用は4.283秒。全経路再確認22.428/19.421秒で元60秒内。同じTCP/SYN/NATを保持したが最大TCP待ちは14.506秒（判定15秒）、UDP1515送信/1263受信/252期限切れ、最大無応答7.249秒、DNS831件中20失敗。各guest起動2回、effects timeoutなし。物理機器・性能・全34項目の完成ではない。

`make check`（Race/vet含む）、BGP関連Race6.503秒、両arch静的六ELF・署名catalog/publisherが9436850で成功。NTP/runtimeplanのRace37.909秒、全vet、通常ビルドと実上記通信も成功した。統合通常版`integrated-ntp-native92`の両arch静的六ELF・署名catalog/publisherも成功した。

運用の追加経路も実通信済み。a2d959fの旧sticky履歴取込はroot通常版で10.87秒、元日時・期限、別MACへの保持IP配布防止、元MAC再取得と取得DNS UDP/TCP/IPv4を確認した。b2d8b59の初期二台lease取込は独立29.144秒で既存端末の期限/更新/DNSが成立したが、旧owner停止中の新規端末は`allocation_blocked`で配布されず、復帰後だけ成功した。これは次の製品修正であり、正常運用の必要条件にしない。

28aaf16のDNS監視3秒/通常問い合わせ1秒の分離は独立dnsmasq1.5秒遅延で確認し、root通常`integrated-ntp-native92`でも通過。1d96d87の実設定適用計測→既存ログ→OTLPは、同じroot通常版と独立Collectorで障害・復帰・重複なしを確認し、並行UDP1246件/失敗0。これらの通常版は上記ntp-drift imageとは別の実行物である。

条件付きLANのVIP復旧には先行走行で間欠失敗があり、実kernelアドレスも待つ同じ元10秒の観測では組合せ版76.22秒・修正前baseline72.37秒が通過した。先行失敗時の選出/適用状態は未採取であり、原因解決とはしていない。

## 継続中の試験と保護する状態

通常Alpine `full-homert-loopback-native92` / 8cc420d の元LAN lease43200秒/T1=21600秒を継続観測中。
独立の普通・特殊dhcpcd端末、全57resource、実再起動後の通信を確認してから開始した。
自然T1は2026-10-31 04:42:16 JST、観測終了は約04:49:37 JST。
専用VM、実行stage、保存DUIDを停止・削除・別版差替えしない。最新コードの自然更新合格へ読み替えない。
私有の実行stage/手順は管理端末の `/tmp/routerd-full-homert-loopback-kvm92-natural.py` と対応logを参照する。
実HGWの保存DUIDは管理ユーザーの `.local/state/routerd2-homert-four` に保持する。

PVE試験は専用所有名を確認したVMだけを作成・停止・削除し、既存bridge/NICへ接続しない。
Ethernetは隔離hub/streamで独立peerへ接続する。管理経路・回復・空きmemoryを先に確認する。
秘密を含む実入力、SSH鍵、identity、pcap、インフラ詳細は公開しない。
完了したstage・重複ELF・生captureは診断と簡潔な記録を終えてから削除する。

## 次に実行すること

1. 選出済み生存機が旧owner停止中の新規端末へDHCP配布し、取得DNS/IPv4を使えるようにする。分断と古いowner復帰時の重複防止を実通信で確認し、全体二台の切替・移行・復旧へ統合する。
2. bgp-gr-families-native92通常製品で実HGW標準7800秒を継続し、自然PD更新、DHCP通知遅延修正と端末の継続利用を確認する。元LAN6時間待ちと並行して進める。
3. 自然LAN/HGW更新を完了し、実homertのSSH条件・移行/復旧手順、対象設定・運用・更新と物理NIC/ONUの条件を確定して受け入れる。
4. k8s/pveの認証参加から動的IPIP/BGPと/32通信までを旧実装から取り込み、実peerへ通す。続いてCloud SAMの実装・許可された環境の受入を進める。

全34項目、対象ハードウェアの資格確認・性能、ARM実行、実ONU直結、Cloud SAMは完了していない。
機能別の再利用可能な通常手順は [scripts/README.md](../scripts/README.md)、配布更新は [releases.md](releases.md) を参照する。
