# routerd2 開発引き継ぎ

2026-10-27。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

## 現在の変更: tailscale-import / native-41

旧`TailscaleNode`をCLI importへ接続した。`tailscale` policyでowned TUN・bootstrap DNS/trustと
`key_file`または`saved_identity`を明示する。旧`authKeyFile`はsystemd EnvironmentFileなので、
生keyとして読まない。旧inline/env資格情報は選択したnative enrollmentへ置き換え、値を出力しない。
旧routerd所有のUDP41641 INPUT例外は、WireGuardと共用する最初のuntrust interfaceの計算で、
元ACLより前のscopeを保つ。外部daemonのnetfilterやkernel状態を引き取らない。

実fixtureは旧sourceを製品CLIで変換し、そのinterface/INPUT/nodeを、明示native LAN/WAN・
FW/NAT・PBR・条件と組み合わせて初回起動する。公式daemonを実CLIで先に登録し、停止・
kernel所有解除後、その不透明なstate fileだけをprovisionしてnative PID1へ移した。
同じnode ID/addressで実転送が通った。失効後の新keyによる再認証でもidentityを維持し、
`acceptRoutes:false`時は受けなかった独立peerのIPv4/IPv6 prefixを、trueへ変更してtable52へ入れた。
LANからpeer側subnetへNAT無しTCP/UDPが通り、相手が元LAN送信元を受信した。
state fileの編集/削除、productへの手動route注入は行っていない。

旧sourceの条件付きgating、acceptDNS省略/true、SSHは一括拒否する。nativeの条件付き実行とは区別する。
移設の確認は公式/native双方1.104.2で、旧版・hardware-sealed identityの資格認定ではない。
実装契約とpolicy例は[legacy importer](../internal/legacyconfig/README.md)を読む。
公式peerにも`TS_LOGS_DIR`を一時directoryへ明示し、default先に残った試験用log設定209 bytesは削除した。

### 既存のnative Tailscale経路

旧routerdの`pkg/tailscale`、`pkg/render/tailscale_systemd.go`、
`pkg/controller/chain/system.go`、`docs/how-to/tailscale.md`を読んだ。
旧実装と同じく実績あるdaemonを使い、製品workerが固定Tailscale1.104.2とidentityを所有する。
boot/controlへengineを組み込まない。起動はdisabledとし、typed LocalAPIの
`Start(UpdatePrefs)`で現在の設定を一括適用する。上流のPersistを保持し、vendor stateを手編集しない。
instance名変更でもidentityの保存先を変えない。設定と保存identityを別に扱う。

daemonがTUN・address・table52/rule5210..5270を、network workerが明示FW/NATを所有する。
既存foreign scopeとnative intentの競合を拒否する。namespace socketで別state directoryとの
同時所有を排除し、原子的なboot/network-namespace markerでSIGKILL後の専用scopeを回収する。
CA・resolver・`/run`を私有化し、通常のDNS処理をhostのresolved/DBusへ向けない。
条件付きserviceは、適用済みResourceStateの元期限で停止する。pollや通知の受信で期限を延ばさない。

compiler native-41、worker ABI5、network journal11、DHCPv6 checkpoint2。
`release_id=tailscale-import`、936c3b5からの今回の変更で実行した。
実装と必要な設定は[README](../internal/protocol/tailscale/README.md)を参照する。
engine無しのdefault host catalogはTailscaleを広告しない。署名catalogにはengine/source両artifactを結ぶ。

### 実通信で確認した範囲

PVE Linux6.8/amd64の隔離network/mount/PID namespaceで、実署名catalogからPID1・
supervisor/control/network/Health/Tailscale workerを起動した。
独立Headscale0.29.6、公式Tailscale1.104.2 peer、dnsmasq、別LAN/WANのTCP/UDP相手を使った。

