# routerd2 開発引き継ぎ

更新: 2026-11-03 01:47 UTC。全体ゴールは継続中。

## 現在

25-resource native profile、main由来build `28cb655c1ced`でDNS拡張を含む実HGW統合gateが終了0。独立dnsmasq 2 processの片系停止で残るupstreamへ切替、両系停止時の両IP cache保持・未取得名のSERVFAIL隔離、復旧を確認。子suffixと複数suffixのsource選択、WAN PD学習DNSによるpublic名のA応答とquery historyのsource、Cloudflare/Google両DoHの実probe成功も確認した。全daemon再起動後も同じDNS source群、BGP両kernel経路とTCP/UDP、Tailscale subnet/exitとidentity、管理・履歴・WAN学習NTP stratum 3、両IP Internetが復旧した。PD worker SIGKILL/通常再起動後2.94/5.18秒で端末通信が復旧。full restartから全確認完了43.90秒はDoH probeの成功待ちとNTP測定を含み、純粋な転送復旧時間ではない。2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。stage/key/CA mount/peer DB/capture/namespaceは撤去済み。

- 製品Goコードは`28cb655`から不変。同版の`make check`/Race/vetを引き継ぎ、今回のPython checks/static build/設定検証を通過。native DNS設定/runnerをこのcommitへ含める。
- 旧routerdのBGP広告callerとTailscaleのrender/chainを参照した。専用CA必須という制限を除き、OS標準CAを専用rootへ渡す。upstream既定のsubnet SNATを維持する。
- homert02/03の実設定は`chatty.lain.local:~/homert02/hosts/{homert02,homert03}/router.yaml`に各110 resources。秘密は転記しない。native単体例25 resources、各homert例14 resourcesで、全設定受入ではない。
- 自然PD T1、WAN遮断、Health→PBR、appliance/updateの別gateと未受入範囲は[試験計画](test-plan.md)に簡潔に残した。raw log/pcapは保持しない。

## 次の作業

1. [実装計画](implementation-plan.md)・[JSON](implementation-plan.json)を使い、複数DNS suffix/upstream/learnedDNS、残るDS-Lite/backup WAN、BGP2 peers、LAN/運用機能をnative単体profileへ接続する。converterは作らない。
2. 実通信の直前に[試験計画](test-plan.md)全体を読む。現行のMakefileコマンドは`UPLINK_TAILNET=1`も必要。全設定のcold start、自然更新、継続通信、worker/daemon復旧を確認し、その後に二台HA・残るwork packageを進める。
3. 今回はPVE/QEMUと追加のPBR調査を実行しない。対象ONU/NIC、全homert、HA、public Tailscale control、cloud等は未受入。独立作業を一つの観測待ちで止めない。

全体開始から9日14時間10分、記録済み期限から4日8時間42分経過。全34 work packageは未完了。根拠のない完成日は出さず、関連チェックと実通信を通した変更をcommit/pushする。不要なstage/key/capture/build outputを各単位で撤去する。
