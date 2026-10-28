# routerd2 開発引き継ぎ

2026-10-28。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

## 現在の変更: info-owner / native-45

旧 Information controller と PD client の固定要求・Bound 連動を比較した。
`request` の四項目を既存一 client の ORO で満たす。Information が PD と同じ master 条件を持ち、
その PD の Bound に依存する構成も同じ owner へ接続する。条件は変換後の型付き値を比較し、
`all` wrapper や `.status.role` の表記差で別 client を作らない。独立した別条件は消さずに拒否する。

PVE Linux6.8/amd64、`info-owner` の製品 CLI 変換→実 worker と独立 Keepalived2.2.8・ISC4.4.3-P1 で、
PD/情報取得、準備待ち、同じ DUID/IAID での切替復旧、WAN /128・VIP の新規 TCP/UDP、
元期限での WAN/Health 停止と PD の停止処理開始、RA 撤去が成功した。
Information の別条件を PD alias へ捨てて適用しないことも局所検査で確認する。
`make check` の Go/Python/Race/vet、両 architecture の CGO_ENABLED=0 build と最終六 ELF の
署名 catalog/publisher/stage Race 検査も成功した。
直前 `info-options` の独立 ISC/radvd/BusyBox/Linux 試験では実 ORO 四項目、PD/IA_NA・LAN RA/配布、
TCP/UDP/DNS、元期限撤去・復旧、DUID rollback と署名成果物まで成功している。
条件付き server、他の依存/準備条件、homert 全体、ARM64 実行と対象 NIC は残る。

実 HGW の別 `health-import` / native-43 長時間試験では、worker 再起動前に自然 T1 を迎え、
capture に一致する Renew/Reply 一組を確認した。終了時の crash/restart・撤去は引き続き待つ。

## 直前の完了変更: pd-admission / native-45

routerd の `resourcequery/when.go`、PD client、VRRP backend・VMAC hook と現行 homert の設定を参照した。
旧 HealthCheck・静的 VirtualAddress・PD の master/backup 条件を、既存の型付き条件へ接続している。
master は VIP 公開ではなく選出を意味する。backup は有限の実 BACKUP 観測であり、master の否定ではない。
primary/追加 VMAC は通常 DAD・元の権限期限を使う。仮想 link の所有は宣言し、物理 parent の管理権限は独立に必要。

`gracefulActivation.readyWhen` は同じ条件変換を使い、PD の `phase: Bound` を有効な既存 PD fact へ接続する。
旧 Bound/Renewing/Rebinding の区分と timeout 既定値45秒を維持する。timeout は公開を強制しない。
VIP 公開後の準備状態低下は、その選出での公開を取り消さない。新しい readiness controller や状態台帳は作らない。

PVE Linux6.8/amd64、`pd-admission` で製品 CLI 変換→実 supervisor/control/network/protocol worker と
独立 Keepalived2.2.8・ISC4.4.3-P1 を使い、取得待ち中の WAN 準備と VIP 非公開、ISC 復帰後の公開が成功した。
切替・VRRP worker 交換・復旧、同じ自動 DUID/IAID での PD 再取得、PD 由来 WAN /128 と VIP の新規 TCP/UDP、
通常 DAD、RA 撤去を確認した。VRRP の元観測期限では WAN の撤去/DOWN、Health の終了、PD の停止処理開始を確認する。
PD の停止処理は既存の20秒上限で撤去 ACK・Release を処理する。期限切れで元 link-local が無ければ Release は送れない。
単独 PD の role guard は、共有 grant を Release せず待機系へ同期する native HA 設定の自動推定ではない。

初回の GR 試行は、期限内に全依存 process の消滅を待つ箇所で timeout した。その後の同条件三試行では未再現。
最終試験は転送権限の失効と PD cleanup を分け、元期限での WAN 停止と worker 停止処理を要求する。
製品期限・試験待機時間は延長していない。最終コードでもこの実通信試験は成功した。
`make check` の Go/Python/Race/vet、両 architecture の CGO_ENABLED=0 build、最終六 ELF の署名
catalog/publisher/stage Race 検査と basic IPv4 v2/PASS・IPv6 v3 は成功した。
ARM64 実行、対象 NIC、homert 全体と既存接続継続の今回の受入は未実施。

