# routerd2 開発・PVE VM検証 引き継ぎ

更新日: 2026-10-25（Asia/Tokyo）
対象: `https://github.com/imksoo/routerd2`

**継続ゴール:** 全残実装・必要試験・対象実機での利用可能性の受入まで進める。
部分checkpointでは完了しない。

最新product checkpointは `c14c764`。固定runtimeの条件付きstartup復旧と、内部release journalのruntime/configuration組を実装した。
候補採用前は元revisionとknown-good文書の一致、採用後はexact failed receiptと後続commitなしを要求する。失敗候補snapshotが消えても採用記録から復旧し、再実行で後続commitを上書きしない。
release prepareは候補・conditional fallback・config store identity・二世代の予約を同じcurrent切替で保存する。取消/失敗でwatermarkを戻さず、paired confirmはexact receipt/current revision、rollbackは再prepareを要求する。
参照の型とtransaction整合性までの内部APIであり、snapshot互換性・稼働中writer fence・paired loader/health接続は未実装。現appliance commandはpaired選択を拒否し、runtimeだけを誤って起動しない。
初回全体check/Race/vet/Python30/6 ELF build171.30秒、clean9.80秒PASS。6 SHAは一致し、release/configrefはcompiler/controlへ依存しない。
実signed release storeと実config journalの両復旧分岐・7保存境界・不正authority拒否を検証。実native Race7 gateはcatalog27.11秒・publisher12.76秒・DHCP10.80秒・SSH9.55秒・startup3.48秒・新recovery3.48秒・二WAN/PBR30.91秒PASS。
QEMU5 gateは新recovery25.82秒・startup23.80秒・SSH28.02秒・二WAN/PBR78.34秒・既存ext4二boot147.50秒PASS。paired signed PID1起動の証明ではない。
全計画、pairedの実起動/confirmation/fallback、書込み中電源断、ABI/権限/engine、実機受入は未完了。前回health falseの内訳/再発条件も未確定のまま保持する。
[条件付き復旧とpaired journalの証跡・境界・残作業](evidence/2026-10-25-paired-startup.md) を参照。全体ゴールはactive。

前回product checkpointは `1048243`、readiness fixtureは `f60b761`。固定runtimeのboot `startup`参照をcontrol workerが一度だけauthoritative journalへ採用する。
世代・expected revision・store/snapshot参照を検査し、採用revisionとstableなstartup_selected eventを同じrecordへ保存する。
同一選択の再実行は後続commitを保持し、未確認commitも通常の再起動rollbackへ渡す。古い世代、同じ世代の改変、別store、未確認状態、outbox満杯を拒否する。
compiler/kernel依存のないconfigref、bounded strict JSON、control専用の固定envも実装した。signed releaseとの同時指定は組のtransaction未実装のため拒否する。
clean check/Race/vet/Python29/6 ELF build110.17秒、fixture変更後10.84秒PASS。製品6 SHAは一致。
native Race6 gateはcatalog27.31秒・publisher12.41秒・DHCP10.65秒・SSH9.55秒・新startup3.48秒・二WAN/PBR31.12秒PASS。
初回QEMU startupは22.65秒FAIL（host36.03秒）。revision6/kernel Cは一致したがhealth falseで、内訳は記録されず具体的原因は未確定。
1 CPU native40回102.43秒では再現せず。fixtureは75秒の全体期限内でfull health/projectionも待つ条件へ強化し、製品は変更していない。
最終QEMUはstartup24.85秒・SSH29.41秒・二WAN/PBR82.13秒・ext4二boot149.32秒PASS。全suite、startup採用を伴う実PID1/電源断、signed組のfallback、対象実機は未完了。
[採用処理の証跡・初回失敗・残作業](evidence/2026-10-25-startup-adoption.md) を参照。継続ゴールはactive。

前回product checkpointは `1fb91bc`。設定journalに永続store identityを持たせ、確定済み現在revisionを不変snapshotとして保存・SHA/provenance検証する。
`routerd config snapshot --expected-revision N` とprivate control APIを実装し、commit/confirmed rollback/control再起動でidentityとsnapshotを保持する。
別store・古いrevision・unconfirmed・改変・symlink/hardlink・容量超過を拒否し、既存snapshotを上書き・自動削除しない。
未公開のstore identityなしjournalは拒否する。明示startup選択・authoritative journalへの採用・runtime/config組のboot transactionは未実装。
clean全体check/Race/vet/Python28/amd64・arm64計6 ELF build99.04秒PASS。arm64の実行は未検証。
実3 commandのnative Race harnessは署名catalog27.01秒・publisher12.66秒・DHCP10.35秒・snapshot CLI/SSH復旧9.30秒・二WAN/PBR31.17秒PASS。
QEMUも永続ext4二boot150.12秒、snapshot CLI/SSH28.42秒、二WAN/PBR81.53秒PASS。store/revision/digest、設定、SSH公開identity、event ID/cursorの保持を確認した。
snapshot書込中の電源断/disk full/fsync失敗、当版OCI/APK、対象実機・全計画の受入は未検証/未完了。継続ゴールはactive。
[設定snapshotの証跡・受入境界・残作業](evidence/2026-10-25-config-snapshots.md) を参照。

