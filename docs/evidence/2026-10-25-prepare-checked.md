# target検査を公開と同じlockへ接続するchecked prepare

2026-10-25 UTC。product `973c2df4acc87bdcb010f21d537bb37f698b36b0`、probe fixture `5f7cfff31b82f34b9762ab31bdaebcbdc4b577a9`。
CORE-02／CORE-06／DIST-01の部分実装。全計画・paired operator更新・対象実機受入は未完了。

## 実装した境界

`PrepareChecked`/`PrepareRollbackChecked`は、mutex/file lockを取得し、現在のjournalと実targetを検証した後、
callerのpreflightが成功したときだけ候補を公開する。rollbackはcallerが渡す別runtimeではなく、actual previousを渡す。
callback必須。reference/世代の不正はcallback前に拒否する。

callbackには独立したcurrent stateのdeep copyと、call中だけ借用する検証済みartifact descriptorを渡す。
入力startup/known-good参照は先にcopy/固定し、callbackのmetadata変更で公開tupleを変えない。
callbackから同Storeを再帰呼出しせず、borrowed FDをclose/保持しない。
callback失敗、nil validator、検査中cancel、stale generationでselector/watermarkを消費しない。
検査からreturnした後にもcontextを確認する。raw Prepare/PrepareRollbackは低水準APIとして残る。

このAPIは実snapshot・稼働中writer・target policyの検査を一つのtransactionへ置くための境界である。
互換性のpolicy本体、operator CLI/SSH prepare、paired loader/実tuple health confirmationを完成したとは数えない。
release packageへconfig compilerやcontrolのproduction依存は追加しない。

## 局所検証

API未実装REDと、不正pairでもtarget callbackを呼ぶ順序のREDを保存し、各実装より前にtest commitを置いた。
実signed release store、別handleのfile lock、pinned target、callbackエラー/FD close、cancel、入力変更、
深いstate view、rollbackのactual previous、generation/publicationを検査した。最終関連Race5.760秒PASS。

全体`make check build-linux RELEASE_ID=prepare-checked-v1`194.15秒、clean12.32秒、probe修正後16.53秒PASS。
unit/Race/vet/Python30、amd64/arm64各3 ELF。初回とclean、fixture変更後の6 SHAは一致する。

| native Race gate | 結果 |
| --- | --- |
| TestBuiltRuntimeCatalog | 28.81秒PASS |
| TestPublishBuiltRelease | 13.61秒PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 11.00秒PASS |
| TestDaemonConfigurationAndRecovery | 9.55秒PASS |
| TestDaemonStartupSelectionAndRecovery | 3.33秒PASS |
| TestDaemonConditionalStartupRecovery | 3.58秒PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 31.02秒PASS |

実daemon gateはroot所有0700 stage・fresh user/network namespace・checked tripletを使う。
二WANは独立peerのTCP/UDP/PCAP、切替/復旧、期限切れ、旧通知、worker復旧、confirmed rollbackを確認する。
arm64は実ELF/署名/stageまでで、実行受入ではない。

## 実signed PID1のchecked prepare

実supervisor/control/network/managementを起動し、既存の同kernel trial confirmationを観測した後、
controlのrevision1→commit2→snapshotから検査を開始する。stale expected revision3、実snapshot欠落を拒否し、
generation・configuration watermark・pendingを保持する。
成功callbackではactual previous runtimeのFDを検査し、稼働中controlのSID/revision/unconfirmed状態とsnapshot provenance/digestを照合する。
release file lockを保持したまま実controlへ新規commitを送り、HTTP503を要求する。
その後のprepared pairでは同一request再送はrevision2のACK、新規commitは409、cancel後はrevision3へcommitできること、
SIDと予約generation11の保持をwitnessにする。snapshotはrunning compilerで解決し、target互換性の証明には使わない。

最終QEMU signed PID1はhost179.28秒/guest164.89秒PASS。
`preflight_race_status=503 stale_revision=rejected missing_snapshot=rejected target_descriptor=verified`を元witnessで確認する。
raw/checked pairはboot前にcancelする。paired PID1起動・paired trial healthや実target state移行のgateではない。

## 初回probe失敗と期限の修正

初回signed QEMUはhost191.18秒/guest175.31秒FAIL。worker Readyの後、probeがconfirmationのwitnessを得られず、
`real PID 1 did not confirm the trial: context deadline exceeded`で終わった。元logに段階名がなく、
実際にどの処理で期限切れになったか、製品変更との因果は確定しない。元manifest/serial/host logを保持する。

probeはUnix API読取と、全selected ELFの再検証を同じ5秒contextで実行していた。
局所fixtureで、RPCは完了していても、より長いartifact検証がその短い期限を継承して中断することをREDで再現した。
APIは5秒のまま、artifact検証は既存probe全体125秒の残り時間を使う。host240秒・CPU/RAMは増やさない。
恒久的に遅い処理は全体期限で失敗し、worker/verified-runtimeの段階名を保存する。
局所Race1.077秒PASS。Go `./...`がtestdataを除外するため、Makeに当fixtureの明示unit/Race/vetを加えた。
この修正で元失敗の具体的stageを断定したとは扱わない。

製品を変えず、final実signed guestの6 ELF SHAも初回失敗時と一致する。
QEMU ext4二kernelはhost220.18秒/guest184.05秒、二WAN/PBRはhost97.96秒/guest84.36秒PASS。
既存runtime-only fallbackで設定/snapshot SID・revision・digest、SSH公開identity、event ID/cursorを保持する。
関連3個別gateであり、全suite一括acceptanceではない。

通常QEMUは512MiB/2CPU、storageは1024MiB/2CPU・private regular-file disk256MiB。
外部NIC/host port/host共有mountなし。host network・block device・mount・clockは変更しない。

## 残作業

実targetのABI/capability・kernel/profile/state移行・失効policy検査を、このcallbackへ実装してoperator prepareへ接続する。
callbackが成功しただけで互換性を保証したとは扱わない。現在のcommandはpaired bootを引き続き拒否する。
paired loader、exact actual tupleのcontinuous health/confirmation、conditional fallback起動policy、実signed paired PID1/kernel受入は未完了。
参照runtime破損時のfallback-before-Open、書込み中の実電源断・ENOSPC/fsync failure、reference-aware snapshot pruning、
same-revision ACK/startup切替監査、独立bootstrap、全engine/ABI/権限分離も残る。
今回のprobe timeout stageと以前のhealth false component/再発条件は未確定として保持する。
当版OCI/APK、arm64実行、全残work package、対象実機/NIC/長時間・性能の受入も未完了。
work package/parityの完了判定は変更せず、全体ゴールをactiveに保つ。

[provenance](2026-10-25-prepare-checked/provenance.json) にproduct/fixture source、元raw SHAとprivate PCAP SHAを記録する。
公開用にdeveloper path・ANSI/改行・公開kernel cert IDを正規化し、ephemeral private SSH key hashを省略する。
公開fingerprintと元raw SHAを保持し、ELF/initramfs/disk/PCAP/config snapshot本体はcommitしない。
証跡directory内の`sha256sum --check SHA256SUMS`で検査できる。
