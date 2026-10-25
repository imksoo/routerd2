# VRRPの元BOOTTIME所有権と実VIP期限

2026-10-25 UTC。product `c0f214bb65bfd7b7c15da9f2e76f05fa6a4950f9`、
clock fixture `521d1512c588b5578bd6fddb77c626e164b83d2f`。
CORE-03／CORE-04／HA-01の部分実装。全計画・対象実機受入は未完了。

## 所有権の経路

旧VRRP snapshotはUTCのvalid_untilから受信時の残り時間を作っていた。
UTC期限だけでVIP/VMACを取得できることを、先行テスト`0e4745c`で期待FAILとして固定した。
新snapshotは実workerが読んだkernel boot ID、CLOCK_BOOTTIMEのObservedNanos、同じbootの絶対ExpiresNanosを必須とする。
ACTIVEは設定された3 advertisement interval＋priority skew、BACKUPは最大5秒の有限観測。
valid_untilは表示用であり、所有権の認可・更新には使わない。

controlは最初の通知から現行supervisorのinstance/generation/role/config digestを照合し、元bootと観測時刻を検査する。
同じproducerのepoch/transition/観測時刻の巻戻し、同一観測の改変、INITIALIZE後のActive、
期限切れ選出を同じtransitionで更新することを拒否する。
transport sequenceだけを増やした再送でも元leaseやローカルreceiptを延長しない。
元BOOT grantを条件factへ渡し、VIPのnetwork.AddressOptions.BootLifetimeへ同じ絶対期限を渡す。
UTCへ変換して新期限を作らず、writerの再適用も元期限から残りlifetimeを計算する。

workerはACK後、正のadvertisement/neighbor announcement前にもboot期限を検査する。
停止等で期限を越えたACTIVEは同じ選出結果のまま新期限を作れない。
BACKUPはpeerが選出timerを更新し続けても1秒ごとに有限false観測を送る。
INITIALIZEはゼロ期限で最終失効し、同じproducer世代の復活を許可しない。
clock読取りが失敗したwithdrawalも、最後のboot/観測時刻を保持したゼロ期限で失効を試み、errorを残す。
AddressOwnerの実効AcceptModeをworkerとreceiverで一致させた。

UTCだけの未リリースprototype snapshotを読む互換readerは置かない。
永続journalのcodecは変更せず、保存fileから古いUp/authorityを復元しない。
peer credential/UID/capability分離の完成ではなく、既存producer envelopeと現行process-tree registryの照合である。

## 先行失敗と局所試験

UTCだけの所有権受入は3秒grantを使った2.886秒の期待FAIL。
API/clock metadata不足は0.231秒compile FAIL、VRRP用clock broker未対応は0.632秒FAIL。
先行テストは`c3f754d`、`00daaf2`、`109895e`、`a043000`、`702d62b`にも分けた。
初回focusedはerr再宣言、次はtestのBootLifetime field参照でcompile FAILした。両logを保存し、修正後focused RaceはPASS。
関連RaceはVRRP1.184秒／runtimeplan17.628秒／network29.706秒／control56.616秒PASS。
最終focusedはVRRP2.164秒／runtimeplan17.401秒／clocktest1.025秒／test-init1.021秒PASS。

元期限の直前/同時、UTC±48hの表示、異boot、過大/overflow時刻、cold controlの旧世代、終端通知後の再送、
BACKUP heartbeat、期限切れACTIVEの無広告を検査した。
開発中の実daemon Raceは10.90秒PASS。dirty sourceの診断であり、clean受入と区別して保存する。

## clean sourceでの実binary・kernel受入

`make check build-linux RELEASE_ID=vrrp-boot-v1`は287.23秒PASS。
全unit、Race Detector、vet、Python32、amd64/arm64各3 static ELFを含む。
source/command/6 SHAはbuild manifestに保持する。arm64はELF/署名/stagingの検査であり、arm64実行ではない。

| native Race gate | 結果 |
| --- | --- |
| TestBuiltRuntimeCatalog | 27.66秒 PASS |
| TestPublishBuiltRelease | 12.91秒 PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 11.00秒 PASS |
| TestDaemonConfigurationAndRecovery | 9.30秒 PASS |
| TestDaemonStartupSelectionAndRecovery | 3.48秒 PASS |
| TestDaemonConditionalStartupRecovery | 3.48秒 PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 30.96秒 PASS |
| TestDaemonVRRPOriginalOwnership | 10.75秒 PASS |
| TestIsolatedNativeVRRP | 11.71秒 PASS |

