# BGPRouterの有限な現在状態・FIBとDerivedEvent

2026-10-25 UTC。product checkpoint `8887e75d1a6702032f25801c0cb828201bfbb7c8`。
CORE-04のprovider／CORE-03の世代・鮮度境界の部分実装。全計画、旧BGP resource全体のparity、対象実機の受入は未完了。

## 実装と対応field

native BGP serviceを `BGPRouter/RESOURCE_ID.status.FIELD` から参照できる。
対応するfieldは `phase`、`establishedPeers`、`backend`、`observedAt`。
IDに `/` を含む場合も使え、inactive宣言もcompile時の型検証対象となる。
他Kind、missing provider、未対応fieldは拒否し、暗黙のstart依存を作らない。
[利用契約](../resource-program.md#derivedevent) を参照。

固定した旧BGPRouter controllerとpeer/RIB型を読み、phaseの集約を確認した。
宣言した全peerの観測から、全EstablishedならEstablished、一部ならDegraded、全非EstablishedならDown、
peer宣言なしならPendingとする。全Establishedでも未解決FIBがあればDegradedとなる。
`backend` はgobgp、`establishedPeers` は数の文字列、`observedAt` はUTC RFC3339Nano。
受理RIBの `acceptedPrefixes` を、選択済みFIB候補の件数で代用しない。

peer観測が欠ける・重複・未宣言・ASN/enum不一致なら現在値はunknownとなる。
peerの構造化表示、受理prefix/RIB・GR/再収束等の旧fieldとBGPPeer resourceの契約は残る。
raw Snapshotの表示は最後の観測であり、現在のEstablishedを保証しない。

## BOOTTIME・適用済みFIB・世代境界

workerはengine walkの**前**にboot ID、BOOTTIMEの元観測・最大5秒の期限を固定し、
遅いwalk・effect ACK・再送・表示で新しい期限へ置き換えない。
BGPのFIB投影もBOOTTIMEで判定し、UTCの巻戻しや未来のUTC表示値で元の経路所有期限を復活させない。
inactiveは元経路と期限を保持せず、同じworkerでfinal inactiveからactiveへ戻ることを拒否する。
inner epoch/sequence、時計巻戻し、同じinner sequenceでの内容変更も検証する。

最初のBGP通知からsupervisorの現在instance／module generation／digestと照合する。
初回publishのACKがworker Readyより先なので、generationの照合にReadyは要求しない。
現在値には別途、active宣言、current binding/source/worker、fresh Ready supervisorが必要である。
control再起動で受理cacheが空でも、旧workerの通知で状態をseedできない。

phaseには、同じ経路/BFD依存内容・config/worker/engine identityの投影がnetwork ACK済みであることも要求する。
変更経路が適用待ちならunknown、経路内容が同じheartbeatならACKを再利用する。
元route/BFD内容のfingerprintと、その投影で数えた未解決FIBを一緒に保持する。
status読取用にはpeer/headerだけをcacheし、全route objectを毎回再生成しない。
このfingerprintはkernel authorityではなく、既存単一writerの成功境界との照合用である。

## test-firstと失敗・修正

先にscalar集約、FIB未解決、完全peer集合、clock/期限、不正参照とinactive宣言のtestをcommitした。
[未実装API FAIL](2026-10-25-bgp-derived/status-red.log) と
[compile拒否FAIL](2026-10-25-bgp-derived/compile-red.log) を保存する。

runtime testは全peer Establishedでも未解決FIBならfalseとなること、適用前unknown、
同じ経路のheartbeatでholdを失わず、変更経路を古いACKへ結び付けないことを固定した。
[初回runtime FAIL](2026-10-25-bgp-derived/runtime-red.log) を保存する。
UTCを1時間未来にしても元の5秒で状態・実投影が失効し、Downを捏造しないこと、
同じinner観測の再送は成功して年齢を変えず、内容変更は拒否することも検証した。

旧受理経路では、現在supervisorが世代2を示しても世代1の新通知を受理した。
[世代境界RED](2026-10-25-bgp-derived/generation-red.log) で再現し、最初の通知から現在世代を照合する修正を行った。

既存fixtureはUTC期限だけを持っていたため、新しい必須boot metadataで失敗した。
[clock fixture失敗](2026-10-25-bgp-derived/fixture-clock-failure.log) を残し、元のclockを明示して更新した。
ACK証拠を変えたのにinner sequenceを進めなかったfixtureも
[FAIL](2026-10-25-bgp-derived/fixture-sequence-failure.log) を保存し、正しいsequenceへ更新した。
productの検証を緩めて通していない。

最終関連Raceはruntime14.802秒、BGP5.902秒、BGP config1.047秒、resource compile2.230秒PASS。
[関連Race](2026-10-25-bgp-derived/focused-race.log) を参照。
clean `8887e75` の全体 `make check build-linux RELEASE_ID=8887e75d1a67` は **115.99秒PASS**。
全unit・Python23・Race Detector・vet・static amd64/arm64 buildを含む。
[check](2026-10-25-bgp-derived/check-build.log) と
[開始/終了commit・dirty=false・ELF SHA](2026-10-25-bgp-derived/build-manifest.json) を保存する。

最初のQEMU Health/Derivedは **67.14秒FAIL**（child50.34秒）だった。
[失敗manifest](2026-10-25-bgp-derived/failed-health-derived-manifest.json) と
[serial](2026-10-25-bgp-derived/failed-health-derived-serial.log) を保存する。
失敗箇所は、targetの保存失敗を注入した後に「local queueの全IDはtargetに存在しない」と要求した判定だった。
targetが保存した後にlocal ACKを失うと、保存済みeventもlocal queueに残る。
そのprefixと新しい未保存eventを含むbatchが失敗した場合、両方がpendingになるのは既存のat-least-once契約どおりである。

実SQLiteで「Aをtarget保存→ACK喪失→Bをenqueue→新規INSERT失敗」を再現し、
targetのAとpendingのA/Bが同居すること、回復後にAの内容・cursorとBを保持して重複しないことを固定した。
[局所再現と既存ACK失敗試験](2026-10-25-bgp-derived/committed-prefix-reproduction.log) はRace1.741秒PASS、
[relay package全体Race](2026-10-25-bgp-derived/relay-package-race.log) は11.541秒PASS。
再現testを `2607c12`、fixture修正を `544233e` に分けてcommitした。

修正fixtureはtriggerのDDL完了を保存失敗の境界とし、直後の既存履歴を記録する。
pendingに新規未保存eventが少なくとも1件あることを要求し、そのIDが失敗中にtargetへ入らないことを確認する。
境界前から保存済みのpending prefixは元の内容・cursorを照合する。
worker/control SIGKILL後の全pendingの回収・一度だけの保存・元内容の検証も保持する。
product変更や期限/RAM/vCPUの拡大で通していない。

clean fixture `544233e` の全体check/Race/vet/Python23/両arch buildは **12.80秒PASS**（既存test cacheを含む）。
[check](2026-10-25-bgp-derived/fixture-check-build.log) と
[manifest](2026-10-25-bgp-derived/fixture-build-manifest.json) を保存する。
同じrelease IDで再buildした全4 ELFのSHAが元のchecked pairと完全一致した。
修正後nativeのHealth/Derived **13.01秒PASS**、二WAN/PBR **31.12秒PASS**。
[fixture native manifest](2026-10-25-bgp-derived/fixture-native-manifest.json) を参照。

## 実daemon・実TCP／kernelとQEMU

新fixtureもproduct前にcommitした。SHA照合済み旧95b2f43 pairは、BGPRouter参照のbootstrap拒否で
**20.33秒FAIL**。[旧pair/結果](2026-10-25-bgp-derived/before-binaries.json) と
[旧daemon excerpt](2026-10-25-bgp-derived/native-old-red-excerpt.log) を残す。

nativeはchecked amd64 pairとSHAが一致し、root所有0700 stagingとRace harnessを使う。
実supervisor/control/network/BGP workerと、別network namespaceの独立TCP BGP speakerを起動した。
最初のDown→Established、sessionを保った未解決next hopによるDegraded、解消後のEstablishedと
2回のassertを数えるdurable EventRule、実kernel routeの非作成/作成を要求する。
workerをSIGSTOPし、raw Active/EstablishedとPIDを保ったまま元期限で現在値とrouteが失効すること、
同一再送では復活せず、SIGCONT後にfresh observationで戻ることも確認した。
worker SIGKILLでidentityを更新し、旧通知409、control SIGKILL中に過去のEstablishedを復元しないことも要求する。

| gate | native | QEMU |
| --- | --- | --- |
| 新BGPRouter→Derived/EventRule・未解決FIB・expiry/replay/recovery | 22.74秒PASS | 54.19秒PASS |
| 既存BGP→IPv4/IPv6 FIB・経路所有期限 | 9.70秒PASS | 31.53秒PASS |
| BFD→Derived/EventRule・650ms hold・worker/control復旧 | 13.71秒PASS | 41.56秒PASS |
| BFD→BGP session/FIB・UDP-only loss・SIGSTOP expiry | 22.74秒PASS | 56.57秒PASS |
| Health→Derived/raw履歴・worker/queue復旧 | 13.38秒PASS、修正fixture13.01秒PASS | 修正fixture100.08秒PASS（初回67.14秒FAILを保存） |
| 二WAN Health→PBR・実TCP/UDP/PCAP・confirmed rollback | 32.13秒PASS、修正fixture31.12秒PASS | 修正fixture85.05秒PASS |

[native manifest](2026-10-25-bgp-derived/native-manifest.json) と各test logを保存する。
QEMUは同じproduct/initializer/kernel/builder、512MiB/2vCPU・外部NICなし・private64MiB scratch。
最初のBGP/BFD4 gateは初回fixture、Healthの2 gateは修正fixtureで、test binary SHAは分けて記録する。
最終Health/Derivedは100.08秒、二WAN/PBRは85.05秒PASS。
Health/Derivedでは15件の新規未保存pendingがworker/control SIGKILLを経て一度ずつ回収された。
二WAN/PBRでは実TCP/UDP・PCAP、worker/control/supervisor再起動、旧通知、期限切れとconfirmed rollbackを確認した。
[修正QEMU source/結果](2026-10-25-bgp-derived/fixture-qemu-source.json)、各manifestとserialを保存する。
鮮度・operation・child/parent期限は変更していない。release pairとは異なるtest identityのELFを記録する。
関連6 gateの再検証であり、全suiteの新aggregate合格ではない。

## 残作業

このproviderは現在のnative session/FIB観測であり、legacy BGPRouter/BGPPeer全体の復元ではない。
受理RIB/prefix/peerの全field、GR/再収束・dynamic peer/resource/profile契約、BGP raw producerとdurable queue、
他provider、構造化selector／Derived連鎖、effects/idempotency/federation、storage retention/外部退避/世代終了と総予算、
scale/long-run/性能、対象実機/NIC・clock/NTP faultと全残計画を続ける。
work package全体の完了とparity flagsは変更しない。

[provenance](2026-10-25-bgp-derived/provenance.json) に読んだ実装、原log SHA、変換、環境とprivate PCAP/snapshotのSHAを記録する。
公開artifactのSHAは `SHA256SUMS` に保存する。ELF/initramfs/PCAP/private staging詳細はcommitしない。
