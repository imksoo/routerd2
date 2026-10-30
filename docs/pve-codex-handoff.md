# routerd2 開発引き継ぎ

更新: 2026-10-31 01:55 JST。全34 work packageのゴールは継続中。
[実装計画](implementation-plan.md)、[機械可読台帳](implementation-plan.json)、[試験計画](test-plan.md)、[AGENTS.md](../AGENTS.md)を読む。
各試験前に試験計画の実行手順と対象項目を再確認する。対象本番の置換は実施していない。
過去の成功や試作形式を設計制約にしない。ここには現在の実行条件・結果・残作業だけを残す。

## 現在のコード

main実装は`551c00c` / native92 / network27。実行中の通常image/HGWは`9a57c8a` / `network-ownership-batch-native92`。
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

9a57c8aの通常Alpine二台も両方向電源断・再参加を完走した。全経路再確認26.744/21.253秒、同じTCP/NAT保持、最大TCP待ち7.398秒、独立UDP4430送信/4213受信/216期限切れ/最終標本時1未確定、最大無応答5.933秒。DNS2696件中19失敗。各guest実kernel起動2回、effects timeoutなし。

別の予備系復帰途中への追加断は、新規通信が61.200秒で戻り、元60秒を超えた。packetで独立FRRから繰り返すCease/Connection Collision Resolutionを特定した。GR capabilityと保存済みpeer復元を照合して修正する。最初の走行は対向listen前の接続拒否でfault前に停止したため、独立対向のlisten完了を確認する手順へ訂正し、切替失敗には数えていない。

組合せ版の条件付きLANでは、制御の短い停止・元authority期限失効・復帰、役割変更、PD喪失/再取得、実DNS/IPv6 TCPUDPと同じ固定DNS TCP接続が76.22秒で完走した。ソケットが残ってもVIPは撤去され得るため、元10秒内で実kernelアドレスも待つよう既存試験を訂正した。先行走行のDNS timeout/10秒VIP未復帰は残る間欠失敗であり、原因解決とはしない。同じ観測方法の修正前通常9a57c8aも72.37秒で通過。次は失敗時の選出/適用状態を取り、復旧成立と間欠原因を分ける。

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

1. 実行中の9a57c8a通常Make/APK imageで二つの独立KVMを起動し、既存TCP/UDPと新規DNS/両IP通信・BGP・Tailscaleの両方向電源断復旧を元60秒/端末15秒で確認する。復帰途上のbackupへの追加障害を別途解決する。
2. 同じ9a57c8a通常製品で実HGW標準7800秒を継続し、自然PD更新、DHCP通知遅延修正と端末の継続利用を確認する。元LAN6時間待ちと並行して進める。
3. 既存LAN lease初期取込など独立の不足機能を実装し、実端末で使う。対象設定・運用・更新と物理NIC/ONUの受入を完了する。
4. k8s/pveの認証参加から動的IPIP/BGPと/32通信までを旧実装から取り込み、実peerへ通す。続いてCloud SAMの実装・許可された環境の受入を進める。

全34項目、対象ハードウェアの資格確認・性能、ARM実行、実ONU直結、Cloud SAMは完了していない。
機能別の再利用可能な通常手順は [scripts/README.md](../scripts/README.md)、配布更新は [releases.md](releases.md) を参照する。
