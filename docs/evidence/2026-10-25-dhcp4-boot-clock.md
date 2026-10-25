# DHCPv4 clientのBOOTTIMEタイマー

2026-10-25 UTC。product checkpoint `fa5792dbeee7f2abf23f9245751b5a37a3a6cb2d`。
CORE-03／NET-05に関わるclient側の修正であり、全計画・DHCP resource全体・対象実機の受入は未完了。

## 実装した境界

有効なserver ACKで、T1・T2・lease expiryを一度だけ現在bootのBOOTTIMEから決める。
後続のUTC補正・表示・tick・再送で作り直さず、更新ACKでだけ新しいlease期限を確定する。
BOOTTIMEにはsuspend中の経過も含まれる。transaction timeout・randomized retry・DHCP packetの `secs` も同じ時計を使う。
lease失効後のqueued ACKは、期限切れをwithdrawして新しいDISCOVER transactionを始めた後に捨てる。
clock読取失敗・boot ID変更・逆行・加算overflowでUTCへfallbackせず、workerを停止する。

UTCのacquired/renew/rebind/expiry表示値とSnapshotのwire formatは変更していない。
既存のMAC/client identity付きINIT-REBOOT保存hintを保ち、fresh ACK前にactiveにしない。
再起動後のhintはUTCで候補を選び、起動時に一度決めた有限の待機と60秒のtransaction期限で扱う。
期限切れ後の古いACKで保存hintを現在のleaseとして復活させない。

**まだ未実装:** 元のboot期限をSnapshotからruntime・fact・network writerへ伝える経路。
現在のcoordinatorはDHCPv4 `expiresAt` とaddress lifetimeにUTCを使う。
したがって、workerが停止した状態やcontrol再起動を含むend-to-endの時計補正安全性は、この修正では達成していない。
次にこの境界を実装し、UTCを読み直して新しい寿命に変換するだけの対処を避ける。

## test-firstと旧処理の失敗

`c49ae3e` に独立BOOT／UTC入力でのT1/T2/expiry・更新ACK・reboot待機・clock failureを先に固定した。
未実装APIでの[compile RED](2026-10-25-dhcp4-boot-clock/api-red.log) を保存する。
`5c5d06b` には停止後ACK・retry／packet secs・overflowと既存fixtureの明示clockを追加した。
product実装はその後の `fa5792d` である。

旧 `65a01b6` のtimer logicを別の一時directoryへ取り出し、時計入力だけのadapterを加えて再現した。
新clock fieldを宣言し、testのUTC値を旧 `c.now` に渡しただけで、旧期限判定は変更していない。
[adapter](2026-10-25-dhcp4-boot-clock/before-clock-adapter.patch)、
[scope/command](2026-10-25-dhcp4-boot-clock/before-behavior.json)、
[behavior RED](2026-10-25-dhcp4-boot-clock/before-behavior-red.log) を保存する。
これはpure timer testであり、旧daemon ELFの試験や実host時計の変更ではない。

旧処理は、UTCを24時間戻すとboot+40秒でもBoundのままでT1を無視し、24時間進めるとboot+39秒で早期失効した。
更新ACK・reboot待機・失効後ACK・retryでもFAILを確認した。
ただし、このadapterはUTCだけのfake `time.Time` を旧処理へ渡す。
旧Runの `now=time.Now` は、retry・transaction timeout・packet secsにはGoのmonotonic情報を保持していた。
それらが通常のUTC補正で壊れるproduction bugを、このpure REDが証明したとは数えない。
leaseは `AcquiredAt` をUTCへ変換して保存するため、T1/T2/expiryのUTC依存とは事情が異なる。
retry等をBOOTTIMEへ揃えた理由はsuspend経過も含む共通時計とclock故障のfenceであり、実suspendは未試験である。
修正後は、同じBOOT進行に対して前後24時間のUTC補正を与えてT1/T2/expiryを保持し、
更新ACKだけが新期限を作り、失効後ACKが現在値を復活させないことを要求する。
package全体の[Race試験](2026-10-25-dhcp4-boot-clock/component-race.log) は **1.042秒PASS**。