前回product checkpointは `f7da1c2`。manifest v2で可変native/data artifact、entrypoint、role→artifactを署名し、全descriptorを固定する。
旧manifest v1／旧暗黙pair publisher／`module_path`は拒否し、互換readerを作らない。factoryはsupervisor・native multicall・独立loggingの3 command。
loggingは約6.9MiBで他roleを拒否し、control/management/protocol engineをlinkしない。startup-config明示参照・全ABI/engine/権限分離は残る。
最終6 ELFのclean全体check/Race/vet/build11.04秒、disk fixture `e6720b9` の11.37秒、kernel image fixture `c88a897` のPython27を含む11.43秒PASS。6 SHAは一致。
実amd64/arm64 catalog署名/stage27.01秒、publisher12.76秒、native実DHCP10.25秒・SSH復旧9.30秒・二WAN/PBR31.12秒PASS。
OCI build133.02秒とnetwork none/read-only起動・終了1.19秒、Alpine APK unit/static/build266.83秒・実3 ELF検証1.03秒PASS。
QEMUはPID 1の3 native＋data起動確定83.25秒、永続disk2 bootのSSH設定/鍵/eventとfallback148.50秒PASS。
通常daemonも修正fixtureでSSH26.85秒・二WAN/PBR78.52秒・実UTC補正29.22秒PASS。関連5個別gateであり全suiteや実機完了ではない。
初回複製ELF容量不足、disk probeの旧filename、通常imageのunpack容量不足を保存し、局所再現後に修正した。
256MiB stageと192MiB展開の実tmpfsで再現。必要35 moduleへ絞ってpayload198.8→84.4MiB、unpack errorをFAILにする。
RAM/CPU/rootfs mount設定/scratch/既存期限は変更していない。CORE-06を進行中へ更新し、全計画と対象実機の受入は継続する。
[可変runtimeの証跡・受入境界・残作業](evidence/2026-10-25-runtime-catalog.md) を参照。

前回の時計補正受入はfixture `c051be2`、実行手順 `00c0d8e`。製品は `a136ae8` のまま。
外部NICのない専用QEMUのPID 1だけで、実CLOCK_REALTIMEを前後24時間補正して復元した。
元DHCPv4 BOOT leaseは前進後のconfig再適用とTCP/UDPを維持し、後退中も元期限でaddress/default routeを撤去する。
同じworkerがReadyのままのSIGSTOP失効、再送、worker/control再起動、旧世代409、fresh ACK復旧とcaptureを確認した。
新clock gate37.74秒・通常DHCP gate34.14秒PASS。native通常DHCPのRace harness10.15秒PASS、brokerなしclock必須実行は0.07秒で期待拒否。
clean全体check/Race/vet/Python24/両arch build9.39秒、実行手順追加後7.36秒PASS（cache含む）。4 ELFは前回checked pairと一致。
固定suite・per-boot kernel marker・private PID 1 socketpairでclock操作を隔離し、production両binaryには含めない。
実NTP discipline/slew・物理suspend・対象実機/NIC/性能と全残計画は未完了。work package/parityは変更しない。
[実clock補正の証跡・境界・残作業](evidence/2026-10-25-dhcp4-clockstep.md) を参照。

前回の期限伝搬検証はproduct `a136ae8`、fixture `66475eb`。
DHCPv4の元BOOT観測・論理epoch/sequence・ACKのlease clockをSnapshot→runtime/fact→network writerへ伝える。
最初の通知からlive supervisor世代を照合し、再送・phase表示・control cache初期化で元期限/ageを更新しない。
writerは絶対BOOT寿命をjournal2へ保存し、再open後もkernel残寿命をUTCから作り直さない。operatorの内部BOOT期限指定と未公開journal1を拒否する。
旧fa5792d pairは元BOOT情報不足で0.62秒FAIL。関連Race、clean全体check/Race/vet/Python23/両arch build128.43秒PASS。
初回nativeの8秒leaseと5秒heartbeat、次のcontrol復旧の同世代要求はfixtureの誤りだった。失敗10.55秒（test10.48秒）/8.74秒を保存し、TTL3秒と実quiesce契約へ直した。
最終fixtureの全体check/build7.41秒PASS（cache含む）。4 ELFのSHAは最初のchecked productと完全一致。
nativeはkernel自律失効8.80秒、新DHCP worker10.35秒、実DHCP/v6/RA18.83秒、native/migrated DNS10.76/10.65秒、
network21.74秒、BFD-derived13.86秒・BFD/BGP23.44秒・二WAN/PBR30.91秒がPASS。
QEMUもkernel20.79秒・新DHCP39.75秒・DHCP31.02秒・native/migrated DNS32.53/33.53秒・network39.04秒・
BFD-derived44.77秒・BFD/BGP56.40秒・二WAN/PBR97.13秒の関連9 gateがPASS。全suite/全計画の完了ではない。
IPv4 route撤去にはlive coordinatorが必要。実NTP/物理suspend、他protocol、全resource/残計画、対象実機/NIC/性能受入は未完了。
[証跡・検証scope・残作業](evidence/2026-10-25-dhcp4-authority.md) を参照。

先行product checkpointは `fa5792d`。
DHCPv4 clientのT1/T2/expiry、retry、transaction timeoutとpacket secsをBOOTTIMEで測る。
UTC補正で元期限を変えず、更新ACKだけで新期限を確定する。失効後queued ACKは旧leaseを復活させない。
旧timer処理の前後24時間補正FAILとclock入力adapterを保存し、関連package全体Race1.042秒PASS。
最初の全体checkは私のprivate scan用source copyがGo探索対象に入って23.34秒FAIL。内容を保ち `_` directoryへ移した。
clean全体check/Race/vet/Python23/static両arch build96.59秒PASS。
checked pair/native Raceの実DHCPv4/v6/RA18.94秒・DHCP→DNS10.70秒・migrated DHCP→DNS10.90秒PASS。
QEMUも実DHCP/v6/RA30.22秒・DHCP→DNS31.56秒・migrated DHCP→DNS32.36秒PASS。
この時点はclientのみの個別回帰で、Snapshot/runtime/network writerはUTC依存だった。元BOOT期限の伝搬は上記a136ae8で進めた。
次に元BOOT期限をこの境界へ伝え、worker/control停止・旧通知・復旧を受入する。全計画・実機の完了は宣言しない。
[証跡と次の境界](evidence/2026-10-25-dhcp4-boot-clock.md) を参照。

