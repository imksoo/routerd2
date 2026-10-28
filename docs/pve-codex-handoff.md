# routerd2 開発引き継ぎ

2026-10-28。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

## 作業中: 旧 routerd の PD / DS-Lite を反映 / native-49

旧 routerd の DHCPv6 client は明示 DUID を優先し、それ以外は interface の MAC から
DUID-LL を生成する。保存リースは DUID・IAID・interface・期限が一致する場合に復元し、
通常停止では自動 Release しない。NTT 回線の旧調査でも DUID-LL の利用と識別子の維持が必要だった。
routerd2 は自動 DUID をリースと別に保持する。通常停止・切替ではローカルのアドレスと RA を撤去し、
上流への Release は送らず、元の期限を持つリースを再起動用に残すよう変更した。
PVE Linux 6.8/amd64 の実 HGW 30秒試験では、SIGKILL と通常停止後に同じ DUID・IAID、
通常停止後に同じ prefix と LAN IPv4/IPv6 HTTPS・DNS の復旧を確認した。寿命0 RA と kernel 撤去を
確認し、停止時の Release は観測されなかった。自然 Renew、ONU 直結と物理機器の受入は未検証。
明示 Release 操作は未実装。
独立 dnsmasq の IA_NA 試験でも通常停止後のローカル撤去、同じ DUID とアドレスでの再起動、
TCP/UDP の復旧を確認した。自然更新、失効と再取得、NotOnLink と DAD failure も同試験で通った。
独立 PPPoE・ISC DHCP による PAP / CHAP でも PD 更新、期限切れ、同じ識別子での復旧、
PPP 交換と rollback を確認し、RA 撤去が PPP 切断に先行し Release を送らないことを確認した。
QEMU appliance の同条件での停止・再起動は未再検証。

旧 routerd の専用 DS-Lite 送信元 `/128` は他の IPv6 通信に自動選択されないよう非優先化される。
routerd2 も同様にし、元の PD の優先期限と有効期限、アドレス所有、DAD 完了を確認してから使う。
旧設定の PD Bound と専用アドレス Applied の依存を取り込み、role 条件とともに適用する。
旧実装の待機系での事前アドレス準備は未実装。PVE 上の実プロセスと独立 ISC DHCP・Keepalived・
Linux AFTR では、実 kernel の deprecated 状態、切替・期限切れ後の復旧、IPv4 TCP/UDP を確認した。
capture では双方の IPv4-in-IPv6 を確認した。複数障害の重なりによる既存 TCP の失敗は未解決。
変更後の二台試験では、PD と主 WAN の Health を必須とした VIP 公開条件のため、PD 失効時に
予備 WAN が使えても VIP が撤去された。主 WAN または予備 WAN のどちらかが使える条件へ試験設定を
修正し、PD 失効後の予備 WAN TCP/UDP と再取得後の DS-Lite 復帰を確認した。旧 homert の PD 必須条件も
同じ意図の確認が必要。
予備 WAN の通信を止めた再試験では、最初の失敗で旧 HealthCheck 状態機械が返す `Failing` を、
routerd2 が無条件に利用可能と判定して VIP を誤公開した。十分な連続成功で Healthy になった後の
一時的な失敗だけを猶予するよう変更した。修正後、両 WAN の相手が停止した起動時は VIP を公開せず、
相手の復旧後は PD と IPv4 TCP/UDP が戻った。二台の既存 TCP・UDP は双方向切替と worker/engine
再起動後も継続し、この試行の TCP 最長応答待ちは約13.3秒、UDP 損失は0件だった。
変更後の `make check`、独立 Health→PBR と複数 WAN 試験、amd64/arm64 の静的ビルド、
実行ファイルの署名・publisher・配置検証も通った。

旧 dnsmasq のリース同期は別の定期処理で、同期先の停止は配布応答を止めない。現行 routerd2 は
同期先への書き込み失敗で DHCPv4 応答を止めるため、片系運転の実装と実クライアント検証が残る。
同期間隔中の障害や分断で未同期の割当が生じる問題を、旧実装が解決済みとは判断しない。

## 現在の変更: HA・PD・DS-Lite / native-48 / network journal12

旧 routerd の DHCPv6 client と DUID・lease 保存を参照した。自動 DUID は lease と別に永続化し、
明示 DUID は自動 identity を上書きしない。HA では設定した同じ DUID/IAID と元の grant 期限を共有する。
PD の delegation 先を client 起動の依存から外し、待機系が未作成の WAN identity を待たずに同期できるようにした。
DHCPv6 server は受信待ち中に届いた最新の入力を処理してから、応答の authority と option を判定する。

