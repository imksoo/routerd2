# DHCPv4の元BOOT期限・runtimeとnetwork writer

2026-10-25 UTC。product checkpoint `a136ae8e9af273f443d22b3fb1f7d045d56be674`。
fixture checkpoint `66475eb416fe3b70322c5c6ebb96ecfaf873411a`。
CORE-03／NET-05の期限・世代境界を進めた。全計画、DHCP resource全体、対象実機の受入は未完了。

## 元の観測とACKの期限

DHCPv4 Snapshotにboot ID・BOOTTIMEの元観測、論理epoch/sequenceとACKごとのLeaseClockを保持する。
LeaseClockは元の取得・T1/T2・失効BOOT値と、ACKを作ったepoch/sequenceを持つ。
UTC表示の相対intervalと一致することを検証し、UTCの現在値や再送の受信時刻から寿命を作り直さない。
publish前の永続化・effect ACKが遅れても元の期限を維持し、ACK完了時に失効済みならworkerは停止する。

transport sequenceを進めた同一再送は許すが、元の観測番号で内容を変えたり、古い観測番号へ戻したりできない。
同じBOOT値で発生した別の観測は、論理sequenceで区別する。
同じleaseでaddress/server/options等を差し替えず、新しいACKだけがlease identityを更新する。
停止済みのworkerは同じincarnationでactiveへ戻れない。
最初のDHCP通知から、supervisor instance・現在module generation/role/digestと照合する。
初回publishはReadyより前なので、世代照合そのものにReadyは要求しない。

active factの元観測はACKの取得BOOTであり、T1/T2 phase表示やGET/再送でmax_ageを更新しない。
同じbootの保存hintは元BOOT期限を使い、別bootのhintは非active候補のままfresh ACKを要求する。
古いworkerの高いlease counterは、そのepochとともに保存され、新しいworkerのfresh ACKを誤って排除しない。

## network writerと所有権

BOOTTIME readerを `internal/boottime` へ取り出し、従来のBFD readerと同じimmutable boot ID cacheを共有した。
runtimeは元BOOT期限でDHCPv4 address/default routeを投影する。
network writerへはboot IDと絶対preferred/valid BOOT期限を渡し、kernelへ書く直前に残り秒数を求める。
UTC deadlineへの再変換をしないため、writer再起動・replayでも元の期限と投影identityを保持する。
旧UTC fieldとBOOT fieldの混在、不正boot/期限を拒否し、operatorのAddress resourceから内部 `boot_lifetime` を指定できない。

network ownership journalは **2**。未公開1は内容を書き換えず拒否する。
旧binaryが新しい寿命情報を無視してstatic lifetimeとして読むことを避けるためで、prototype1→2の移行や混在rollbackは受け入れていない。
現在bootの同じjournalを開き直して残寿命を照合し、外から長くされたaddressを採用・削除しない。
controllerが動かない間のIPv4 **address** 自律失効を実kernelで確認する。
IPv4 **default route** の撤去はlive coordinatorのreconcileが必要で、全controller停止中の自律route失効はこの実装の保証ではない。

## test-first・失敗と修正

`7334b55` にBOOT/UTC独立入力の投影、元age、lease payload、世代境界、operator拒否とwriter所有権を先に固定した。
[未実装API RED](2026-10-25-dhcp4-authority/api-red.log)、
[旧workerがcacheをseedしたRED](2026-10-25-dhcp4-authority/generation-red.log)、
[旧journalを受け入れたRED](2026-10-25-dhcp4-authority/journal-red.log) を保存する。
実daemonと実kernelのfixture、epoch/counterと拒否理由のtestもproduct前にcommitした。

初回のclock抽出には[未使用import](2026-10-25-dhcp4-authority/first-wiring-failure.log) があった。
既存fixtureは[必須BOOT情報・実際のworker世代](2026-10-25-dhcp4-authority/required-fixture-failure.log) を持っていなかったため更新した。
[fixture import失敗](2026-10-25-dhcp4-authority/fixture-wiring-failure.log) と
[過去UTCにした通知／UTC lifetime照合の失敗](2026-10-25-dhcp4-authority/former-utc-fixture-failure.log) も保存する。
元観測を逆行させるexpiry fixtureを、実workerが出す現在のinactive/Expired観測へ変更し、cutoverでは元BOOT期限を照合した。
productの世代・時計検証を緩めて通していない。

空cacheの追加testも、最初は別のstate/runtime directoryを作ってしまい、config digestが違うため正しくErrStaleになった。
[失敗](2026-10-25-dhcp4-authority/cold-cache-fixture-failure.log) を残し、同じpath・source・revisionで受理cacheだけを新しくした
[Race試験](2026-10-25-dhcp4-authority/cold-cache-race.log) は3.186秒PASS。
これは受理処理のcomponent試験である。実control起動は動的workerを一度quiesceし、新世代でprotocolを再取得する別の境界である。

関連RaceはDHCPv4 **1.043秒**・resource compile **2.249秒**・runtime **17.250秒** PASS。
shared clock/BFD/networkもPASS（cacheを含む）。[関連Race](2026-10-25-dhcp4-authority/focused-race.log) と
[operator／旧journalの明示拒否Race](2026-10-25-dhcp4-authority/explicit-rejection-race.log) を保存する。
clean `a136ae8` の全体check/Race/vet/Python23/static両arch buildは **128.43秒PASS**。
[check](2026-10-25-dhcp4-authority/check-build.log) と
[開始/終了commit・dirty=false・4 ELF SHA](2026-10-25-dhcp4-authority/build-manifest.json) を保存する。

## native fixtureの判定修正

