# routerd2 開発引き継ぎ

更新: 2026-11-03 02:04 UTC。全体ゴールは継続中。

## 現在

28-resource native profile、main由来build `ce1589c1bcab`で4 DS-Liteの実HGW統合gateが終了0。独立した4つのWAN /128 sourceと3 AFTRへの4 tunnelを設定し、各deviceへsocketをbindしたTCP/HTTPS・UDP/DNSが初回と全daemon restart後にすべて往復した。各委譲source宛てのIPv4-in-IPv6 returnを既存captureで確認。LANの既定経路通信、DNS複数上流/learnedDNS/両DoH、BGP、Tailscale subnet/exit、管理・履歴、WAN学習NTP stratum 3も通過した。PD worker SIGKILL/通常再起動後のLAN復旧2.92/4.88秒、full restartから全確認完了44.21秒（DoH probe待ち・NTP測定込み）。2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。hostのens18/ens20 addressとdefault routeの一致を確認し、stage/key/CA mount/peer DB/capture/namespaceを撤去した。

- 製品Goコードは`28cb655`から不変。同版の`make check`/Race/vetを引き継ぎ、今回のPython checks/static build/設定検証を通過。4 DS-Lite設定とDNS peerの実応答による起動確認をこのcommitへ含める。
- backupをDHCPとした誤った追加は削除済み。実homertは静的IPv4。ens20はDHCP無応答で、このhost用の未使用static address/gateway・上流が最小不足setup。4 DS-Liteの確認は独立して完了した。
- 旧routerdのBGP広告callerとTailscaleのrender/chainを参照した。専用CA必須という制限を除き、OS標準CAを専用rootへ渡す。upstream既定のsubnet SNATを維持する。
- homert02/03の実設定は`chatty.lain.local:~/homert02/hosts/{homert02,homert03}/router.yaml`に各110 resources。秘密は転記しない。native単体例28 resources、各homert例14 resourcesで、全設定受入ではない。
- 自然PD T1、WAN遮断、Health→PBR、appliance/updateの別gateと未受入範囲は[試験計画](test-plan.md)に簡潔に残した。raw log/pcapは保持しない。

## 次の作業

1. [実装計画](implementation-plan.md)・[JSON](implementation-plan.json)を使い、複数DNS suffix/upstream/learnedDNS、4 DS-Liteの選択policy/backup WANの静的接続、BGP2 peers、LAN/運用機能をnative単体profileへ接続する。converterは作らない。
2. 実通信の直前に[試験計画](test-plan.md)全体を読む。現行のMakefileコマンドは`UPLINK_TAILNET=1`も必要。全設定のcold start、自然更新、継続通信、worker/daemon復旧を確認し、その後に二台HA・残るwork packageを進める。
3. 今回はPVE/QEMUと追加のPBR調査を実行しない。対象ONU/NIC、全homert、HA、public Tailscale control、cloud等は未受入。独立作業を一つの観測待ちで止めない。

全体開始から9日14時間35分、記録済み期限から4日9時間07分経過。全34 work packageは未完了。根拠のない完成日は出さず、関連チェックと実通信を通した変更をcommit/pushする。不要なstage/key/capture/build outputを各単位で撤去する。
