# 採用と共有するread-only起動入力検査

2026-10-25 UTC。product `28c98cae3e44baed3ec1cfb17f48c928d7806022`、先行テスト `346d13f`。
CORE-02／DIST-01の部分実装。全計画・signed paired boot・対象実機受入は未完了。

## 実装

`config.Store.InspectStartup`は、既存journalと選択snapshotを検査し、現在record・snapshot・既採用かどうか・条件付き復旧の分岐を返す。
identity、revision、generation、confirmation、outbox容量、snapshot SHA/provenance/canonical JSON、current compilerでの文書検査を、実採用`AdoptStartup`と共有する。
検査はrecord/event/選択を書かず、directoryを作らず、fsyncせず、世代を消費しない。返値は別のwriteの認可tokenではない。
採用は毎回再検査するため、検査後の別commitを古い観測で上書きできない。

explicit選択はowned0700のstate directoryを一度開き、その同じdescriptorからjournalとsnapshot directoryを開く。
`config.json`はowned0600・regular・単一link・正の有限sizeを要求する。symlinkを追わず、FIFOはnonblocking open後に型で拒否する。
これはexplicit startup検査/採用/replayの境界であり、一般の全Load/Save経路やancestor directory全体の保護を完了したとは扱わない。

未採用candidateにはexact expected revisionと未確認commitなし、復旧前にはprepared baselineの文書一致、復旧後にはexact failed receiptと後続commitなしを要求する。
既採用selectionは古いsnapshotを再読せず、現在の後続commit/confirmationと元のrecovered分岐を返す。snapshot返値のversion0は、この既採用分岐で未読の意味であり、実file不在を観測したstate factとして利用しない。
再採用のdurability syncは従来どおり採用側だけが行う。

`InspectStartup`自身はwriter lockを取得しない。呼出元はengine mutex→release lockの順を守り、独立writerを除外したまま検査とprepare判断を終える必要がある。
実release-bound controlのwriter fenceと`PrepareChecked`を使うfixtureへcandidateとfallback両検査を接続した。
operator CLI/APIによるpaired prepareやloaderはまだ提供しない。実検査対象はcurrent compilerと設定の二入力であり、incoming compilerや全state familyの適合性ではない。

## 先行失敗と局所検証

先行テストは未実装APIのcompile FAILを固定した。採用処理だけを調べるoverlayでは、新API試験だけを除外し、旧実装がsymlink/hardlink/0644 journalを採用するFAILを確認した。
FIFOは旧readerを無期限にblockさせないためこのRED実行の対象から外した。修正後はFIFOも必須試験で実行しPASSした。

最初の関連Raceではconfig32.230秒、control58.154秒、guest fixture1.098秒PASS、release124.401秒FAIL。
追加試験のrelease ID `inspection-r2`が既存toy ELF patch fixtureの等長条件を満たさず、実検査へ到達していなかった。
既存の等長`fixture-r2`へ修正し、検証条件は弱めない。修正後focused Raceはconfig2.103秒、release8.168秒PASS。

候補/fallback欠落、baseline不一致、revision更新、古い世代、別store、未確認commit、outbox満杯、改変snapshot、unsafe journalを拒否する。
検査前後のfile内容hash・inode・mode・size・mtimeとtreeの一致で書込みなしを確認する（読取りに伴うatimeは対象外）。
実signed release storeでは、両入力の検査成功時だけprepareし、失敗時はselector/watermarkを変更せず、設定journalも書き換えない。
条件付き復旧はcandidate採用前後と既採用replay、snapshot削除後の後続の未確認commit保持を検査する。
検査後に別commitした場合はAdoptStartupがrevisionを再検査して拒否する。

## 当版の実binaryとkernel検証

clean worktreeの`make check build-linux RELEASE_ID=startup-inspection-v1`は220.58秒PASS。
format、全unit、Race Detector、vet、Python30とstatic amd64/arm64各3 ELF buildを含む。build manifestはsourceと6 SHAを記録する。
arm64は署名/ELF/staging検査であり、実行したとは扱わない。

| native Race gate | 結果 |
| --- | --- |
| TestBuiltRuntimeCatalog | 27.31秒 PASS |
| TestPublishBuiltRelease | 13.21秒 PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 10.95秒 PASS |
| TestDaemonConfigurationAndRecovery | 9.3秒 PASS |
| TestDaemonStartupSelectionAndRecovery | 3.33秒 PASS |
| TestDaemonConditionalStartupRecovery | 3.53秒 PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 31.16秒 PASS |

QEMU signed PID1はhost168.81秒/guest154.25秒PASS。
実controlでpreflight中503、stale revision/実snapshot欠落拒否、prepare後の新commit409・同一再送ACK・cancel後commit再開を確認する。
`candidate_inspected=true fallback_inspected=true`のwitnessを要求する。pairはboot前にcancelし、paired起動/healthの証明とは数えない。
永続ext4二kernelはhost213.04秒/guest178.37秒PASS。
二WAN/PBRはhost97.21秒/guest83.75秒PASS。
実TCP/UDP/packet captureでHealth切替/復旧、worker再起動、古い通知、期限切れ、confirmed rollbackを検査する。

QEMUは外部NIC/host port/共有host mountなし。signed PID1/storageは1024MiB/2CPU、通常daemonは512MiB/2CPU、storageはprivate regular disk256MiB。
CPU/RAM/deadlineを増やさない。host network/mount/clock/block deviceを変更しない。

## 残作業

target自身のcompiler・実state/absence inventoryと全reader/writer、kernel/engine/profile・owner/identity/lease/revocation policy、qualified format migrationを接続する。
新しいinspectionをABI facts全体や全preflight policyが完成したものと扱わない。
operator paired prepare、paired loader、exact tuple continuous health/confirmation、bounded conditional fallback、signed paired PID1/kernel gateが必要。
参照runtime破損時のfallback-before-Open、write-time電源断/ENOSPC/fsync、reference-aware snapshot pruning、same-revision ACK/startup切替監査も継続する。
独立bootstrap、全engine/ABI/権限分離、当版OCI/APKとarm64実行、全残work package、対象実機/NIC/性能・長時間受入は未完了。
前版のQEMU timeoutの詳細寄与とhealth false component/再発条件も未確定のまま保持する。work package/parity/全体完了flagは変更しない。

[provenance](2026-10-25-startup-inspection/provenance.json) は元raw SHAとprivate PCAP SHAを保持する。
developer path、ANSI/改行、公開kernel cert IDを正規化し、ephemeral private SSH key hashを省略する。公開fingerprintは保持する。
ELF/initramfs/disk/PCAP/config snapshot本体はcommitしない。証跡directory内で`sha256sum --check SHA256SUMS`を実行できる。
