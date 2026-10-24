# BFDの有限な現在状態からDerivedEvent/EventRuleへ

2026-10-24 UTC。最終product checkpoint `d37648960abb374f7366bc792b7007bb02d6cfb5`。
BFD初回は `e848fbe`、codec再利用は `896b61a`。
CORE-04のprovider部分実装。全計画・対象実機の受入、BFD resource全体のparityは未完了。

## 実装した契約

実BFD workerの `phase`、`peerStates`、`peers`、`observedAt` を、ResourceProgramのtyped参照から読む。
固定した旧routerdのcontroller/Derived評価コードを読み、4 fieldの文字列表現と集約phaseを維持した。
peerごとのselectorやDerived連鎖、他protocol providerを対応済みとは数えない。
[設定と型/表現](../resource-program.md#derivedevent) と [BFD操作](../bfd.md#current-status-in-derivedevent) を参照。

snapshotにkernel boot IDでスコープしたBOOTTIME観測/期限を追加する。local状態は元観測から5秒、
Up/Initはさらに全該当peerの元パケット受信期限、current supervisor観測期限で制限する。
同じ通知の再送・periodic publish・GETでUpの受信時刻/期限を延長しない。
raw Upが期限切れならunknownとなり、Down eventを捏造しない。
現在のactive宣言/binding/digest/generation/producer epochとfresh ready supervisorが必要である。
履歴のasserted booleanを現在のUpへ復活させない。

BFDの受理は既存の独立した短いcache lockを保ち、network applyの排他を待たない。
immutable cacheを先に取り、その後clockを読む。peer discriminator/遷移counterをcontinuityへ含め、
通知集約中のUp→Down→Upを、古いpending holdへ継ぎ足さない。
集約phaseが同じでもpeerの変化で保守的にholdをやり直し、counter飽和はunknownとする。
continuity変更をworker authority変更とは分け、同じworkerでのretraction graceを維持する。
全64 peerの旧 `peers` 表現を収めるためfield上限を64KiBへ広げ、全snapshot2MiB/1,024参照の上限は維持した。
大きな値をdurable producer stateへ保存しない。Derivedのassert/retract出力は既存のatomic outbox/EventRuleへ接続する。

## test-firstと検証結果

先に有限観測、元packetの年齢、通知集約flap、worker replacement、inactive/type/field検証、
64KiB境界とdurable stateへの非コピーをtest commitへ固定した。
[protocol未実装FAIL](2026-10-24-bfd-derived/status-red.log)、
[runtime未実装FAIL](2026-10-24-bfd-derived/runtime-red.log)、
[continuity未実装FAIL](2026-10-24-bfd-derived/continuity-red.log) を保存する。
実daemon fixtureもproduct前にcommitした。SHA照合済み旧 `3bca14b` ELFは、
BFD typed参照を未対応として20.19秒FAILとなる。
[旧pair](2026-10-24-bfd-derived/before-binaries.json) と [旧daemon excerpt](2026-10-24-bfd-derived/native-old-red-excerpt.log) を参照。

最初のruntime試験は、試験データの受信時刻だけを進め、受信counterを増やさなかったため拒否された。
[FAIL](2026-10-24-bfd-derived/fixture-receive-counter-failure.log) を保存し、counterを正した。
正当な裏付けのない受信更新を拒否するproductの検証は緩めていない。
最大64 peer、Up/Down/Degraded/Pendingの表現とfield容量も確認した。
[最終関連Race](2026-10-24-bfd-derived/focused-race.log) はBFD1.325秒、runtime8.232秒、Derived/resourceprogramはcache済みのPASS。

初回clean `e848fbe` の `make check build-linux RELEASE_ID=e848fbec6ecd` は **120.40秒PASS**。
全unit・Python23・Race Detector・vet・static amd64/arm64 buildを含む。
[check](2026-10-24-bfd-derived/check-build.log) と
[開始/終了commit・dirty=false・ELF SHA](2026-10-24-bfd-derived/build-manifest.json) を保存する。

最終clean `d376489` の全体check/Race/vet/static両arch buildは **104.86秒PASS**。
[最終check](2026-10-24-bfd-derived/final-check-build.log) と
[最終build manifest](2026-10-24-bfd-derived/final-build-manifest.json) に開始/終了commit・dirty=false・SHAを保存する。

nativeは最終checked amd64 pairとSHAが一致し、root所有0700 stagingからRace harnessで実行した。
新fixtureは独立したUDP codec/peerと実supervisor/control/network/BFDを使う。
最初のDown、二度のUpと一度のDown、4 fieldを読むDerived出力、2回のassertを数えるdurable EventRuleを要求する。
BFDをSIGSTOPし、raw Up/PID/readyを保ったままcurrent値が失効することを確認する。
同一通知のACK成功でも期限を更新せず、SIGCONT・worker SIGKILL後はfresh観測で回復し、古い通知は409となる。
peerを止めてcontrolをSIGKILLし、保存済みassertionが現在Upへ戻らないことと、再受信後の回復も確認する。

| gate | 最終native | 最終QEMU |
| --- | --- | --- |
| BFD current→Derived/EventRule・expiry/replay・worker/control復旧 | 11.70秒PASS | 41.76秒PASS |
| 既存BFD→実BGP session/FIB・UDP-only loss・SIGSTOP expiry | 22.87秒PASS | 56.19秒PASS |
| Health→Derived/raw履歴・worker/queue復旧 | 13.11秒PASS | 92.09秒PASS |
| 二WAN Health→PBR・実TCP/UDP/PCAP・confirmed rollback | 31.18秒PASS | 89.46秒PASS |
| 圧縮履歴・保存失敗/回復/control SIGKILL | 7.84秒PASS | 38.73秒PASS |

[最終native manifest](2026-10-24-bfd-derived/final-native-manifest.json) と各test logを保存する。
初回e848fbeの4 native試験は別の [初回manifest](2026-10-24-bfd-derived/native-manifest.json) に残す。
既存native BFD試験はexact Go PASSを出し、QEMU用markerはinitが出す。
private runnerがin-test markerまで要求して一度停止したが、product試験自体はexit0/PASSだった。
[元runner結果](2026-10-24-bfd-derived/native-first-runner-manifest.json) とerrorも保持し、
同じ成功試験を再実行せず、正しい契約で結果を検査して残り2試験を実行した。
regression側のdirtyはdocs変更であり、実ELFとRace harness SHAは最初のclean側と一致する。

QEMUは同じproduct/integration sourceを使用し、512MiB/2vCPU・外部NICなし・private64MiB scratch。
release pairとQEMU test identityのELFは別buildとしてSHAを記録する。
最終QEMU5 gateはすべてPASSし、各manifestの二ELF・test binary・init・kernelのSHA一致も確認した。
関連5 gateの再検証であり、全QEMU suiteの新aggregate合格とはしない。
実機NICやホストnetworkへ適用していない。

## QEMUで検出した履歴処理の問題

初回e848fbeのBFD-derived42.37秒と既存BFD57.38秒はPASSしたが、
既存Health/Derivedは **52.11秒FAIL**。logging再起動後の二度目のassertion/EventRule待ちで失敗し、
protocol配送・rule処理・容量観測にdeadline errorが出た。空きfilesystem/RAMは残っていた。
[初回失敗manifest](2026-10-24-bfd-derived/failed-initial-derived-manifest.json) とserialを保存する。

localの新testは、定常的な16件/batchの圧縮保存が **2,098,093byte** を割り当てるためFAILした。
storeごとに一つの圧縮/round trip検証workspaceを再利用し、 **467,666byte** へ減らした。
CRC・長さ・追加stream/余分なbyte・元内容との一致を確認し、Closeでworkspaceを解放する。
[割当RED](2026-10-24-bfd-derived/codec-allocation-red.log) と [関連Race14.195秒](2026-10-24-bfd-derived/codec-race.log) を参照。
これはcodec割当の測定であり、製品全体のthroughput/RSS受入ではない。

codec修正だけの896b61aはclean全体check106.63秒とnativeを通ったが、
QEMU Health/Derivedは **51.51秒FAIL** した。同じ問題を解決済みとはしない。
[二度目の失敗manifest](2026-10-24-bfd-derived/failed-codec-only-derived-manifest.json) とserialを保存する。

localでは、archive body検証を意図的に停止すると、同じstoreへの新event保存が
**1秒のcontext deadline**に達することを [新testのRED](2026-10-24-bfd-derived/read-connection-red.log) で再現した。
Listがgzip/JSON検証中も唯一のSQLite接続を保持していたため、有限なimmutable pageを先に読み、
rows/接続を返してからbodyを検証するよう修正した。新保存が進み、readerは捕捉済みの元pageを返す。
元ID・内容・CRCの検証を省かず、[event store全体Race24.008秒](2026-10-24-bfd-derived/read-connection-race.log) がPASSした。
修正後QEMU Health/Derivedは **92.09秒PASS**。guest512MiB/2vCPU/scratch64MiBと、鮮度・operation・fixtureの期限は変えていない。
同じ最終pairを実8MiBのprivate tmpfsで動かし、mandatoryなhealthy filesystem警告と
全archive/保存失敗/回復/control SIGKILLを **7.50秒PASS**。
[quota manifest](2026-10-24-bfd-derived/quota-manifest.json) とlogを保存する。

## 残作業

BFD raw packet/全遷移のdurable producer/queue・停止削除後の履歴回収は未実装。
現在のDerivedはsampled reconciliationであり、全flapのlossless履歴ではない。
他typed provider、構造化selector、Derived連鎖、旧BFD/BGP profile等の全resource契約、
privileged effects/idempotency・federation、storageの外部退避/retention/世代終了と総予算、
wall-clock変更を伴う観測fieldの述語連続性、多peerの実packet・長時間/規模/性能、
残work packageと対象実機/NICの受入を続ける。
parity flagsとwork package全体の完了状態は変更しない。

[provenance](2026-10-24-bfd-derived/provenance.json) に読んだ実装・原log SHA・変換・private PCAP/snapshotのSHAを記録する。
公開版のchecksumは `SHA256SUMS` に保存する。ELF/initramfs/diskとprivate staging詳細はcommitしない。

公開前の変更全体と新commit履歴のsecrets scanは [0件](2026-10-24-bfd-derived/secrets-scan.json)。
初回のkernel公開証明書fingerprintと既存の壊れたPEM fixture検出の分類・対応もprovenanceへ残す。
