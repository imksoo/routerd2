# 起動前payload破損からの検証済みfallback

2026-10-25 UTC。product `35c24d1adcc9fe87fa0dbf78d807e5bdf13112e2`。先行テスト `03ba761`／`9367cd9`／`6fd1a1e`／`81e0a49`／`68296b5`。
CORE-06／DIST-01の部分実装。全計画・paired loader・対象実機受入は未完了。

## 変更した起動経路

従来のapplianceはOpenで全selected payloadを検証してからBeginBootを呼んでいた。
pending candidateのELFが改変・欠落するとOpenで止まり、正常なbaselineが残っていても起動選択へ到達できなかった。
prepared／実行済みtrialの各ケースで、artifact hash変更と欠落の実FAILを先に確認した。

`release.OpenForBoot(ctx, options, kernelBootID)`はprivateなpinned directory/lock handleを開き、既存selectorのBeginBootを完了してからstoreとverified Imageを返す。
missing selectorをinstaller扱いで初期化せず、返されたimage以外を実行しない。通常のOpenやstatus/management操作は全selected payloadを検証するstrictな契約を保つ。
applianceは実kernel boot IDを読んでこのAPIを呼び、署名されたentrypoint/role catalogのdescriptor handoffと従来のhealth confirmationを続ける。

検査と変更は従来のprocess mutex＋file lock内にある。canonical state、owner-protected file、generation/checksumとactive/pending/previousの正確なsymlinkを最初に検査する。
これは署名されたjournalではなく、owned directoryとchecksum/invariantsで保護する既存journal契約である。
その後、candidate以外の全参照について、署名・全artifact hash・mode/size・native identity/ABIを検査する。
候補だけが検証に失敗した場合に限り、既知のbaselineへ戻す一つのgenerationを作る。任意の古いdirectoryやfactory executableを探して代用しない。

未試行candidateはそのruntimeへ実行を許可していないため、現在のbaseline/startupを維持し、失敗したcandidate参照と理由を記録する。
実行済みtrialは既にstateへ書いた可能性があるので、candidateの署名済みABI metadataがまだ検証でき、fallback ABIと等しいことを要求してから既存のconditional fallbackを選ぶ。
候補と旧版のformatが異なる場合、または署名trial metadataが失われた場合、推測で復旧せずselectorを変えず拒否する。

runtime high watermarkとconfiguration store/generation high watermarkは減らさない。trialではcandidate/fallback snapshotの条件付き参照を保つ。
prepared rollbackでPendingとPreviousが同じ破損payloadを指す場合、そのPrevious参照も外し、失敗履歴だけに残す。最初の実装はこの参照を残したため、局所REDを追加して修正した。
configuration自体の採用はこの関数では行わず、operator paired loader/trial fenceの完成を主張しない。

## 失敗と保存境界

selector/journal/link破損、検証できないbaseline、信頼できない実行済みtrial metadataは実行fileを返さず拒否する。
baselineのない初回候補が壊れている場合は失敗recordを保存し、ErrNoFallbackを返す。成功したrollbackや空の正常runtimeへ置き換えない。
既に確認済みactive releaseの破損には自動rollbackしない。この自動復旧policyはpending/trial updateに限定する。

書込み前のcontext cancelはselectorや世代を消費しない。journalのI/O errorを7境界へ注入し、公開前は旧record、selector交換後は新recordとErrUncertainが見えることを検査した。
同じ7境界で実subprocessをExit99し、cleanup deferを通らず再open/retryしても一つのcompleteなselectorへ戻り、追加generationや破損候補再選択を作らないことを確認した。
これはprocess exitと決定的errorの検査であり、全syscall中の物理電源断や実ENOSPC/fsync failure完了ではない。

局所ログには旧payload拒否RED、API欠落compile RED、Previous参照REDと、focused/最終focused/process-exit Raceを保持する。
実schema/署名・owned directory・state機械を使うが、unitのsynthetic ELFを実PID1起動と混同しない。

## 実binaryとkernelの検証

clean sourceの`make check build-linux RELEASE_ID=boot-recovery-v1`は273.18秒PASS。
全unit、Race Detector、vet、Python32、amd64/arm64各3 static ELF buildを含む。build manifestにsource、commandと6 SHAを記録する。
arm64は署名/ELF/stagingの検査であり、arm64上の実行受入ではない。

