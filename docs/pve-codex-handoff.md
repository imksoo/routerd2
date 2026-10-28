# routerd2 開発引き継ぎ

2026-10-28。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

## 現在の変更: egress-route / native-46

旧 routerd の advisory 選択と実 `policyroute` 適用、既存の native ECMP を参照した。
`egress_route` は条件が真の最高 preference 候補を選び、同順位を一つの kernel route にまとめる。
相対重みは既存 next hop の `weight`、条件と元期限は既存 fact/hold と writer を使う。
別の worker・選択状態・互換経路は追加しない。入力と境界は [ResourceProgram](resource-program.md) を読む。

PVE Linux6.8/amd64、`egress-route` の実 supervisor/control/network/Health、独立二 WAN・LAN と capture で、
kernel の重み1:2、新規 TCP/UDP の両 WAN 分散、障害除外・復旧、上位 preference の選択と
confirmed rollback が成功した。control/Health を止め、network writer を交換しても元期限で撤去し、
同じ producer PID/世代の復帰後に通信が戻った。candidate の `for: 300ms` も実経路で使っている。
初回 rollback 判定は試験が古い応答構造体へ decode して confirmation を残したため timeout した。
応答ごとに初期化する既存の方法へ直し、経路・設定の復旧まで通した。製品期限は延長していない。
`make check-health-pbr` の既存切替/古い通知/元期限/rollback と新しい分散試験、`make check` の
Go/Python/Race/vet、両 architecture の CGO_ENABLED=0 build、最終六 ELF の署名
catalog/publisher/stage Race 検査も成功した。network journal は既存11、worker ABI は5。
IPv6 動的選択、source-only hash、既存接続継続、旧 EgressRoutePolicy 全体と homert 全体の受入は残る。

旧入力の PD/Information は、同じ master 条件と自分の PD の Bound 依存を一 client/socket へ接続する。
四つの Information 要求も既存 ORO を使う。DUID-LL・IAID・永続 identity・期限の owner は変えない。
独立した別条件は消して適用しない。conditional server と残る準備条件は [import contract](../internal/legacyconfig/README.md) を参照。
ARM64 実行、ONU 直結と対象 NIC の資格認定は未実施であり、本番 homert は置き換えていない。

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

## 実上流: ens18 の実 HGW

`get-hgw-pd` の読取専用確認と homert の AFTR intent を使った。ens18 は管理経路でもあるため、
別 MAC・隔離 router/LAN namespace で試験する。host address/route/sysctl/clock は変更しない。
DHCPv6 Reply の送信元は49153。有効な DHCPv4 貸出も受けるが server identifier は HGW の IPv4 と異なる。

`3affd07` / `health-import` native-43 の7800秒試験が完走した。758回の LAN IPv4/IPv6 HTTPS と
学習 DNS UDP/TCP、再起動前の自然 Renew/対応 Reply 一組、最後の PD worker SIGKILL 後の
同じ DUID/IAID・Rebind/Reply・通信復旧、zero-lifetime RA・kernel 撤去・Release/Reply が成功した。
host 管理 address/MAC/default route は不変。private stage・capture は終了時に削除済みで、生存 owner は無い。
過去の長時間試験で起きた DNS 応答判定失敗の原因は未特定であり、今回の成功を修正済みとは扱わない。

最新 native-46 / `egress-route` でも30秒の `make check-uplink` が成功した。両 family HTTPS、
DNS UDP/TCP、DUID/IAID を保つ SIGKILL/Rebind 復旧、RA/Release 撤去と host 管理状態不変を確認した。
この短期試験では自然 Renew は未到達。7800秒の結果を最新実行版の結果に置き換えない。
再実行: `make check-uplink UPLINK_INTERFACE=ens18 UPLINK_AFTR=<確認したAFTR>
UPLINK_SECONDS=7800 RELEASE_ID=<実行版>`。
ONU 直結・物理 NIC・継続 TCP の資格認定は別である。ens20 の以前の無応答を上流全体へ外挿しない。

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
2. 実HGWの完走結果と最新版の短期結果を区別し、変更に必要な実回線検証を続ける。
   ONU/HGW側のRA/DHCPv4/DHCPv6 PDからLAN・DNS・DS-Liteまで、実相手と対象版を区別して進める。
   仮想peerとの成功を、この回線や物理NICの合格に置き換えない。
3. その後k8s-rt/pve-rtの実設定を確認し、Cloud SAMへ進む。
   SAMは旧docsの再設計目標を読み、認証/取消、構成配布、/32所有とNAT無し転送、移動・復旧を完成させる。
   新しい有料環境やproduction操作の許可を推定しない。
