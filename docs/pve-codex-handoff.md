# routerd2 開発引き継ぎ

2026-10-28。全34 work packageのゴールは継続中。homert本番の置換は未実施であり、
対象実機で利用可能とはまだ判断していない。[実装計画](implementation-plan.md)と
[機械可読台帳](implementation-plan.json)、[AGENTS.md](../AGENTS.md)を読む。
過去の試験記録を互換性・設計の制約にしない。ここには現在必要な操作条件と残作業を残す。

## 現在の変更: pd-host / native-44

homert02の現行設定と旧routerdの`delegatedAddressWithPrefixLength`・DS-Lite用source処理を
読み直した。homertはPDからWAN用の複数/128を設置するが、旧nativeは一律/64で、importも拒否していた。
`delegations[].prefix_length`でLAN /64とhost /128を区別し、旧指定をそのまま渡す。
別のDS-Lite host suffixを作る場合も/128とし、余計な接続/64 routeを増やさない。
既存のPD client・寿命・writerを使い、別のstate/選択処理や互換readerは追加しない。

PVE Linux6.8/amd64、`make check-dhcp6-ra-pd RELEASE_ID=pd-host
DHCP6_SERVER=dist/peer-env/isc-dhcp/root/usr/sbin/dhcpd`が成功した。製品CLIの変換結果と独立ISC/radvd/Linuxで、
同じ/64内のWAN /128を二つ設置し、両送信元のTCP/UDPとLAN向けkernel routeを確認した。
通常DAD、RA/IA_NA追加、worker交換・元期限・失効/復旧、DNS、DUID trial rollbackも通った。
実HGWでも30秒の`make check-uplink`を行い、WAN /128を使う実AFTR/DS-Lite IPv4 HTTPS、
IPv6 HTTPS、DNS UDP/TCP、worker SIGKILL/RebindのDUID保持、RA/Release撤去が通った。
同時実行中の別PDのgeneration/epoch/DUID/prefix/期限とhost管理状態は不変だった。
長時間試験は引き続きnative-43であり、このnative-44の自然Renew受入とは扱わない。
`make check`のGo/Python/Race/vet、両architectureのCGO_ENABLED=0 buildと最終六ELFの
署名catalog/publisher/stageのRace検査も成功した。ARM64実行、旧条件/VRRP importやhomert全体の置換は未完了。

## 直前の完了変更: health-import / native-43

旧routerdのHealthCheck engineとhomert02の現行設定を比較した。native workerは既に旧probe・
threshold処理を再利用している。今回、固定targetの旧HealthCheckをそのworkerへ変換し、
省略時の旧interval 30秒・timeout 3秒とthresholdを保持した。interfaceが無指定なら移行policyの
`health_checks`で対象interfaceを明示し、必要なsource/markも渡す。別WANへの迂回を健康と誤認しない。
interfaceの管理権限は独立宣言が必要。条件付き起動・動的target・旧EgressRoutePolicyの変換は未完了。

PVE Linux6.8/amd64、`make check-health-pbr RELEASE_ID=health-import`が成功した。
製品CLIで変換した二probeを実supervisor/control/network/healthへ渡し、独立した二WANとLAN端末・
packet captureでTCP/UDP切替・復旧を確認した。worker/control/network交換、古い通知、元期限の
撤去、confirmed rollbackも通った。これは旧homert条件全体の移行完了や物理NIC受入ではない。
`make check`のGo/Python/Race/vet、CGO_ENABLED=0の両architecture build、最終六ELFの
署名catalog/publisher/stageのRace検査も成功した。ARM64実行は未検証。

## 直前の完了変更: ha-pd-prepared / native-43

変更の起点は75650e4。homert02の稼働中設定を読み取り専用で確認し、PD clientがVRRP master条件で
動き、graceful activationがPD等の準備を待つことを確認した。本番設定は変更していない。
旧nativeではHA PDがVIP公開を待って循環することを実署名PID1二台・独立ISCで再現した。
現在は有効な選出でPD取得を始め、LAN DHCPv4配布はVIP公開後に始める。

最初の修正試行では間欠的なTCP停止やPD期限/復旧待ち失敗が残った。controlを計測すると、
不変のVRRP worker設定まで数十回書き直していた。設定受理時に全workerと両HA roleのfileを一度準備し、
観測更新からfile書込みとその後の二重status取得を除いた。別のcacheや有効flagは追加していない。
切替を繰り返した後も各設定fileの更新時刻が初期準備時から変わらないことを実プロセスで確認した。

