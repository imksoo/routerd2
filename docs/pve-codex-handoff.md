# routerd2 開発引き継ぎ

更新: 2026-11-03 03:22 UTC。全体ゴールは継続中。

## 現在

38-resource native profile、基準main `b03debf` + LAN配布設定、build ID `b03debf15982`で実HGW統合gateが終了0。通常・予約・OUI・特定MACの独立端末5台で、通常gateway/DNS .1と特別gateway/DNS .253を実ACKから設定し、公開HTTPS/UDP DNSが往復した。通常/予約端末はNTP2アドレスとdomain-searchも取得。実Releaseで3日分の保持期限が保存され、端末IP撤去後も別MACの同アドレス要求には別leaseを配り、元MACには保持アドレスを再配布した。全daemon restart後も特別端末3台のRenew/ACK・通信とfingerprint APIが復旧した。LAN RAのhigh優先度/MTU1500/valid最大7200秒/preferred最大3600秒を実packetで確認。上流選択、4 DS-Lite、DNS/DoH、nDPI/計数/SNI、fresh syslog/OTLP、BGP/NTP/Tailscaleも通過。PD worker SIGKILL/通常再起動後の復旧2.96/5.85秒、全daemon restartから全確認102.76秒（60秒抑制と30秒sampling等を含む）。2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。自然T1・既存socket継続・3日/7日経過削除・全homert/HA/実機資格は今回の範囲外。

- 製品Goは`b1a3e48`から不変。今回Race/vet、Python checks/static build/設定検証が通過。7日/daily/vacuumをoperational event・DNS/flow/firewallへ設定した。全datasetの保持/期限削除を今回実証したことにはしない。
- backupをDHCPとした誤った追加は削除済み。実homertは静的IPv4。実設定の低優先候補は既存LAN上の静的gateway。ens20追加WANと新規static IPを前提にした依頼は撤回済み。4 DS-Liteの個別疎通に続き、既存優先順位の選択と両LAN端末の通信も受入済み。
- 旧routerdのBGP広告callerとTailscaleのrender/chainを参照した。専用CA必須という制限を除き、OS標準CAを専用rootへ渡す。upstream既定のsubnet SNATを維持する。
- homert02/03の実設定は`chatty.lain.local:~/homert02/hosts/{homert02,homert03}/router.yaml`に各110 resources。秘密は転記しない。native単体例38 resources、各homert例14 resourcesで、全設定受入ではない。
- 自然PD T1、WAN遮断、Health→PBR、appliance/updateの別gateと未受入範囲は[試験計画](test-plan.md)に簡潔に残した。raw log/pcapは保持しない。

## 次の作業

1. LANの通常/予約/OUI/特定MAC配布、特別gateway/DNS、3日sticky hold、NTP/domain-search、fingerprint、RA high/MTU/寿命を今回受入済み。次は同一profileの自然PD T1と通信継続を確認し、その待機中に残る単体intent/HA実装を進める。全private DNS peer、BGP運用、保持dataset等の残差は未受入のまま。
2. 実通信の直前に[試験計画](test-plan.md)全体を読む。現行のMakefileコマンドは`UPLINK_TAILNET=1`も必要。全設定のcold start、自然更新、継続通信、worker/daemon復旧を確認し、その後に二台HA・残るwork packageを進める。
3. 今回はPVE/QEMUと追加のPBR調査を実行しない。対象ONU/NIC、全homert、HA、public Tailscale control、cloud等は未受入。独立作業を一つの観測待ちで止めない。

全体開始から9日15時間54分、記録済み期限から4日10時間26分経過。全34 work packageは未完了。根拠のない完成日は出さず、関連チェックと実通信を通した変更をcommit/pushする。不要なstage/key/capture/build outputを各単位で撤去する。