先行product checkpointは `8887e75`。
BGPRouterの `phase` / `establishedPeers` / `backend` / `observedAt` をDerivedEvent/EventRuleへ接続した。
全peerの観測と同じ経路/BFD依存のnetwork ACKを要求し、未解決FIBならDegraded、適用待ちはunknownになる。
同じ経路のheartbeatはACKを再利用する。engine walk前のBOOTTIMEで元の最大5秒を固定し、
BGPのFIBもその期限で失効する。最初の通知からlive supervisorのinstance/generation/digestを照合する。
旧95b2f43 pair20.33秒FAIL、関連4 package Raceとclean全体check/Race/vet/static両arch build115.99秒PASS。
native6 gateは新BGP-derived22.74秒・既存BGP9.70秒・BFD-derived13.71秒・BFD/BGP22.74秒・
Health/Derived13.38秒・二WAN/PBR32.13秒PASS。QEMUのBGP-derived54.19秒・BGP31.53秒・BFD-derived41.56秒・BFD/BGP56.57秒もPASS。
Health/Derivedは初回67.14秒FAILを保存。target保存後のACK喪失でpendingに残る既存prefixを、未保存と誤判定していた。
実SQLiteで局所再現後、fixture544233eで保存失敗境界の既存履歴と新規未保存を分けて検証する。
修正後clean全体check/Race/vet/両arch build12.80秒PASS（cache含む）、4 ELFのSHAは元と完全一致。
同じ製品pairの修正native Health/Derived13.01秒・二WAN/PBR31.12秒PASS。
修正QEMU Health/Derived100.08秒・二WAN/PBR85.05秒もPASS。関連6 gateの個別受入であり、全計画・全suite・対象実機の完了ではない。
受理RIBの件数をFIB候補数で代用せず、richer status/GR/旧resource/profile、他providerと全残計画は残す。
work package全体の完了とparityは変更しない。[証跡と残作業](evidence/2026-10-25-bgp-derived.md) を参照。

先行product checkpoint `95b2f43`では、BFD `observedAt` の `equals` / `any_of` で、通知集約中のA→B→Aや別の一致値への移行が
古いhysteresisを完成させる問題をtest-firstで再現し、修正した。
workerで集約前の時刻変更を数え、比較用continuityとpresence-onlyのcontinuityを分ける。
650msの実hold、正のcounterと定期更新、期限切れ・再送・worker/control復旧を要求する。
旧d376489 ELF4.43秒FAIL、関連4 package Raceとclean全体check/Race/vet/static両arch build115.48秒PASS。
checked pairのnativeはBFD-derived13.46秒・Health/Derived13.06秒・BFD/BGP23.14秒・二WAN/PBR33.25秒PASS。
最終QEMUはBFD-derived44.16秒・Health/Derived95.07秒・BFD/BGP53.39秒・二WAN/PBR98.27秒PASS。
時刻反転の再現はsimulated clockで、host/guestのCLOCK_REALTIME操作やNTP fault／実機clock受入ではない。
BGPを含む他providerと全残計画を継続する。work package全体の完了とparityは変更しない。
[検証・残作業](evidence/2026-10-25-bfd-timestamp.md) を参照。