PVE Linux6.8/amd64、20csのVRRP広告で、PD付き二台試験は修正後二回完走した。
上流ISC停止中の準備待ちtimeoutはVIPを公開せず、復帰後にPD取得→公開した。
同じTCP socket/UDP mapping、両方向切替、BOOTTIME +7200sの同期と元PD期限撤去・復旧、
古い通知/同期断/不完全TLS upload/worker・engine交換も通った。PD無しHAも成功した。
`make check`のGo/Python/Race/vet、両architectureのCGO_ENABLED=0 buildと署名catalog/publisher/stage、
Tailscaleの実PID1/独立peerの全通信・失効・rollback試験も成功した。ARM64実行と対象NICは未検証。

再実行は`make check-ha-pd check-ha-state RELEASE_ID=ha-pd-prepared DOCKER='sudo -n docker'
DHCP6_SERVER=dist/peer-env/isc-dhcp/root/usr/sbin/dhcpd`。fixtureはWAN選出とLAN VIP公開を
実addressで区別し、低速TLS uploadは100 Continueでbody読取開始を確認してから切替える。
期限やTCP待機上限は延長していない。調査trace・raw出力・完了済みstageは削除する。
実HGWの長時間試験は下記のDNS失敗調査へ進んでいる。生存中のownerのstate/captureだけ保持する。
旧条件付きinputのimport、広い分断/fencingと対象機器の受入は残る。

## 直前の完了変更: duid-import / native-43

旧routerdのDUID選択・PD snapshotとdnsmasqの永続lease設定を再確認した。
WANの明示`clientDUID`は既に変換する。LANの移行policyにも`server_duid`を追加し、
既存のnative明示DUIDへ直接渡す。旧lease fileのreaderやidentity移行層は作らない。
省略時は自動identityを永続化し、明示値・trialはそれを上書きしない。

PVE Linux6.8/amd64、`make check-dhcp6-ra-pd check-dhcp6-server RELEASE_ID=duid-import
DHCP6_SERVER=dist/peer-env/isc-dhcp/root/usr/sbin/dhcpd`が成功した。製品CLIの変換結果から
実daemonを起動し、独立ISC/radvd/BusyBox/Linuxと実Replyの指定server DUID、PD/RA・TCP/UDP・
DNS・WAN DUID rollbackを確認した。自動server DUIDはworker/control/daemon交換とrollbackで保持した。
`make check`、CGO_ENABLED=0の両architectureビルドと署名catalog/publisher/stageのRace検査も成功。
実homertのidentity移設、最新のNTT実回線・対象NIC受入は未検証。raw出力・capture・一時releaseは残さない。

### 直前の起動read修正

起動待機timeoutを旧b498dbdの実PID1で再現した。PVE Linux6.8/amd64、全試験processを
CPU0へ固定し、同CPUで関連release Race検査を並行した。network/logging/managementはReady、
controlは世代1・再起動0で起動中、read量362,516,067 bytesの時点で20秒上限に達した。
負荷無しの1 CPU旧版では14.669秒、controlのread量898,844,195 bytesだった。
`/proc/PID/io`の`rchar`は論理read量であり、物理disk I/O量ではない。

release `Open`→`WithState`→writer構築が、選択metadataを読む度に全artifactを3回取得・検証していた。
`Open`と`WithState`はcanonical journal・checksum・exact linkを検証し、runtimeの取得・prepare・
boot選択でpayloadを検証する責任へ整理した。設定writerの選択/store/startup fenceとtransaction lockは維持する。
20秒の待機上限で比較した。

修正版は同じ1 CPU/Race並行条件で15.732秒、control read量106,942 bytesでsocketを公開し、
その後の実通信・再起動・rollback・失効/再認証・tags/shields・DNSが通った。
独立Headscale0.29.6・公式Tailscale1.104.2・dnsmasq・TCP/UDP相手を使った。
関連release Raceも成功。破損payloadの取得/boot拒否、選択journalのfenceは局所検査で維持した。
再実行は`make check-tailscale RELEASE_ID=startup-io-fixed`。1 CPU比較ではCPU0へtasksetし、
別terminalで同CPUの`go test -race -count=1 ./internal/release`を並行する。
比較用b498dbd worktreeには同じfixtureと共有peer/engine/sourceを使い、終了後に削除した。

### 既存のTailscale観測・制御

旧routerdのstatus/controller/renderを読み、nodeとpeerのDNS名・付与済みtags、
現在のtailnet名・DNS suffix・MagicDNS設定を実daemonから観測するようにした。
接続先tailnetが無い場合は省略し、設定の`advertise_tags`を付与済みtagsとして表示しない。

