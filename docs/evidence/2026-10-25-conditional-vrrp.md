# 条件付きVRRPのkernel ACKと依存workerの停止順序

2026-10-25 UTC。所有権ACK実装`542c428`、VRRP-owned interfaceの依存検査とcompiler contract更新`62bc99b`。
CORE-03／CORE-04／HA-01の部分実装。全計画・対象実機受入は未完了。

## 変更した契約

条件を一つでも含むsourceでは、旧AcceptがVRRP通知を記録した直後に成功を返していた。
実workerは成功後にkernelのVIP/VMACを検査して広告するため、未適用の成功応答が起動失敗を引き起こし得る。
先行テストで、VIP適用前の成功、依存worker終了前の撤去ACK、適用失敗や旧世代への成功応答を再現した。

VRRP producerごとに一つのpending effectを持ち、callbackはmuを解放して待つ。
callbackはmodule-set lockを取得せず、workerの起動・停止を行わない。
serialized coordinatorがnetwork workerの適用ACKを得てから完了させる。
同じsequence/bodyの再送もpending ACKを待ち、元boot IDと絶対所有期限を延長しない。
適用直前に現行registryとgeneration/config digest/source epochを照合し、退役したpending producerを投影へ入れない。
ACTIVE成功には、VMACと全VIP、元BootLifetimeが一致し、元所有期限が現在も有効であることを要求する。
実workerはその後に既存のnetlink確認を行う。これはnetwork ACKとworker確認を結ぶ処理であり、全workerの同時snapshotではない。

初回Ready待ち・最終終了待ちもpending effectを処理する。
依存workerの停止要求だけではVIPを撤去せず、実際の終了まで待つ。
自身のinactive ACKを待つVRRP producerだけは、ACK前に終了することを要求しない。
待機中の処理は停止とnetwork適用だけを行い、新しい依存workerを起動しない。
更新による上書き、期限切れ、generation変更、source切替、呼出しcancel、network errorを成功として返さない。
撤去失敗時にも新しいinactive観測を保持し、古いACTIVEを復元しない。元kernel期限は継続する。

## 動的interfaceの依存検査

初回の実daemon試験は、依存serviceを追加する段階でHTTP400になった。
グラフにdepends_onとACTIVE条件があっても、旧native検証器はVRRP所有interfaceの使用を一律に拒否していた。
この拒否をpure compilerで再現し、consumerからVRRP resourceへの直接depends_on/ownerと、
consumer.whenまたはowner.ready_whenが正のservice.<name>.activeを必須にする場合だけ許可した。
三値論理のall/any/notを検査し、無関係なOR分岐や否定されたactiveだけでは許可しない。
BACKUPのreadyはVIP所有を意味しない。static objectと他protocolのdynamic interface制限は維持する。

新しいsource受入範囲を旧compilerと同一と宣言しないため、署名付きconfiguration contractを
resource-program-native-2へ変更した。JSON schemaと永続codecは変更しない。
旧native-1とのprepare/boot/rollback/fallbackは既存ABI検査で拒否する。
新旧compilerのmigrationやpaired operator loaderを完成させたものではない。

## 失敗を保持した局所検証

- 元sourceのclean test-only checkoutで、早期ACK・停止順序・失敗/旧世代/期限切れ/cancel/上書きの拒否を期待FAILとして固定した。
- 実装中の旧世代検査は成功ACKを拒否してもVIPを適用しており、追加テストがFAILした。投影前のregistry fenceへ修正した。
- 初回全体checkは再送テストがenqueue前にactorを進めるためFAIL。pending enqueueを待つよう修正し、局所Race20回に合格した。
- 最初の実daemon試験は動的interfaceのcompiler拒否でFAIL。pure compiler REDから依存検査を実装した。
- ABI試験の初回はFacts型の誤指定によるcompile FAIL。また旧否定fixtureのnative-2が新Currentと一致してFAILした。完全なformat factsと異なるcontract fixtureへ修正した。
- consumerのdependency/owner、正負条件、all/any/not、二重否定、De Morgan形、owner readiness、network error、重複、撤去失敗後の再送、cancel、上書き、旧generation、元期限を局所試験で確認した。

