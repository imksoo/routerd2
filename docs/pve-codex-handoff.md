# routerd2 開発引き継ぎ

更新: 2026-11-03 01:39 UTC。全体ゴールは継続中。

## 現在

25-resource native profile、基準main `5b46865` + OS trust-store修正、build ID `5b4686577c4a`で実HGW/独立FRR・Headscale・Tailscale・Linux clientの統合gateが終了0。CA指定を省略してHTTPS登録でき、LAN→tailnet peerのIPv4/IPv6 TCP/UDP、peer→広告LANのIPv4 TCP/UDP、product exit node経由のIPv4/IPv6 HTTPS・UDP DNSが通った。全daemon再起動後もnode identityを保持して同じ通信が復旧した。BGPの相互広告・両kernelの経路・LAN 2端末のNATなしTCP/UDP、DNS/cache/history・SSH/Web・WAN学習NTP源stratum 3と両IP Internetも同時に通過。PD worker SIGKILL/通常再起動後1.89/5.09秒で端末通信が戻り、full restartから全確認完了17.80秒（NTP測定を含む）。2通信round、zero-lifetime RA/kernel withdrawal、upstream Releaseなしを確認。host CA bundleのSHA-256は試験前後で一致。stage/key/CA mount/control DB/capture/namespaceは撤去済み。

- `make check`（Race Detector・vetを含む）、通常static build、native設定検証が終了0。CA修正とnative設定/runnerはこのcommitに含む。
- 旧routerdのBGP広告callerとTailscaleのrender/chainを参照した。専用CA必須という制限を除き、OS標準CAを専用rootへ渡す。upstream既定のsubnet SNATを維持する。
- homert02/03の実設定は`chatty.lain.local:~/homert02/hosts/{homert02,homert03}/router.yaml`に各110 resources。秘密は転記しない。native単体例25 resources、各homert例14 resourcesで、全設定受入ではない。
- 自然PD T1、WAN遮断、Health→PBR、appliance/updateの別gateと未受入範囲は[試験計画](test-plan.md)に簡潔に残した。raw log/pcapは保持しない。

## 次の作業

1. [実装計画](implementation-plan.md)・[JSON](implementation-plan.json)を使い、複数DNS suffix/upstream/learnedDNS、残るDS-Lite/backup WAN、BGP2 peers、LAN/運用機能をnative単体profileへ接続する。converterは作らない。
2. 実通信の直前に[試験計画](test-plan.md)全体を読む。現行のMakefileコマンドは`UPLINK_TAILNET=1`も必要。全設定のcold start、自然更新、継続通信、worker/daemon復旧を確認し、その後に二台HA・残るwork packageを進める。
3. 今回はPVE/QEMUと追加のPBR調査を実行しない。対象ONU/NIC、全homert、HA、public Tailscale control、cloud等は未受入。独立作業を一つの観測待ちで止めない。

全体開始から9日14時間10分、記録済み期限から4日8時間42分経過。全34 work packageは未完了。根拠のない完成日は出さず、関連チェックと実通信を通した変更をcommit/pushする。不要なstage/key/capture/build outputを各単位で撤去する。