実PID1/独立Headscale/公式peerで、CLI importしたshieldsUp:trueがホスト宛・広告LAN宛の
IPv4/IPv6新規TCP/UDPを遮断し、LAN発の新規TCP/UDPと返信は通ることを確認した。
confirmed rollback後は同じidentityで着信が復旧した。INPUTの許可と、着信成立を先に確認している。

Headscale0.29.6ではpersonal keyと広告要求だけではtagsが付かないことを実測した。
personal登録を管理者が失効させ、承認済みtagged keyで登録すると、serverの付与結果と
製品の観測が一致し、両familyの転送が通った。広告要求とkey fileを除いても保存identityは
server付与tagsを保持した。tagged nodeの管理者失効後、key無しのworker再起動では
転送とtailnet DNSが復活しない。serverの登録・承認を製品の設定rollbackで巻き戻さない。
独立peerのDNS名と管理者付与tagsも観測に反映され、LAN発の両family通信が通った。

### 既存のnative DNS統合

native DNSの`upstreamFrom`に`TailscaleNode/NAME`の`dnsServers`を接続した。
固定daemonのlocal MagicDNS `100.100.100.100`を、Running/Onlineかつ元観測期限内だけ使う。
OSの`acceptDNS`はfalseのままで、domainとTUNへのbindは明示native DNS sourceが所有する。
controlが配るglobal/split resolverやsearch domainの自動取り込みとは区別する。

実PID1/独立Headscale peerで、LAN UDP/TCPのA/AAAA、元条件期限切れ時のSERVFAIL、
node失効時の上流撤去、新keyによる同一identityの再認証後のDNS復旧が通った。
AAAA応答にはHeadscaleの`magicdns-aaaa` node attributeを設定する。
製品のDNS listenerにはLAN INPUT許可を明示する。
実測で約0.7〜0.9秒の元権限に対しDNSの固定1秒pollが遅く、周期的な期限切れを検出した。
既知の入力期限の半分を基に50ms〜1秒でpollする形へ直した。期限は延長しない。
連続した未cache問い合わせを複数更新周期にまたがって確認した。
同じresolverの別WAN sourceは障害中もUDP/TCPで応答し、private tailnet名はそこへ流れなかった。
packet captureでA/AAAA質問とlocal MagicDNSへの実通信も確認した。

### 既存のimportとidentity

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
公式peerの`TS_LOGS_DIR`も一時directoryへ固定する。

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

compiler native-43、worker ABI5、network journal11、DHCPv6 checkpoint2。
`release_id=startup-io-fixed`、b498dbdからの今回の変更で実行した。
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

再実行は`make check-tailscale RELEASE_ID=startup-io-fixed`。
peerのchecksum検査・必要時の取得、製品build、engine/source生成、署名catalog/PID1試験を含む。
資格情報・生ログ・capture・一時releaseは終了時に削除する。

実試行では、継承directory FDを別mount namespaceへbindできないこと、上流daemonが
ログ設定の保存先も必要とすることを確認し、所有する子の起動経路を修正した。
試験側の初期設定・app listener待機・capture reader・UDP遮断直後の経路待機も修正した。
設定transactionは完了を待つため12秒のclient上限を使い、意図した経路・転送の結果は弱めていない。
Runningやpingだけをrouter機能の受入とは数えない。

今回の`make check`でGo/Python・Race Detector・vetが通った。今回のrelease選択読取変更にも
関連Race/vetを実行し、`make build build-publisher build-linux RELEASE_ID=startup-io-fixed`で
nativeとamd64/arm64 core6をCGO_ENABLED=0でbuildした。最終の実PID1試験は
上記のCLI import・外部identity・再認証・経路受理・LAN DNS・shieldsUp/rollback・tagged登録と失効を含めて成功した。

engineは前回Go1.27.3/CGO_ENABLED=0でamd64/arm64ともbuild済み。今回amd64を再生成した。
pinned upstream sourceと実import71 moduleのzip、全license notice、build資料を生成する。
今回のcore6静的ELFの署名catalog/publisher/stageが両architectureともRace Detector付きで通った。
前回Tailscale engine/sourceを含む実署名publisher/stageもamd64/arm64双方で通った。
ARM64での実行・対象NIC性能・本番tailnet受入とは区別する。

### Tailscaleの残作業

`accept_routes`、承認済みtags、shields_upの両family実転送/遮断・復旧は確認済み。
OS acceptDNS/global・split resolver/search domainの取り込み、Tailscale SSHの別権限/OS契約、旧TailscaleNodeの条件付きimport、
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
RA/Release撤去まで成功した。`make test-tools`も成功。長時間試験の完走・自然Renewは未確認。
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
