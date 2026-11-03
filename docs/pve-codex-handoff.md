# routerd2 開発引き継ぎ

更新: 2026-11-03 03:40 UTC。全体ゴールは継続中。

## 現在

38-resource native profile、基準main `b03debf` + LAN配布設定、build ID `b03debf15982`で実HGW統合gateが終了0。通常・予約・OUI・特定MACの独立端末5台で、通常gateway/DNS .1と特別gateway/DNS .253を実ACKから設定し、公開HTTPS/UDP DNSが往復した。通常/予約端末はNTP2アドレスとdomain-searchも取得。実Releaseで3日分の保持期限が保存され、端末IP撤去後も別MACの同アドレス要求には別leaseを配り、元MACには保持アドレスを再配布した。全daemon restart後も特別端末3台のRenew/ACK・通信とfingerprint APIが復旧した。LAN RAのhigh優先度/MTU1500/valid最大7200秒/preferred最大3600秒を実packetで確認。上流選択、4 DS-Lite、DNS/DoH、nDPI/計数/SNI、fresh syslog/OTLP、BGP/NTP/Tailscaleも通過。PD worker SIGKILL/通常再起動後の復旧2.96/5.85秒、全daemon restartから全確認102.76秒（60秒抑制と30秒sampling等を含む）。2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。自然T1・既存socket継続・3日/7日経過削除・全homert/HA/実機資格は今回の範囲外。

- 製品Goは`b1a3e48`から不変。今回Race/vet、Python checks/static build/設定検証が通過。7日/daily/vacuumをoperational event・DNS/flow/firewallへ設定した。全datasetの保持/期限削除を今回実証したことにはしない。
- backupをDHCPとした誤った追加は削除済み。実homertは静的IPv4。実設定の低優先候補は既存LAN上の静的gateway。ens20追加WANと新規static IPを前提にした依頼は撤回済み。4 DS-Liteの個別疎通に続き、既存優先順位の選択と両LAN端末の通信も受入済み。
- 旧routerdのBGP広告callerとTailscaleのrender/chainを参照した。専用CA必須という制限を除き、OS標準CAを専用rootへ渡す。upstream既定のsubnet SNATを維持する。
- homert02/03の実設定は`chatty.lain.local:~/homert02/hosts/{homert02,homert03}/router.yaml`に各110 resources。秘密は転記しない。native単体例38 resources、各homert例14 resourcesで、全設定受入ではない。
- 自然PD T1、WAN遮断、Health→PBR、appliance/updateの別gateと未受入範囲は[試験計画](test-plan.md)に簡潔に残した。raw log/pcapは保持しない。

HA基盤の最新確認: 基準main `9fb64f4`の通常実行物、build ID `9fb64f4267e9`で`make check-ha-state`が終了0。独立Linux namespace/PID1の2 product processとclient/serverで、A→B/B→AのVRRP/NAT引継ぎ、同一TCP socketとUDP mapping、同期済みDHCP leaseの実Renew/ACK、worker/conntrack engine再起動を確認した。TCP再接続なし（captureのSYNは1回）、最大応答待ち1.665秒。UDPは50ms送信/200ms返信予算で3009送信・2942受信・67期限超過、最大無応答1.110秒（このfixture条件での測定）。同期断中の独立割当/更新と復帰、未設定証明書の拒否、実lease期限での失効、3日sticky期限を変えない再起動/引継ぎ、保持中の別client割当と元client再取得も通過。設定削除で同期workerを回収し、元のconntrack sysctlへ復元した。実HGWの38-resource全体を二台化した結果や、14-resource例・実homert二台の資格とは分ける。

## 次の作業

進行中（2026-11-03T03:25:30Z）: 設定/runner `5861b7e`、通常build ID `b03debf15982`（製品Go不変）で`--seconds 7800 --full-restart --tailnet`の自然更新観測を起動した。live ownerと現在のPD bound/IAID1を実プロセス・IPCで確認済み。`/tmp/routerd2-natural-uplink.log`が現在の進捗。終了0/自然Renew/Replyはまだ未確認。重複起動やlive stageの削除をせず、実プロセスを再確認して観測を継続する。待機中は残る単体/HA作業を進める。

1. LANの通常/予約/OUI/特定MAC配布、特別gateway/DNS、3日sticky hold、NTP/domain-search、fingerprint、RA high/MTU/寿命を今回受入済み。次は同一profileの自然PD T1と通信継続を確認し、その待機中に残る単体intent/HA実装を進める。全private DNS peer、BGP運用、保持dataset等の残差は未受入のまま。
2. 実通信の直前に[試験計画](test-plan.md)全体を読む。現行のMakefileコマンドは`UPLINK_TAILNET=1`も必要。全設定のcold start、自然更新、継続通信、worker/daemon復旧を確認し、その後に二台HA・残るwork packageを進める。
3. 今回はPVE/QEMUと追加のPBR調査を実行しない。対象ONU/NIC、全homert、HA、public Tailscale control、cloud等は未受入。独立作業を一つの観測待ちで止めない。

全体開始から9日16時間11分、記録済み期限から4日10時間43分経過。全34 work packageは未完了。根拠のない完成日は出さず、関連チェックと実通信を通した変更をcommit/pushする。不要なstage/key/capture/build outputを各単位で撤去する。