PVE Linux 6.8/amd64、`ha-egress` の署名済み実 daemon 二台、独立 ISC DHCP、Linux AFTR、
IPv6-only の主 WAN、独立 IPv4 予備 WAN と LAN client で検証した。PD 由来 B4、Health→PBR の
障害・復旧、同じ DUID/IAID・prefix と期限を保つ双方向切替、conntrackd 強制終了後の復旧、
同期断、古い通知、PD 失効と新規 TCP/UDP の予備 WAN・主 WAN 復旧が通った。capture は既存 TCP/UDP の
双方向 IPv4-in-IPv6、直結 IPv4 経路への迂回なし、TCP SYN 一回と同期 packet を確認した。
既存 TCP の最長応答待ちは約13秒、UDP 損失はこの試行で0件だった。広告間隔は homert の既定1秒で、
主 WAN の Health 成立を VIP 公開条件とした。既存接続を維持できない別の WAN へ切り替えた後は新規通信を確認した。
20cs の統合構成では worker の再起動と復旧の不安定さを観測しており、原因と切替遅延の改善は残る。
1秒広告で control 停止からの復帰中に次の切替を重ねた試行では、既存 TCP が15秒で応答せず失敗した。
各切替で同じ TCP/UDP の復旧を確認してから次を起こした再試行は完走し、最長応答待ちは約8.4秒だった。
その再試行では個別切替の VIP 公開まで約6秒、TCP 応答まで公開後約1秒以内だった。
別途観測した昇格では、選出から PD 再開まで約1.4秒、続く B4 の DAD と AFTR 準備に約2.4秒、
Health と VIP 公開に約1.1秒を要した。DAD の待機を省かず、連続障害の失敗と改善余地を追う。
初期試行で対向 AFTR の link down が戻り経路を削除し、IPv4 の直結迂回が生じたため、その試行は合格から除外した。

`make check`、既存の Health→PBR、直接 WAN と PD の HA 試験、独立 DHCPv6 information client、
amd64/arm64 の CGO_ENABLED=0 build、六 ELF の署名 catalog/publisher/stage Race 検査が成功した。
同版の実 HGW 30秒試験は DUID/IAID を保つ worker 復旧、IPv4/IPv6 HTTPS、学習 DNS、RA/Release 撤去が成功し、
自然 Renew は観測していない。ONU 直結、物理 NIC、ARM64 実行、homert 全体の受入は未検証。

## 直前の確認: source-hash / native-47 / network journal12

`forwarding.ipv4_ecmp_hash: source` を既存の writer/setting owner へ接続した。
namespace 全体の IPv4 ECMP に Linux custom source-only hash を使い、別の nft/connmark 経路は追加しない。
元 policy/mask を記録し、有効化と解除の順序、writer 交換・rollback を既存 journal で処理する。
独立二 WAN・三宛先の実 TCP/UDP で、送信元ごとの経路固定、両 WAN 利用、障害/復旧、
kernel 元値の解除と rollback 再適用、writer 交換・元期限撤去が成功した。
初回は不要な反復通信が capture の4096 frame上限に達したため、上限を維持して必要な通信へ絞った。
`make check` の Go/Python/Race/vet、両 architecture の CGO_ENABLED=0 build と最終六 ELF の
署名 catalog/publisher/stage Race 検査も成功した。旧 journal11 の開発 fixture は12へ更新し、互換 reader は作らない。
新形式でも既存 Health→PBR の切替・古い通知・元期限・rollback 試験が成功した。
実 HGW の30秒試験も PD/両 family HTTPS/DNS、DUID/IAID を保つ crash/Rebind、RA/Release 撤去を通し、
host 管理 address/MAC/default route は不変だった。native-47 の自然 Renew は未到達。

## 直前の確認: egress-route / native-46

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
IPv6 動的選択、policy ごとの mark/hash、既存接続継続、旧 EgressRoutePolicy 全体と homert 全体の受入は残る。

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
   統合構成の20cs広告時の不安定さと既存 TCP の最大約13秒の応答待ちを調査する。
   Tailscaleの残項目、旧stateless DHCPv6入力の条件付きHA/import不足、VRRP/PDの分断・復旧、
   管理・更新・戻し方を実際の相手で確認する。本番切替は行わない。
2. 実HGWの完走結果と最新版の短期結果を区別し、変更に必要な実回線検証を続ける。
   ONU/HGW側のRA/DHCPv4/DHCPv6 PDからLAN・DNS・DS-Liteまで、実相手と対象版を区別して進める。
   仮想peerとの成功を、この回線や物理NICの合格に置き換えない。
3. その後k8s-rt/pve-rtの実設定を確認し、Cloud SAMへ進む。
   SAMは旧docsの再設計目標を読み、認証/取消、構成配布、/32所有とNAT無し転送、移動・復旧を完成させる。
   新しい有料環境やproduction操作の許可を推定しない。