## clean productの受入

`make check build-linux RELEASE_ID=conditional-vrrp-ack-v1`は依存検査込み284.16秒PASS。
clean source `62bc99b`での最終check/buildは15.01秒PASS、6 ELFのSHAは一致した。
全unit/Race/vet、Python33とamd64/arm64各3 static ELFを含む。

| native Race gate | 結果 |
| --- | --- |
| TestDaemonVRRPConditionalOwnership | 14.16秒 PASS |
| TestBuiltRuntimeCatalog | 26.86秒 PASS |
| TestPublishBuiltRelease | 13.01秒 PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 10.80秒 PASS |
| TestDaemonConfigurationAndRecovery | 9.60秒 PASS |
| TestDaemonStartupSelectionAndRecovery | 3.28秒 PASS |
| TestDaemonConditionalStartupRecovery | 3.33秒 PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 30.92秒 PASS |
| TestDaemonVRRPOriginalOwnership | 10.50秒 PASS |
| TestIsolatedNativeVRRP | 11.71秒 PASS |

| QEMU gate | host全体 | guest |
| --- | --- | --- |
| daemon-vrrp-conditional | 50.81秒 PASS | 37.44秒 |
| daemon-health-pbr | 91.60秒 PASS | 81.13秒 |
| daemon-vrrp-clock | 40.96秒 PASS | 30.44秒 |
| release-boot | 175.68秒 PASS | 161.33秒 |
| storage | 212.09秒 PASS | 175.41秒 |


native最初のcampaignは7件PASS後、Health/PBRの証跡保存でENOENTとなった。
private runnerを試験ごとの出力先に分けた際、そのdirectoryを作っていなかった。
局所file writeでRED/作成後GREENを確認し、製品・oracle・期限を変更せず必要directoryを準備した。
既存PASSを再走せず、失敗したHealth/PBRと未実行2件を同じ6 ELF/4 Race harnessで実行した。
10件の合格結果は二つのcampaignを集約したものであり、初回全件連続PASSとは扱わない。

条件付きVRRPの実daemon試験は、実supervisor/control/network/VRRP workerと独立peerを使う。
元BOOT期限・同じprojection再適用・実TCP/UDP・VMAC広告capture・期限後の失効・worker/control再起動・旧世代拒否を確認する。
さらにACTIVE中にha4 macvlanを使用する実DHCPv4 client workerを追加する。
consumerをSIGSTOPしてconfig撤去を要求し、supervisorがstoppingを表示する間も実VIPが存在し、commitが終了していないことを確認する。
SIGCONT後にworker終了、VIP撤去、commit完了、TCP/UDP不達を確認する。
このclientにはleaseを配布していない。HA上のDHCP server/RA配信の完全受入を代替しない。

QEMUは外部NIC・host port・共有mountなし、512MiB/2CPUと既存期限を使う。
新しい条件付きsuiteをqemu-protocolsへ追加した。時計補正は専用guest-only brokerの固定操作だけで、host/PVE clockを変更しない。
署名付きPID1とext4二kernel gateは両payloadが新native-2を宣言する同一契約の受入であり、native-1からの移行成功ではない。
arm64はstatic ELF/署名/staging検査であり、実行を検証していない。

## 残作業

- ready_whenに基づく広告開始・graceful activation、全track/cluster/fencingとHA-01全体。
- supervisor停止や依存worker自身の障害に対するwatchdog/recipient lease、UID/GID/capability/UDS peer認可、全protocol時計とtyped status。
- より一般的なdynamic interface/resource依存、CORE-03/04全体、operator paired prepare/loader/tuple healthと全state/engine/更新復旧policy。
- 全34作業単位の完了判定、現行OCI/APKとarm64実行、実suspend/resume、対象機種/NIC/性能/長時間受入。

実装済み・このsourceで検証済み・未検証を分け、semantic parityやwork packageを完了へ上げていない。
正規化済みログ・source/6 SHA・個別結果は[証跡directory](2026-10-25-conditional-vrrp/)に保存し、PCAP本体はprivate側で保持してhashを記録する。