再実行: `make check-vrrp-unicast RELEASE_ID=pd-admission
DHCP6_SERVER=dist/peer-env/isc-dhcp/root/usr/sbin/dhcpd`。
契約と残制限は [legacy importer](../internal/legacyconfig/README.md) と各 protocol の README を読む。
過去の逐次試験記録はここへ積み増さない。native HA の既存接続・共有 grant 試験は
`make check-ha-pd check-ha-state`、関連設定と未完了範囲は実装計画を参照する。

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

## 実上流: ens18の実HGW接続を確認、長時間試験は実行中

既存`get-hgw-pd`の読取専用問い合わせで実HGWのPD配布を確認した。到達経路はens18であり、
以前に無応答だったens20とは別の経路だった。homert02の設定から実AFTR intentを確認して使った。
`ha-pd-admission` / native-43で初回確認後、`ha-pd-prepared`でも再検証した。`make check-uplink UPLINK_INTERFACE=ens18
UPLINK_AFTR=<確認したAFTR> UPLINK_SECONDS=30 RELEASE_ID=ha-pd-prepared`で実行した。
管理address/MAC/default routeを保持した別MAC・隔離LANで、実HGWのRA/PD、LANからDS-Lite経由の
IPv4 HTTPS、IPv6 HTTPS、学習DNSへのUDP/TCP、RA撤去とRelease/Replyが通った。Replyの送信元は49153。
終了後のhost管理状態は開始前と一致し、短期試験のnamespace・raw出力は削除済み。

file準備変更前`ha-pd-admission`での7800秒試験は、自然更新に達する前に
`example.net A`のDNS応答判定で失敗して終了した。約30分の時点まではreadyかつ全worker世代1だった。
元のassertに応答本文がなくRCODE/Answerを特定できなかったため、作業差分で失敗時に本文と
UDP/TCPの種別を出すようにした。DNSの判定は変更しない。ee50461/`ha-pd-prepared`の再試行は
約42分まで実通信を継続し、途中でPD workerをSIGKILLしてDUID保持・Rebind復旧を確認した。
captureは同じDUID/PDでのRebindと、49153からのReplyを示した。新しいReplyが期限を更新したので、
元期限との単純一致assertは失敗したが製品の誤延長ではない。その後LAN両family HTTPSとUDP/TCP DNSも通った。
自然Renewは未到達で、この試行は意図して中断・削除した。失敗や完走として扱わない。

現在は3affd07/`health-import`と更新fixtureで7800秒を再実行中。worker SIGKILLは観測期間の最後に行う。
`renew_at`変更を自然Renewに数える表示を除き、captureのRenew/対応Replyを起動復旧のRebind/Replyと
区別する。初期期限を含む長さの試験では、再起動前の自然Renew/Replyが無ければ失敗する。
30秒の実HGW試験はDUID/IAID保持、Rebind/Reply、LAN両family HTTPS・UDP/TCP DNS、
RA/Release撤去まで成功した。`make test-tools`も成功。長時間試験の完走は未確認。自然 Renew/対応 Reply は再起動前の実 capture で一組確認した。
失敗版のnamespace・stageは削除され、host管理address/MAC/default routeは開始前と一致した。
現在のownerがbounded captureとprivate stateを所有し、終了時に削除する。
前の長時間試験側の独立LANではpublic DNSへのIPv4 UDP/TCP応答とAFTR内packetも確認した。
DHCPv4の有効貸出が隔離WANへ入ることも確認したが、server identifierはHGWのIPv4とは異なった。
ONU直結・物理NIC・継続TCPの資格認定とは区別する。
ens20の無応答を上流全体の利用不能へ外挿せず、未回答のens20質問を実HGW試験の前提にしない。

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
   Tailscaleの残項目、旧stateless DHCPv6入力の条件付きHA/import不足、VRRP/PDの分断・復旧、
   管理・更新・戻し方を実際の相手で確認する。本番切替は行わない。
2. 実行中の実HGW自然更新試験を完了まで確認し、最後のPD worker crash/restartとDUID保持も確認する。
   ONU/HGW側のRA/DHCPv4/DHCPv6 PDからLAN・DNS・DS-Liteまで、実相手と対象版を区別して進める。
   仮想peerとの成功を、この回線や物理NICの合格に置き換えない。
3. その後k8s-rt/pve-rtの実設定を確認し、Cloud SAMへ進む。
   SAMは旧docsの再設計目標を読み、認証/取消、構成配布、/32所有とNAT無し転送、移動・復旧を完成させる。
   新しい有料環境やproduction操作の許可を推定しない。