- hostnameのcontrol serverへ、私有bootstrap DNSとTLS trustから接続した。
- IPv4/IPv6 subnetとexit-nodeのTCP/UDPが通り、WAN側serverが実NAT送信元を受信した。
- 実UDP遮断でDERPへ移り、同じsubnet/exitのIPv4/IPv6 TCP/UDPが通った。直通も確認した。
- worker SIGKILL後、同じnode ID/addressで転送が復旧し、table52のlookup ruleが重複しなかった。
- auth keyを設定から除きファイルも削除した後、control交換とPID1全体の再起動でidentityを再利用した。
- 広告routeを変えるconfirmed trialで独立peerのroute撤去を確認し、rollback後に元routeとIPv6通信が戻った。
- control停止中、元条件期限でTUNを撤去した。controlの世代交換前に観測し、復帰後の通信も確認した。
- nodeを失効させると通信が止まり、keyのないworker再起動でも旧権限が復活しなかった。
- WAN source policyをlookup5270より後の6000に置いた構成で実転送を確認した。
- captureでLAN/WANのTCP/UDP、DNS、暗号化underlay packetを確認した。

再実行は`make check-tailscale RELEASE_ID=tailscale-import`。
peerのchecksum検査・必要時の取得、製品build、engine/source生成、署名catalog/PID1試験を含む。
資格情報・生ログ・capture・一時releaseは終了時に削除する。

実試行では、継承directory FDを別mount namespaceへbindできないこと、上流daemonが
ログ設定の保存先も必要とすることを確認し、所有する子の起動経路を修正した。
試験側の初期設定・app listener待機・capture reader・UDP遮断直後の経路待機も修正した。
設定transactionは完了を待つため12秒のclient上限を使い、意図した経路・転送の結果は弱めていない。
Runningやpingだけをrouter機能の受入とは数えない。

今回の`make check`でGo/Python・Race Detector・vetが通った。最終import変更にも
関連Race/vetを実行し、`make build build-publisher build-linux RELEASE_ID=tailscale-import`で
nativeとamd64/arm64 core6をCGO_ENABLED=0でbuildした。最終の実PID1試験は
上記のCLI import・外部identity・再認証・経路受理を含めて成功した。

engineは前回Go1.27.3/CGO_ENABLED=0でamd64/arm64ともbuild済み。今回amd64を再生成した。
pinned upstream sourceと実import71 moduleのzip、全license notice、build資料を生成する。
今回のcore6静的ELFの署名catalog/publisher/stageが両architectureともRace Detector付きで通った。
前回Tailscale engine/sourceを含む実署名publisher/stageもamd64/arm64双方で通った。
ARM64での実行・対象NIC性能・本番tailnet受入とは区別する。

### Tailscaleの残作業

`accept_routes`の両familyの実経路/転送は確認済み。tags、shields_upの実相互接続は未検証。
acceptDNSとnative DNS、Tailscale SSHの別権限/OS契約、旧TailscaleNodeの条件付きimport、
旧版/sealed identityの移設、OS image/TUN setup統合、広い障害条件と対象実機受入は未完了。
旧daemonが所有するtable52を実行中のまま引き取らない。切替条件とidentityの所有を明示する。
EXT-02全体やhomert置換可能とは判断しない。

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

## 実上流の不足

ens20を使う許可済み隔離macvlan/LAN試行では、RS/Solicitの送信に対するRA/DHCPv6応答が無く、
DHCPv4も取得できなかった。元のens20 MACからのRSにもRAが無かった。
hostのIPv4 gateway到達性はあるが、回線・PVEの追加MAC制約・接続変更のどれが原因かは未確定。
ONU/PD配布側との現在の接続条件を尋ねた質問は未回答。SSH先を繰り返し要求しない。
同じ無応答試行を成功扱いせず、setupが変わるまで独立作業を続ける。
`make check-uplink UPLINK_INTERFACE=ens20 UPLINK_AFTR=...`には実際の回線のAFTR指定が必要。
`gw.transix.jp`は過去の例であり、この回線の確定値ではない。
最新版の実ONU・対象NIC受入は未完了。

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
2. 実上流のsetupが使えれば、ONU/HGW側のRA/DHCPv4/DHCPv6 PDからLAN・DNS・DS-Liteまで通す。
   仮想peerとの成功を、この回線や物理NICの合格に置き換えない。
3. その後k8s-rt/pve-rtの実設定を確認し、Cloud SAMへ進む。
   SAMは旧docsの再設計目標を読み、認証/取消、構成配布、/32所有とNAT無し転送、移動・復旧を完成させる。
   新しい有料環境やproduction操作の許可を推定しない。