先行product checkpoint `d376489`（BFD初回 `e848fbe`）では、BFDの `phase` / `peerStates` / `peers` / `observedAt` を有限な現在状態としてDerivedEventへ渡し、durable EventRuleへ接続した。
Up/Initは元packet受信期限を超えず、通知再送や表示で期限を延長しない。通知集約中のflapはcounterでholdをリセットする。
実UDP peerを使い、期限切れ・同一再送・worker/control再起動・旧通知409・過去Upを復活させない経路を追加した。
初回QEMUの既存Health/Derivedは52.11秒FAILで保存。localの圧縮保存16件/batchは2,098,093byteを割り当て、
store内codec workspace再利用で467,666byteへ減った。期限/RAM/vCPUは変更していない。
codec修正だけのQEMUも51.51秒FAIL。localで読取中のbody検証が新規保存まで1秒timeoutさせることを再現し、
有限pageを取得してSQL接続を返した後にgzip/JSONを検証するよう修正した。
event store全体Race24.008秒と最終clean check/Race/vet/static両arch build104.86秒PASS。
最終nativeはBFD-derived11.70秒・BFD/BGP22.87秒・Health/Derived13.11秒・二WAN/PBR31.18秒・archive7.84秒PASS。
最終QEMUはHealth/Derived92.09秒・BFD-derived41.76秒・BFD/BGP56.19秒・二WAN/PBR89.46秒・archive38.73秒がPASS。
実8MiB tmpfsのmandatory容量警告/保存失敗/回復/control SIGKILLも7.50秒PASS。
これはCORE-04のprovider部分実装であり、BFD raw producer、他provider/構造化selector/Derived連鎖、
全残計画と対象実機/NIC/性能受入は未完了。parityとwork package全体の完了状態は変更しない。
[契約](resource-program.md#derivedevent) と [検証・残作業](evidence/2026-10-24-bfd-derived.md) を参照。

先行product `3bca14b`では、eventの元payloadを、同じWAL/FULL transaction内のgzip領域へ自動退避し、hot枠/pageを回収する。
元event row・ID・cursorを残すため、未読consumer・pending outbox・timerの参照と再送ACKを維持する。
読取時はgzip/CRC・長さ・SHA・event IDを検証し、破損や余分なstream、展開量超過を拒否する。
`events` はhot、`archived_events` は圧縮、`history` は全履歴を表示し、exportは両領域を含む。
圧縮領域100万件/256MiB、全履歴100万件/元byte1GiBの上限を持ち、IDを忘れない。
最終clean全体check/Race/vet/static両arch build106.40秒、checked ELFと同じnativeは
archive6.75秒・Derived11.76秒・二WAN Health/PBR30.12秒がPASS。修正後QEMU archive41.97秒・Derived110.31秒・二WAN Health/PBR90.25秒もPASS。
実8MiB quotaでも保存失敗/回復/control SIGKILL6.45秒PASS。
三boot/二SIGKILLは72.91秒PASS。二度目のcut前後ともraw全4件が圧縮領域にあり、
元内容/cursor4/5/6/7と完了job記録3件を保持した。
初回QEMU Derivedは53.11秒FAILを保存した。繰り返し読取の再展開割当を736,221→71,755byte/16件へ減らし、
4MiB/4,096件以下のcacheと、圧縮をDB書込transaction外へ移す修正を加えた。期限/RAM/vCPUは変えていない。
大きなeventで7 SQLite pageを解放し、新eventが2 pageをDB拡大なしで再利用した。
DB schemaは6で、未公開1/2/3/4/5はread-only拒否。外部archive退避・保持期限/世代終了・
総filesystem予算・規模/長時間と全計画/対象実機の受入は未完了。
[契約](event-storage.md) と [検証・残作業](evidence/2026-10-24-event-archive-tier.md) を参照。

先行 `0b4bb28` では、完了したconsumer/producer outboxを、小さなSHA-256重複抑止記録へ置き換えて枠を自動回収する。
元key・cursor/初回state versionを保存し、待機中job・state・event履歴は変更しない。
同一再送/ACKは成功し、内容変更・元cursor変更・cursor巻き戻し後の再実行を拒否する。
独立した100万件/256MiBのtombstone予算と容量表示を設け、満杯で元記録を忘れない。
保存失敗はread-only inspectionで消さず、次の回収成功でhealth/confirmを回復する。
最終clean `b197c17` の全体check/Race/vet/static両arch build22.47秒、
実daemonの自動回収/保存失敗/復旧/control SIGKILL4.70秒、Derived11.93秒・二WAN Health/PBR29.85秒がPASS。
大きなpayloadの24 SQLite pageを解放し、新jobが16 pageをDB拡大なしで再利用した。
QEMU archive/自動回収35.54秒・Derived121.51秒・二WAN Health/PBR96.05秒、実8MiB quota4.41秒がPASS。
三boot/二SIGKILLは75.27秒PASS。完了回収記録1→3→3件と、元履歴4件/cursor4/5/6/7を保持した。
DB schemaは5、未公開1/2/3/4はread-only拒否。
[契約](event-storage.md) と [検証・残作業](evidence/2026-10-24-job-compaction.md) を参照。
event履歴のarchive/回収、dedupの長期運用、他残計画と対象実機受入は未完了。

先行product `6006090` はevent journalの件数/byte/DB/WAL/filesystem容量を可視化し、warning/exhaustedと
freshなtrial confirm拒否を追加した。`show event-store`、private API、localの
`routerd events export/verify` で、固定tipまでの完全な履歴prefixを0600の新規ファイルへ公開・検証する。
既存ファイルと競合作成先を上書きせず、元ID/内容/cursorを保持し、producerを止めない。
clean全体check/Race/vet/static両arch build102.77秒、実daemonのarchive1.99秒・Derived11.92秒・
二WAN Health/PBR29.56秒、実8MiB quotaでのhealthy容量警告1.65秒がPASS。
QEMU archive26.13秒・Derived89.26秒・二WAN Health/PBR87.25秒もPASS。
同じ新productの三boot/二SIGKILL回帰は72.65秒PASS。元2件と追加1件の内容/cursor 4/5/6を保持した。
これは容量観測と履歴copyであり、archive後もsource/dedup/cursorを削除しない。領域回収は未実装。
[操作と保存契約](event-storage.md) と [検証・残作業](evidence/2026-10-24-event-capacity.md) を参照。

先行の検証checkpointでは、製品コードf5e9cc5を変更せず、専用fixture `9e14ce8` で三回の実kernel bootを追加した。
未配信queue保存後と、target保存/local ACK後の登録解除失敗中に、hostからQEMUをSIGKILLした。
同じ256 MiB ext4 diskで、最初の2件と次bootの追加2件が一度ずつ配信され、二度目のcut後も内容・cursor 4/5/6/7が一致した。
登録待ちが再発見され、障害解除後は健全に戻り、Health/Derived現在値は復活しない。旧通知と履歴POSTは409。
全体check/Race/vet/static両arch build17.24秒、三boot QEMU合計74.36秒PASS。
これはguest VMの二境界の強制終了試験であり、host/物理storage電源断・全cut点・全計画の受入ではない。
[電源断試験の証跡と残作業](evidence/2026-10-24-health-power.md) と
[専用fixture](../images/health-power-test/README.md) を参照。再現targetは `make qemu-health-power`。

宣言を残した条件inactiveのHealth queueを、workerの再有効化と排他的に回収する。
選択binding・fresh supervisor・queue lockを確認し、宣言済みの所有登録は空になっても保持する。
`stopped_pending_relays` は途中のpageと保存失敗を可視化し、現在instance/Derived入力を復活させない。
所有権の待機中はcontrol source lockを解放し、設定更新と競合したcallbackは次回へ回す。
全体check/Race/vet/static両arch build100.37秒、最終nativeは条件停止4.41秒・永久削除1.58秒・
Derived11.88秒・二WAN Health/PBR29.89秒がPASS。固定harnessの実配送は旧238bc58が10秒timeout FAIL、
最終f5e9cc5が2.72秒PASS。2回の停止/回収/再有効化とcontrol SIGKILL、元event内容の一致を確認した。
最終QEMUは条件停止48.37秒・Derived97.76秒・二WAN Health/PBR91.65秒がPASS。
関連3系統の再検証であり、全suiteの新aggregate合格とはしない。
途中で検出したTryLockの回収見送り、control排他中の通常送信deadline、test用DDL競合の失敗と修正も保存する。
[条件停止回収・検証・残作業](evidence/2026-10-24-health-stopped.md) を参照。全計画・実機受入は未完了。

先行 `238bc58` は永久削除後の回収を追加したcheckpoint。

Health queueの所有登録をworker起動前に永続化し、永久削除後に実controlが回収する経路を追加した。
登録済みの名前だけを扱い、sourceからの削除・fresh supervisorでのworker不在・queueの排他lockを要求する。
元ID/内容をtargetへ保存しlocal ACKを完了してから登録を外す。missing storeを空で置換せず、
未登録・foreign状態は採用/削除しない。候補適用失敗後の元設定復旧でも残登録をhealthへ出す。
clean全体check/Race/vet/static両arch build131.74秒、実daemon11.79秒がPASS。
fixture期限修正後の実daemon12.15秒、未配信IDを対象にした小さな新旧比較1.68秒もPASS。
旧205f02fのdaemonでは永久削除後の配信が10秒timeoutでFAILし、差分を実配送で固定した。
QEMU拡張gateは113.66秒PASS。初回75秒の子fixture期限と、imageと同居するrootfsの容量不足は
別々に記録し、子150秒/親170秒とtest-only scratch 64MiBへ修正した。
6MiBのWAL作業領域不足をlocalで175件目のSQLITE_FULLとして再現し、同じ処理の64MiB/400件を確認した。
製品のdurability/容量/鮮度やVM RAM512MiBは変えていない。
関連QEMUは二WAN Health/PBR97.30秒、管理/実SSH41.82秒、EventRule63.62秒、BFD/BGP59.82秒がPASS。
既存の設定/journal/SSH保持・2回起動署名fallbackも120.84秒PASS。新queueの強制電源断合格とはしない。
[永久削除後の回収・失敗・残作業](evidence/2026-10-24-health-retirement.md) を参照。
当時の条件inactive回収は後続f5e9cc5で追加した。他protocol/status provider、実queueのQEMU電源断、retention/容量運用と
全計画・対象実機受入は残る。部分checkpointでゴールやparityを完了扱いにしない。

先行 `205f02f` ではHealthCheckのprobeごとの元eventをworker側のprivate WAL/FULL queueへ保存し、
実controlの限定APIからdurable journal/EventRuleへ接続した。
現在のsupervisor/config/generation/登録済みepochへfenceし、履歴で現在factを登録・更新しない。
ACK消失、実SQLiteのtarget保存後local ACK失敗、同時enqueue、64件/64KiB上限をRaceで検証した。
clean全体check/Race/vet/static両arch build110.66秒、実daemon9.61秒がPASS。
DB拒否中のqueue4件がhealth/controlのSIGKILL後も同じID・内容で一度だけ公開された。
QEMU拡張gate67.82秒、二WAN Health/PBR88.85秒、管理/実SSH36.55秒もPASS。
既存EventRule62.06秒・BFD/BGP53.44秒も同じproduct/integration sourceのQEMUでPASS。
[probe履歴の証跡と残作業](evidence/2026-10-24-health-protocol-events.md) を参照。
当時残っていたservice永久削除後の残queue回収は上記後続で追加した。
他protocol producer、retention/容量運用、他status providerと全計画・対象実機受入は継続する。

先行 `1ba9020` ではHealthCheckの有限なtyped current statusからDerivedEvent、durable journal、EventRuleまでを接続した。
`derived_event` resource、producer state/outbox、元観測時刻とauthorityのfence、sample間の
predicate continuity、read-onlyの `show derived RESOURCE_ID` を実装した。
source A→B→Aのscope再利用、inactive providerの旧Up、新authorityへの旧assertionのgrace継承を
local FAILで再現して修正した。全体check/Race/vet/static両arch buildと、実daemonの
管理8.21秒・Derived7.25秒、QEMU Derived37.17秒・管理/実SSH31.72秒がPASS。
同じproduct/test sourceの関連QEMU EventRule56.57秒、二WAN Health/PBR70.80秒、BFD50.96秒もPASS。
SIGSTOPした同じReady health processの観測をconsumer max_ageで失効させ、旧通知409、
control/health/logging再起動、実SQLite書込失敗の503と解除後の回復を検証した。
[live DerivedEventの証跡と残作業](evidence/2026-10-24-live-derived.md) を参照。
他status provider・構造化field・Derived連鎖・他raw protocol event・作用/federation・長期運用と
規模/対象実機受入は残る。HealthCheckのraw eventは上記後続で接続した。

先行 `6185ba8` ではEventRuleを `event_rule` resourceから実control workerへ接続した。新activationのtip開始、
state/cursor/outbox・timer復元、定義/条件/source fencing、安定ID付き履歴publication、
bounded healthを実装した。無条件ruleはlogging停止・control再起動でも集計を保持する。
timer/input順序、投影間flap、無関係なdegradedでの誤停止をlocal FAILで固定して修正した。
全体check/Race/vet/両arch build、実daemon29.09秒、新QEMU gate59.42秒がPASS。
実DBのlifecycle保存失敗でも、commit済みrevisionを保持し、不健全ACK/healthと回復を検証した。
先行259380dの関連QEMU management34.13秒、二WAN Health/PBR75.02秒、BFD52.17秒もPASS。
初回QEMUのhealth503による失敗は保存し、合格へ含めない。
[live EventRuleの証跡と残作業](evidence/2026-10-24-live-eventrule.md) を参照。
その時点の残作業だったHealthCheck→DerivedEventの接続は、上記後続checkpointで実装した。

履歴cursorに依存しないproducer state/outboxを追加した。空journalでDerived初回出力を
保存する実SQLite fixture、state保存失敗、SQLITE_FULL、16競合、SIGKILL/WALがPASS。
schemaは4で未公開1/2/3をread-only拒否。clean check/Race/vet/static両arch buildと、
QEMU実SSH/control再起動45.74秒、既存config/event/SSH保持の二回起動137.18秒がPASS。
同じclean ELFで実daemon二WAN TCP/UDP/PCAP・全復旧sequenceもRace込み30.09秒PASS。
当時はlive producer/controllerに未接続。後続で接続したが、新producerのQEMU電源断受入とは区別する。
[producer保存の証跡と残作業](evidence/2026-10-24-producer-state.md) を参照。

DerivedEventのpure evaluatorを追加した。有限snapshot、明示的unknown、BOOTTIME hysteresis、
元の観測時刻と同じwitnessの期限、安定出力IDを実装した。固定routerdとの直接比較、
実SQLiteのhold復元/保存失敗/retry/期限切れ、clean check/Race/vet/両arch buildがPASS。
当時はresource/daemonに未接続。空journalのbootstrap保存部とlive HealthCheck接続は後続で追加した。
[Derived componentの証跡と残作業](evidence/2026-10-24-derived-evaluator.md) を参照。

state/cursor/outboxの同時保存、timer version CAS、cursorだけの巻き戻し拒否を追加した。
当時のevent DB schemaは3。未公開1/2はD-09に従いread-only拒否し、互換readerは作らない。
EventRule全8演算のpure componentと安定ID/永続timer/budgetを追加し、固定routerdとの
直接比較、実SQLite保存失敗後のretry/reopen、Race/full check/static buildがPASS。
ResourceProgram/daemonのEventRule resourceへは未接続。

`03cac87` の二回起動はoutbox ACK前にconfig.jsonのhashを採ったためFAILした。
private diskのread-only抽出・未ACKレコード復元で初回hashと完全一致を確認した。
fixtureをpublicationとACKの両方を待つ形へ修正し、file全体hash比較を保持したまま
設定event ID/cursor一致も追加した。固定 `f4f6140` の二回起動は124.67秒でPASS。
[最新証跡・明示した残作業](evidence/2026-10-24-event-transitions.md) を参照。

`show observations [after CURSOR] [limit 1..16]` を運用/candidateモードと補完へ追加した。
全体check/static buildと、control SIGKILL後に実SSHでAPIと同じ履歴を読むQEMU試験がPASS。
[CLIの証跡](evidence/2026-10-24-observations-cli.md) を参照。

設定commit/confirm/confirmed rollbackのproducer outboxを設定本体と同時保存し、
control workerからWAL/FULL journalへ転送する。再送は同じIDでdedupし、容量不足では
履歴を捨てず、rollback枠を予約して新commitを副作用前に拒否する。
read-onlyのbounded `/v1/observations` とhealthのpending/errorを追加した。
全体check/static amd64/arm64、実SSH・control SIGKILLの履歴保持、
新コードの二WAN TCP/UDP/PCAP、QEMU management/Health-PBR/BFDはPASS。
[設定イベントの証跡・実行条件・残作業](evidence/2026-10-24-config-events.md) を参照。

先行clean `c2f3612` は一度のMake実行で**全20 QEMU系統PASS、exit 0**。
[一括試験の証跡](evidence/2026-10-24-qemu-aggregate.md) を参照。
後続 `7bdf27f` の関連3 QEMU gateを旧aggregateへ混ぜて全20再合格とはしない。
BFD admin watchdogは2秒、control freshnessは500ms。
[保存部・BFD方針の証跡](evidence/2026-10-24-events-clock.md) に変更経緯を保持する。

未完了は他raw protocol event producer、Health回収の全規模/遅延受入、EventRuleの規模/全条件受入、他Derived status provider、consumer作用、federation、
retention/archive/容量alarm、一般status reference、多binary runtime/startup handoff等の全計画。
対象機種・NIC・性能条件も未確定で、実機利用可能とはまだ判定しない。
完了したdetached worktreeは証跡を回収して削除した。private原log/manifest・診断patchは
`dist/goal-validation/retired-worktrees/`、公開証跡は `docs/evidence/` にある。

以下は先行checkpointの履歴。
fact leafの `max_age` と元の観測時刻の保持、条件式の `for` を実装した。
producer期限と独立した失効、通知間のflap、timer解除、authority変更を検証し、
`when_hold` / `ready_when_hold` と復旧待ち中のbackup TCP/UDPも実daemon・QEMUでPASS。
[鮮度制限](evidence/2026-10-24-condition-max-age.md) と
[安定時間の証跡](evidence/2026-10-24-condition-hold.md) を参照。
supervisor全体の再起動にもprotocol通知をfenceする起動ごとのIDを追加した。
実daemonで低いgenerationへの再利用・旧通知409・revision保持・TCP/UDP復旧を確認済み。
署名trialのPID 1自動確認と、private ext4の2回起動・設定/SSH保持・未確認trial fallbackもPASS。
[最新の証跡・制約](evidence/2026-10-24-supervisor-authority.md) を参照。
全20 QEMU gateの同一commit aggregateは固定worktreeで実行中。一般status reference等の残作業を継続する。
先行aggregateはBFDのadmin ACK timeoutで停止した。
ACK観測時刻を保持して遅い消費と区別し、受信済みACKを先に処理する修正を追加した。
500ms制限は保持し、consumer停止の再現・negative control・Race Detector・QEMU単独で検証した。
[BFD ACKの証跡](evidence/2026-10-24-bfd-admin-ack.md) を参照。
当時の修正後aggregateは `959ba98` の別worktreeで実行し、admin timeoutで停止した。
その結果は `dist/all-qemu-bfd-receipt.log` に保持する。

**PVE検証後の追記:** コードcheckpointは `7046fd5` へ進んだ。
`make check`、権限付きHealth実ソケット（raw ICMP含む）、二WANの実daemon/TCP/UDP/PCAP、
worker再起動・古い通知・fact expiry・confirmed rollbackを検証した。
`make check-health-pbr` とresource条件・保留理由のAPI/CLIを追加し、
最小kernelのNetlink lookup、generation再利用、nsfs mountinfo、VRRP停止を修正した。
詳細・未検証・残作業は [PVE検証証跡](evidence/2026-10-24-pve-health-pbr.md) と実装計画を参照。
以下は前環境からの引き継ぎ時点の履歴である。

実装コードのチェックポイント: **`834038edc7a9bf50e36f4f06fcf125f0aa2d88ff`**（mainへ反映済み）

## 1. 依頼と進め方

ユーザーの依頼は、`docs/implementation-plan.md` と `docs/implementation-plan.json` に従い、全機能の実装・検証を進めること。
開発とGitHub操作（mainへの反映を含む）は許可されている。通常の実装、テスト、commit/pushについて確認を繰り返さない。

**GitHub Actionsを待たず、ローカルで実装→関連テスト→修正を連続して進める。**
機能単位でRace Detector・vet・ビルドを確認してcommit/pushし、CIは別系統の統合検証として使う。
計画確認や定期タスクの設定だけで作業を終えない。

作業状態の正本はGitHubのコード、実装計画、検証証跡とする。チャットをまたいで別の計画を作り直さない。
既存の定期タスクが同時に動く可能性があるため、push前にremoteの進行を確認し、他の変更を上書きしない。

## 2. なぜPVE VMへ移すか

今回のクラウド実行環境では、次の操作が実際に拒否された。

- Unixソケットの作成・接続: `socket: operation not permitted`
- ICMP raw socketの作成: `listen ip4:icmp 127.0.0.1: socket: operation not permitted`

そのため、supervisor/control/networkの実プロセス連携、実daemon試験、raw ICMPを完走できない。
一方、通常のGo単体テスト、Race Detector、ビルド、loopbackのTCP/HTTP/DNSは実行できた。

**PVEホスト自体ではなく、使い捨て可能なLinux VMを開発・検証先にする。**
VMの管理経路を試験用ネットワークから分離し、netnsやQEMU内でWAN/LANを構成する。
本番WAN/HAの切替、本番ディスク初期化、有料クラウドの作成は今回の隔離検証と別扱いであり、勝手に対象を広げない。

## 3. 最新の実装内容

今回の開始点は `e4be8804fe70a3769523d188a8af1e86b26b5e77`。
以下の変更を `834038edc7a9bf50e36f4f06fcf125f0aa2d88ff` にまとめて反映した。

### CORE-03 / CORE-04: 条件とruntimeの接続

- ResourceProgramの `when` / `ready_when` を受け付け、三値評価を実際の投影へ接続した。
- 一つの投影内で、一貫したfact snapshotを使用する。
- `when` がfalseまたはindeterminateなら、そのresourceと依存・owner subtreeを保留・撤去する。
- `ready_when` は自身の起動を妨げず、依存resourceへの準備完了を制御する。
- supervisorの `service.NAME.ready` と、受理済みprotocol snapshotの `service.NAME.active` を評価する。
- boot ID、source digest、operator revision、worker generation、producer epoch・sequence、CLOCK_BOOTTIME期限で古い観測を排除する。
- 条件付き設定のprotocol callbackは観測を受理し、serialized reconcile loopがworker停止→network ACK→worker起動の順で適用する。
- 停止中にfactが変化した場合は再評価し、同一設定のworkerでも既に停止済みなら再起動する。
- confirmed commitの確認では、ready_whenと適用済み投影への収束も確認する。
- operator revisionとは別に、成功したnetwork projection generationをstatus/healthへ出す。永続復元や全worker世代管理の完成を意味しない。

主なファイル:

- `internal/resourceprogram/projection.go`
- `internal/runtimeplan/resource_conditions.go`
- `internal/runtimeplan/runtime.go`
- `internal/runtimeplan/resource_conditions_test.go`

### NET-01: Health workerの最初の実装

- `service.health` をserviceconfig、supervisor起動設定、worker dispatcher、runtime snapshot受理へ接続した。
- ICMP/TCP/DNS/HTTPに対応。interfaceを必須とし、source address・markを指定できる。
- targetは現段階ではIPv4/IPv6のunicast literal。動的targetSourceは未実装として扱う。
- TCP/DNS/HTTP probeと閾値判定は固定済みrouterd実装を再利用する。
- ICMPはinterface/markを設定したraw socketを使用し、送信先、echo ID、sequence、乱数payloadを照合する。
- Passing→Healthy、Failing→Unhealthyと復旧の閾値を扱い、historyは20件に制限する。
- `.active` は旧egress契約に合わせ、Healthyおよび閾値未到達のFailingでtrueとする。Passing/Unhealthyはfalse。未観測・失効はindeterminate。
- 有効期間は `max(2分, 3×interval + timeout)`。CLOCK_BOOTTIMEの期限とsupervisor世代の双方で失効させる。
- 停止時はwithdrawalを発行する。単なる再送で期限を延長しない。

主なファイル:

- `internal/protocol/health/health.go`
- `internal/protocol/health/probe_linux.go`
- `internal/protocol/health/README.md`
- `internal/serviceruntime/health.go`

### 経路確認と最小Health→PBR

- network workerにread-only `POST /v1/routes/lookup` を追加した。
- target/interface/source/markを指定したNetlink lookupを行う。外部の `ip` コマンドを呼ばない。
- 経路情報の取得はbest effort。取得失敗だけをprobe失敗に変換しない。
- `examples/health-pbr.yaml` を追加した。
- probe用host routeをapplication用tableから独立させ、Healthによるprimary経路の追加・撤去、常設backup経路、source PBR ruleを組み合わせた。
- **これは最小構成であり、EgressRoutePolicy全体のpriority/mark/hash/hysteresis互換を完成したものではない。**

### 併せて修正した不具合

- 再帰的な条件式の型がCLI補完候補を使い切る問題: 入力prefixに沿って候補を探索するよう修正。
- JST等では負のUnix時刻が1970年に見え、期限検証を通る問題: route/addressの期限をUnix時刻とUTCで検証するよう修正。

## 4. 実行済み検証と未検証を区別する

使用環境: Linux amd64、**Go 1.27.3**。実リポジトリをcloneし、実際の型・依存関係で実行した。代替型による独立テストではない。

| 検証 | 結果 |
| --- | --- |
| network / health / runtimeplan / resourceprogram / serviceconfig / serviceruntime / config / control / cli / cmd-routerdのテスト・Race Detector | PASS |
| `go vet ./...` | PASS |
| `CGO_ENABLED=0 go build ./cmd/routerd ./cmd/routerd-modules ./cmd/routerd-bundle` | PASS |
| `go test -run '^$' ./...` | 全packageのテストコードをcompile成功。テスト実行の成功とは区別 |
| loopbackの実TCP・HTTP・DNS probe | PASS |
| raw ICMP integration | socket作成が環境に拒否され失敗 |
| `go test ./...` 全体 | 実行したが未合格。Unixソケット依存の複数suiteが環境制限で失敗。DHCP server readinessもtimeout |
| 実supervisor/control/networkを使うHealth→PBR | 未検証 |
| 二WANの実TCP/UDP、packet capture、障害・復旧 | 未検証 |
| 今回の変更に対するGitHub Actions | 待機・合否確認をしていない |

runtimeのテストは本物のcoordinatorコードと、in-memoryのnetwork/supervisor境界を使う。
これを実kernelへの適用や実パケット転送の証拠として扱わない。

GitHubへ反映したtreeは `ee5d5f60d9d3479117c450e2f4dc81aec043a8b3`。
ローカルで検証したtreeとの完全一致を確認した。本引き継ぎ文書の追記は、そのコードチェックポイントに対するdocs-only更新である。

## 5. PVE VM側の最初の実行手順

### 5.1 作業環境

- 既存checkoutの未コミット変更を確認して保護する。自動resetや上書きをしない。
- checkout内にAGENTS.mdがあれば読む。
- GitHubの最新mainを取得し、上記コードチェックポイントを含むことを確認する。
- 検証したcommit SHA、OS/kernel、Go版、権限、実行ログを記録する。
- Go 1.27.3、C compiler、Python、Gitを準備する。
- Unixソケット、network namespace、CAP_NET_ADMIN、CAP_NET_RAWが利用可能であることを確認する。

```sh
git status --short
git fetch origin
git log -5 --oneline origin/main
git rev-parse HEAD
go version
```

cleanなcheckoutを用意したうえで、通常のfast-forward更新または専用worktreeを使う。
最新mainに後続変更があれば、今回のチェックポイントへ無条件に戻さない。

### 5.2 最初に閉じるローカル検証

```sh
make check
go test -race -tags integration ./internal/protocol/health -run '^TestScoped' -count=1 -v
make build-linux
```

`make check` はformat、全Goテスト、依存ライブラリのテスト、Python tool tests、Race Detector、vetを実行する。
失敗したら原因を修正する。skip追加や期待値の緩和で合格にしない。
特にクラウド側で実行できなかったUnixソケット・supervisor・ICMPのsuiteを確認する。

### 5.3 既存のkernel/QEMU gate

`images/README.md` とMakefileを読み、Docker・QEMU・image toolchainを準備する。

```sh
make image IMAGE_CONFIG=qemu
make qemu-network
make qemu-protocols
```

これらはローカルで実行する。GitHub Actions完了待ちへ切り替えない。
既存gateは今回追加した二WAN Health/PBRの受入試験そのものではない。

## 6. 最優先で追加する実daemon・実パケット試験

独立したWAN-A/WAN-B peerとLAN clientを持つnetnsまたはQEMU labを作り、実際のrouterd supervisor/control/network/health workerを起動する。
`examples/health-pbr.yaml` をlabのinterface/addressへ合わせる。

1. probeの送信interface/source/markがWAN-Aの指定と一致することをcaptureする。
2. WAN-Aを遮断しても、probeがWAN-Bへ回り込んで成功しないことを確認する。
3. LANからの実TCP/UDPが正常時はprimaryへ流れることを確認する。
4. unhealthy threshold到達後にbackupへ切り替わることを確認する。
5. 復旧後、healthy thresholdを満たしてprimaryへ戻ることを確認する。
6. probe用host routeとPBR rule、無関係な所有者のobjectが途中で失われないことを確認する。
7. health/control workerのkill/restart、遅延した旧世代通知、fact expiry、confirmed rollbackを試す。
8. operator revisionとprojection generation、表示状態と実経路が整合することを確認する。
9. テストをrequiredなlocal targetへ組み込み、PASS marker・capture・ログ・対象commitを残す。

失敗・未実行を成功として扱わず、問題を直してから対応する受入条件を閉じる。

## 7. その後の残作業

全34作業単位の完了には至っていない。計画書のwork packageとacceptance gateを正本として続ける。

- **CORE-03:** 一般status reference、全protocolのreadiness/lifecycle、resourceごとのeffects。
  `for` / `max_age` と最小Health/PBRの状態表示・実daemon受入は上記checkpointで追加済み。
- **CORE-04:** durable event journal、consumer cursor、outbox、再送・dedup。transient factとは別の保持契約で実装する。
- **NET-01:** targetSource/dynamic target、旧HealthCheck import、二経路probeと経路証跡の実パケット受入。
- **NET-02:** 完全なEgressRoutePolicy、priority/mark/hash、hysteresis、nft mark/connmark、table/ruleの所有権と実転送。
- 以降は計画のWAN/LAN、HA、BGP/BFD/WG、SAM/federation/cloud、運用surface、配布image、最終qualificationを順に進める。

`parity.json` のsemantic parityは今回の変更で上げていない。
実装の存在、mock試験、実daemon動作、実kernel/packet検証、対象環境qualificationを別々に記録する。

## 8. PVE側Codexへの開始指示

> imksoo/routerd2の最新mainを取得し、docs/pve-codex-handoff.mdと実装計画を読んで開発を継続してください。開発およびGitHub操作は許可済みです。GitHub Actionsを待たず、PVEの隔離Linux VMでローカルビルド・単体テスト・実daemon・実パケット試験を進めてください。最初にクラウド環境で阻まれたUnixソケットとraw ICMPを含む検証を閉じ、次に二WANのHealth→PBR障害切替・復旧を実証してください。その後も全実装計画を継続し、計画確認だけで停止しないでください。