最初のnativeは実kernel lifetime **8.80秒PASS** の後、daemonが **10.55秒FAIL**（Go test10.48秒） した。
[初回manifest](2026-10-25-dhcp4-authority/initial-native-manifest.json) と
[失敗excerpt](2026-10-25-dhcp4-authority/heartbeat-fixture-failure.log) を保存する。
8秒leaseをSIGSTOP中に失効させてReady維持を要求したが、既存の5秒heartbeat timeoutが先に作動するため、このfixtureの要求は成り立たなかった。
captureのcleanupもdaemon shutdownより遅く、interface-downをcapture errorとしていた。
leaseを3秒（T1=1/T2=2）にし、captureをdaemon停止より前に終了させるよう修正した。
製品のheartbeat/operation期限、RAM/vCPUを増やしていない。

修正後nativeも **8.74秒FAIL**。
[manifest](2026-10-25-dhcp4-authority/first-fixture-native-manifest.json) と
[失敗excerpt](2026-10-25-dhcp4-authority/control-generation-fixture-failure.log) を保存する。
control復旧は `Runtime.ApplyConfig` で旧dynamic workerをquiesceする設計なのに、同じ世代の存続を要求したことが原因だった。
最終fixtureはACKを遮断してcontrolをSIGKILLし、新しいDHCP世代・旧通知409・fresh ACK前の非activeを要求する。
timeoutを伸ばす修正やproduct変更は行っていない。

fixture4173a15のclean全体check/buildは **39.06秒PASS**、最終fixture66475ebは **7.41秒PASS**（既存test cache含む）。
[最初のfixture check](2026-10-25-dhcp4-authority/first-fixture-check-build.log)、
[最終check](2026-10-25-dhcp4-authority/fixture-check-build.log) と
[最終manifest](2026-10-25-dhcp4-authority/fixture-build-manifest.json) を保存する。
同じrelease IDで再buildした4 ELFすべてのSHAが最初のchecked productと完全一致した。

## 実daemon・実packet・QEMU

SHA照合済み旧fa5792d pairは、独立serverから実leaseを取得した後、元BOOT情報がないことで **0.62秒FAIL**。
[旧pair/実行結果](2026-10-25-dhcp4-authority/old-daemon-red.json) と
[旧daemon RED](2026-10-25-dhcp4-authority/old-daemon-red.log) を保存する。
root所有0700 stage、fresh user/network namespaceとRace harnessを使い、host networkは変更していない。

新daemon fixtureは実supervisor/control/network/DHCPv4 workerと独立UDP DHCP serverを起動する。
kernel address/default routeと、lease addressを実際にsourceとして使うTCP/UDPを確認する。
更新ACK、Ready/PIDを保ったSIGSTOP中の元期限失効、同じ通知の再送、worker SIGKILLと旧通知409、
control SIGKILLによる新世代・保存hintの非active・fresh ACKでの復旧を要求する。
AF_PACKETでDHCPとTCP/UDPの実frameを検査し、foreign static addressを全transitionで保持する。

実kernel fixtureはBOOT期限6秒のIPv4 addressを作り、writer/journalを開き直して寿命を更新しないこと、
writerを閉じている間の自律失効、期限切れintentの再適用で復活しないことを確認する。

| gate | native | QEMU |
| --- | --- | --- |
| kernel addressのBOOT期限・writer reopen・自律失効 | 8.80秒PASS（初回checked product） | 20.79秒PASS |
| 実DHCP worker→address/default/TCP/UDP/PCAP・期限/復旧 | 10.35秒PASS | 39.75秒PASS |
| DHCPv4/IPv6-PD/RAの実packet | 18.83秒PASS | 31.02秒PASS |
| native DHCP→DNS | 10.76秒PASS | 32.53秒PASS |
| migrated resource DHCP→DNS | 10.65秒PASS | 33.53秒PASS |
| 既存network kernel forwarding/NAT/DS-Lite等 | 21.74秒PASS | 39.04秒PASS |
| BFD→Derived/EventRule | 13.86秒PASS | 44.77秒PASS |
| BFD→BGP session/FIB | 23.44秒PASS | 56.40秒PASS |
| 二WAN Health→PBR・TCP/UDP/PCAP・confirmed rollback | 30.91秒PASS | 97.13秒PASS |

[native manifest](2026-10-25-dhcp4-authority/native-manifest.json)、
[QEMU source/結果](2026-10-25-dhcp4-authority/qemu-source.json)、各manifestとserialを保存する。
最初のkernel gateはproduct a136ae8、他8 gateは修正fixture66475ebのharnessで、製品ELFは同一SHAである。
QEMUはTCG512MiB/2CPU・外部NICなし・private64MiB scratch。protocol/network/daemonは別harnessである。
新しいdaemon fixtureには180秒child/210秒parentを設定した。既存gateの期限は変更していない。
関連9 gateの個別回帰であり、全suite aggregate、実vendor serverとのinterop、物理suspend/NTP受入ではない。

## 残作業

DHCP current provider、他protocolの期限境界、IA_NA・DHCP resource field・server/relay/DAD/release、
IPv4 routeの全controller停止時の契約、raw producer durability、storage retention/外部退避、effects/federation等と全残計画を続ける。
実際のCLOCK_REALTIME/NTP補正、物理suspendと対象実機/NIC・長時間/性能は未検証。
work package全体の完了とparity flagsは変更しない。

[provenance](2026-10-25-dhcp4-authority/provenance.json) に読んだ実装、元log SHA、変換、環境とprivate PCAPのhashを記録する。
公開artifactのhashは `SHA256SUMS` に保存する。ELF/initramfs・private staging・PCAP本体はcommitしない。