| native Race gate | 結果 |
| --- | --- |
| TestBuiltRuntimeCatalog | 27.71秒 PASS |
| TestPublishBuiltRelease | 13.21秒 PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 10.60秒 PASS |
| TestDaemonConfigurationAndRecovery | 9.55秒 PASS |
| TestDaemonStartupSelectionAndRecovery | 3.53秒 PASS |
| TestDaemonConditionalStartupRecovery | 3.33秒 PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 30.86秒 PASS |

| QEMU gate | host全体 | guest合計 | 結果 |
| --- | --- | --- | --- |
| release-boot | 183.94秒 | 169.50秒 | PASS |
| storage | 214.10秒 | 177.02秒 | PASS |
| storage-corrupt-trial | 210.15秒 | 174.41秒 | PASS |

全gateはproduct `35c24d1`のclean sourceから新しくbuildした実binaryを使う。
signed PID1は実entrypoint/各role inodeとhealth confirmation、実controlでのprepare fence/宣言済み6 family等を維持する。
通常ext4 gateは以前どおり1 attemptの未確認trialから次kernelのbaselineへ戻り、SSH/config/snapshot/event/identityを保持する。

新`--corrupt-trial` gateは2 attemptを許可し、最初の実kernel bootでtrial PID1を動かし、SSHでrevision2をcommitし、未確認状態でpoweroffする。
二つ目のkernelのtest-only initializerは署名されたtrialを再検証した後、entrypointの1byteを反転し、元署名metadata・size・最終modeを保つ。
実変更のartifact名、異なるbefore/after full SHA256、attempt1/max2をUARTへ出す。initializerはselectorを変えず、通常どおりunmount後に製品をexecする。
実製品がpayloadを拒否し、未消費のattemptを通常再試行せず、verified baseline PID1と全role inodeを起動することを独立probeで確認する。
残った署名metadataのABI照合、失敗record、同じSSH host fingerprint、revision2/config intent、snapshot store/digest/revision、event ID/cursorと新kernelへの再適用を確認する。
host verifierはpayload-verificationの理由、実hash差と未消費attemptを必須とし、通常attempt上限fallbackで代用できない。
valid-looking witnessでもfailed phase・wrong order・異常exit・mode違い・同一hash・欠落reportは拒否する。旧verifierのphase失敗見逃しも実REDから修正した。

各QEMUは1024MiB／2CPU、既存240秒/phaseを使う。storageは各gateごとの新規private regular256MiB disk、外部NIC/host port/共有host mountなし。
host network/clock/mount/block deviceを変更しない。これはboot間の明示payload改変であり、physical power cutや任意metadata復旧の完了ではない。
当版のnative二WAN/PBRはTCP/UDP/packet captureを検査する。private PCAP本体、disk/image/binaryやprivate keyは公開せず、provenanceにSHAを記録する。

## 残作業

operator paired prepare/loader/exact tuple health、全protocol/engine state inventoryとtarget compiler/kernel/profile・identity/lease/owner/revocation policy、独立bootstrap/worker authorityを接続する。
実行済みtrialの署名metadataが失われた場合は現在拒否する。より広い破損復旧policyや確認済みactiveの復旧は追加の資格付けが必要で、未検証の古い版へ自動実行しない。
全stateのwrite-time電源断/ENOSPC/fsync、reference-aware prune、startup/ACK監査、全engine/ABI/権限分離、OCI/APK当版、arm64実行を続ける。
34 work package全体と対象実機/NIC/性能/長時間受入は未完了。全体ゴールをactiveに保ち、完了flagやparityは変更しない。

読んだ主な実装・契約は `internal/release/{store_linux,state_linux,runtime_abi_linux,configuration}.go`、既存state/paired/crash/ABI試験、`cmd/routerd/release_boot.go`、`images/storage-test/guest/`、`scripts/qemu-storage.py`とfixture README、release contract、実装計画/hand-off/ADR0001。
[provenance](2026-10-25-boot-recovery/provenance.json)に元raw SHA・正規化とprivate PCAP SHAを保持する。公開directoryで`sha256sum --check SHA256SUMS`を実行できる。

公開前にindex全体と未push履歴をgitleaksで検査した。indexの15件は既存のcompleted output識別子14件と無効base64 PEM否定fixture1件に完全一致し、当版の新fileには検出なし。履歴scanは0件。rule/fixtureを抑制変更せず、[scan結果と対象tree/history](2026-10-25-boot-recovery/publication-scan-review.json)に記録した。