## 全体checkと検証環境の修正

最初の `make check` は **23.34秒FAIL**。
前回作業で私が作った秘密情報scan用の不完全なsource copyが `dist` 配下にあり、Goの `./...` 探索対象に入っていた。
製品packageの失敗ではない。[失敗check excerpt](2026-10-25-dhcp4-boot-clock/first-check-failure-excerpt.log) と
[失敗manifest](2026-10-25-dhcp4-boot-clock/first-check-failure.json) を保存する。
失敗manifest内のELFは前回buildの残存物で、この失敗runでbuildできた証拠ではない。

二つのprivate scan directoryを、内容を保ったままGoが探索しない `_` 始まりの名前へ移した。
evidenceやsourceを削除せず、以後のscanもこの配置を使う。
build完了前のnative launcherもmanifest未生成で停止したため、
[launcher log](2026-10-25-dhcp4-boot-clock/premature-native-launch.log) を保存する。
この試行は試験を起動しておらず、daemon／packet testのFAILとして数えない。

clean `fa5792d` の `make check build-linux RELEASE_ID=fa5792dbeee7` は **96.59秒PASS**。
全unit・Python23・Race Detector・vet・static amd64/arm64 buildを含む。
[check](2026-10-25-dhcp4-boot-clock/check-build.log) と
[開始/終了commit・dirty=false・4 ELF SHA](2026-10-25-dhcp4-boot-clock/build-manifest.json) を保存する。

## 実packet・実daemonとQEMU

root所有0700の一時stageへ、checked amd64 pairと二つのRace harnessを配置した。
hostのnetworkを変更せず、fresh user/network namespaceのvethと独立UDP serverを使う。
DHCPv4のAF_PACKET受信・checksum offload・取得/更新/期限切れ、既存DHCPv6 PD/informationとRAも確認する。
別のdaemon fixtureでは、実supervisor/control/network/DHCP-server/DNS workerと実DHCP clientを起動し、
lease→DNS、所有期限、worker復旧と旧resourceからのmigration経路を確認する。

| gate | native | QEMU |
| --- | --- | --- |
| DHCPv4/IPv6-PD/RAの実packet | 18.94秒PASS | 30.22秒PASS |
| native DHCP→DNS・所有期限/復旧 | 10.70秒PASS | 31.56秒PASS |
| migrated resource DHCP→DNS・所有期限/復旧 | 10.90秒PASS | 32.36秒PASS |

[native manifest](2026-10-25-dhcp4-boot-clock/native-manifest.json)、
[QEMU source/結果](2026-10-25-dhcp4-boot-clock/qemu-source.json)、各manifestとlogを保存する。
QEMUはTCG512MiB/2CPU・外部NICなし・private64MiB scratchで、既存fixture/operation期限を変更しない。
protocol suiteとdaemon suiteは別harnessである。daemonのtest identityはrelease identityと区別する。
関連3 gateの個別回帰であり、全suite aggregate、実vendor serverとのinteropや実機clock/suspend受入ではない。

## 残作業

次はDHCPv4元BOOT期限のsnapshot／coordinator／network writerへの伝搬、重複・遅延通知・worker/control停止時の失効と復旧を進める。
DHCPv6等の時計境界、DHCP current providerとstructured selectors、IA_NA・resource field全体、server/relay/DAD/releaseも別の残作業である。
既存BGP/BFD・event durability/retention・effects/federation・全残work packageと対象実機/NIC/性能・長時間試験の完了状態は変更しない。

[provenance](2026-10-25-dhcp4-boot-clock/provenance.json) に読んだcode、元log SHA・変換・環境とscopeを記録する。
公開artifactのhashは `SHA256SUMS` に保存する。ELF/initramfs・private stageや実packet artifact本体はcommitしない。