新daemon試験は実supervisor/control/network/VRRP workerを起動する。
独立peerからVIPへTCP/UDPを通し、実macvlan/VMACと広告をcaptureする。
workerとsupervisorを一時停止して通知を固定し、同じprojectionの再適用でも元期限を保つことを確認する。
controlの新規適用やsupervisorの再起動を待たず、kernelのVIPが元期限で失効し、古いprojectionの再適用でも復活しないことを確認する。
worker/controlを実SIGKILLして再取得し、旧generationの送信を拒否する。TCP/UDP復旧とforeign address保持も確認する。
別のnative二peer IPv4/IPv6試験は、実raw socketの選出・広告受信・VMAC/VIP検証・failoverを確認する。

| QEMU gate | host全体 | guest | source |
| --- | --- | --- | --- |
| vrrp（二peer IPv4/IPv6） | 21.54秒 | 17.33秒 | c0f214b |
| daemon-vrrp-clock | 45.07秒 | 33.30秒 | 521d151 |
| daemon-dhcp4-clock | 40.07秒 | 29.43秒 | 521d151 |

QEMU clock試験は固定の+24h、-24h、restoreだけを専用PID1 brokerへ要求する。
実CLOCK_REALTIME差と不変のboot IDを確認し、同じ元BOOT deadlineを再適用する。
UTCが飛んでも早期失効せず、元期限後はVIPが消え、古いprojectionでも復活しないことをTCP/UDPとkernelで確認する。
既存DHCP clock gateも再実行し、共有brokerの回帰を確認した。
clock syscallはtest-only initializerだけにあり、nonce/kernel marker/固定suiteを満たさないhost/通常processから使えない。
QEMUは512MiB/2CPU、外部NIC・host port・共有mountなし。
CPU/RAM/期限を増やさず、host/PVE VM自体のclock/network/mountを変更しない。

## 初回QEMU失敗とfixture修正

最初の新clock gateはhost26.78秒／guest11.50秒でFAILした。
隔離子execへclock socket FDが渡されず、socket operation on non-socketで製品起動前に停止した。
元manifest/serial/host logを保存し、合格に数えない。二peerの成功も別に保持した。
共有namespace fixtureがDHCP clock名だけをFD転送対象としていたため、socketpair＋実child exec試験を追加し、
VRRPの欠落を局所REDで再現した（`90319e0`）。
専用の2試験だけでFDを転送し、通常試験は転送しないよう修正した。
local Raceで両socketを子FD3で確認し、実際に1byte通信する。peer PID1確認やnonce条件は緩めていない。
この局所試験はFD継承を通常child execで再現し、namespaceを含む受入はQEMUの最終gateで行った。

fixture修正後の全体check/buildは15.42秒PASS、integration-tagのFD RaceとvetもPASS。
製品6 ELFのSHAは修正前後で全て同じ。二peer vrrpとnative9は元source、clock2件は修正fixture sourceであり、
全gateの同時・同一source実行とは主張しない。

## 残作業

当版の実daemonは無条件VRRP resourceを使う。条件付きVRRP callbackでのkernel ACK・worker停止順序、
ready_when/graceful activation、track/preempt/unicast/旧auth・動的VIP、RouterdCluster/fencingと全HA-01は未完了。
次は条件付きVRRPの実ACK順序を再現してcoordinator受入を進める。今回のclock/registry改善だけでCORE-03/04全体を完了にしない。
全protocolの有限時計・state inventory、worker UID/capability/UDS peer credential、paired runtime/config loader、
全engine/ABI、write-time電源断/ENOSPC/fsync、OCI/APK/arm64実行と全残計画を続ける。
実VM suspend/resume、対象実機/NIC/性能・長時間受入は未検証であり、決定的な期限切れ試験を実suspendの代用としない。
全体ゴールはactive。

読んだ主な実装はVRRPのtypes/machine/run/transportとwire/native試験、runtimeplanのAccept/ownership/project/condition facts、
networkのBootLifetime、clocktestとkernel initializer、namespace fixture、DHCP clock試験、実装計画/hand-off/resource contract。
[provenance](2026-10-25-vrrp-boot-authority/provenance.json)に各raw SHAと段階、private PCAP SHAを記録する。
binary/image/PCAP本体はcommitせず、公開directoryで`sha256sum --check SHA256SUMS`を実行できる。

公開前のindex全体scanは既存のcompleted output識別子14件と無効base64 PEM否定fixture1件に完全一致し、新file・credential・未分類の検出はなかった。未push履歴scanは0件。rule/fixtureの抑制変更はせず、[検査対象と結果](2026-10-25-vrrp-boot-authority/publication-scan-review.json)を記録する。
