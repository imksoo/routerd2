# routerd2 開発引き継ぎ

更新: 2026-11-03 02:49 UTC。全体ゴールは継続中。

## 現在

34-resource native profile、基準main `5da8661` + 上流選択設定と同一LAN forwardの制限撤去、build ID `5da86617fc36`で実HGW統合gateが終了0。Health 5本と既存EgressPolicyを接続し、DS-Lite群120→4本目80→LAN上の静的gateway60→群120の順に両LAN端末の新規UDP/DNS・TCP/HTTPSが往復した。実conntrack markは通常274/273、次候補275、静的276、復帰274/273。kernelの経路とgatewayも一致し、実設定の60秒hysteresisを保持した。全daemon restart後にも通常群と通信が復旧（全確認72.29秒、切戻し抑制・DoH/NTP確認込み）。4 DS-Lite個別疎通、DNS/両DoH、管理、独立BGP 2 peersの各停止/再加入、NTP stratum3、Tailscale subnet/exitも通過。PD worker SIGKILL/通常再起動後のLAN復旧1.98/4.93秒、2通信round、zero-lifetime RA/kernel撤去、上流Releaseなし。自然T1と既存socketの無損失継続は今回未確認。

- 同一LAN gatewayへの転送を拒む制限を製品Goから撤去。今回の`make check`/Race/vet、Python checks/static build/設定検証を通過。Healthと上流選択をnative設定へ接続し、独立LAN gatewayとの実通信手順を含める。
- backupをDHCPとした誤った追加は削除済み。実homertは静的IPv4。実設定の低優先候補は既存LAN上の静的gateway。ens20追加WANと新規static IPを前提にした依頼は撤回済み。4 DS-Liteの個別疎通に続き、既存優先順位の選択と両LAN端末の通信も受入済み。
- 旧routerdのBGP広告callerとTailscaleのrender/chainを参照した。専用CA必須という制限を除き、OS標準CAを専用rootへ渡す。upstream既定のsubnet SNATを維持する。
- homert02/03の実設定は`chatty.lain.local:~/homert02/hosts/{homert02,homert03}/router.yaml`に各110 resources。秘密は転記しない。native単体例34 resources、各homert例14 resourcesで、全設定受入ではない。
- 自然PD T1、WAN遮断、Health→PBR、appliance/updateの別gateと未受入範囲は[試験計画](test-plan.md)に簡潔に残した。raw log/pcapは保持しない。

## 次の作業

1. homertの上流優先順位を既存EgressPolicyへ接続し実通信を受入済み。PD由来DS-Lite群120→別DS-Lite80→既存LAN static gateway60。PPPoEを勝手に新設せず、既存終端と候補に合わせる。ens20や追加IPの回答を待つ必要はない。次は残る運用機能（syslog/OTLP、保持、DPI/flow/firewall history）とLAN scope/identityを単体profileへ接続する。[実装計画](implementation-plan.md)・[JSON](implementation-plan.json)を使い、複数DNS suffix/upstream/learnedDNS、4 DS-Liteの選択policy/backup WANの静的接続、BGPの残る運用契約、LAN/運用機能をnative単体profileへ接続する。converterは作らない。
2. 実通信の直前に[試験計画](test-plan.md)全体を読む。現行のMakefileコマンドは`UPLINK_TAILNET=1`も必要。全設定のcold start、自然更新、継続通信、worker/daemon復旧を確認し、その後に二台HA・残るwork packageを進める。
3. 今回はPVE/QEMUと追加のPBR調査を実行しない。対象ONU/NIC、全homert、HA、public Tailscale control、cloud等は未受入。独立作業を一つの観測待ちで止めない。

全体開始から9日15時間21分、記録済み期限から4日9時間53分経過。全34 work packageは未完了。根拠のない完成日は出さず、関連チェックと実通信を通した変更をcommit/pushする。不要なstage/key/capture/build outputを各単位で撤去する。
