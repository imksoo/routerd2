# routerd2 開発引き継ぎ

更新: 2026-11-03 03:02 UTC。全体ゴールは継続中。

## 現在

38-resource native profile、基準main `b1a3e48` + 運用設定、build ID `b1a3e489ca77`で実HGW統合gateが終了0。独立LAN端末の公開HTTPS/DNSについて双方向計数・native nDPI由来・TLS SNIをtraffic-history API、管理WebへのLAN接続拒否をfirewall-history APIから確認した。独立syslog receiverと公式OTLP Collectorへ製品ログ/metrics/tracesが届き、全daemon restart後の新しいHealth syslog/log/metric/spanも実時刻で照合した。通常の上流選択120→80→60→120、4 DS-Lite、DNS/両DoH、管理、BGP 2 peersの各停止/再加入、NTP stratum3、Tailscale subnet/exitも通過。PD worker SIGKILL/通常再起動後のLAN復旧2.02/5.84秒、全daemon restartから全確認73.22秒、2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。7日保持は設定済みで期限削除の今回実証ではなく、自然T1・既存socket無損失継続・全homert/HA/実機資格も別に残る。

- 製品Goは`b1a3e48`から不変。今回Race/vet、Python checks/static build/設定検証が通過。7日/daily/vacuumをoperational event・DNS/flow/firewallへ設定した。全datasetの保持/期限削除を今回実証したことにはしない。
- backupをDHCPとした誤った追加は削除済み。実homertは静的IPv4。実設定の低優先候補は既存LAN上の静的gateway。ens20追加WANと新規static IPを前提にした依頼は撤回済み。4 DS-Liteの個別疎通に続き、既存優先順位の選択と両LAN端末の通信も受入済み。
- 旧routerdのBGP広告callerとTailscaleのrender/chainを参照した。専用CA必須という制限を除き、OS標準CAを専用rootへ渡す。upstream既定のsubnet SNATを維持する。
- homert02/03の実設定は`chatty.lain.local:~/homert02/hosts/{homert02,homert03}/router.yaml`に各110 resources。秘密は転記しない。native単体例38 resources、各homert例14 resourcesで、全設定受入ではない。
- 自然PD T1、WAN遮断、Health→PBR、appliance/updateの別gateと未受入範囲は[試験計画](test-plan.md)に簡潔に残した。raw log/pcapは保持しない。

## 次の作業

1. 上流選択とsyslog/OTLP・nDPI flow/firewall historyの単体接続を受入済み。次はDHCPv4の特定MAC向けscope、NTP/domain-search、3日sticky保持、fingerprint保持、RA high/MTU1500/寿命上限を旧実装と保存中homert intentに沿って接続する。DHCPv6Serverは実設定でstatelessのため、不要なstateful poolは新設しない。全private DNS peerや残るBGP契約も照合する。
2. 実通信の直前に[試験計画](test-plan.md)全体を読む。現行のMakefileコマンドは`UPLINK_TAILNET=1`も必要。全設定のcold start、自然更新、継続通信、worker/daemon復旧を確認し、その後に二台HA・残るwork packageを進める。
3. 今回はPVE/QEMUと追加のPBR調査を実行しない。対象ONU/NIC、全homert、HA、public Tailscale control、cloud等は未受入。独立作業を一つの観測待ちで止めない。

全体開始から9日15時間33分、記録済み期限から4日10時間5分経過。全34 work packageは未完了。根拠のない完成日は出さず、関連チェックと実通信を通した変更をcommit/pushする。不要なstage/key/capture/build outputを各単位で撤去する。
