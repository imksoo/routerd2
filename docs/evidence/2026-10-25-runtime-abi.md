# manifest v3とnative ABI宣言の結び付け

2026-10-25 UTC。最終product `3331b81a056a2d03c5d577a48bcdb2b4d50da7d9`、memo前product `7b4a070`。
CORE-02／CORE-06／DIST-01の部分実装。全計画・paired更新・対象実機受入は未完了。

## 実装した境界

native ELFのreadonly `.rodata`から一つのcanonical `ROUTERD2-RUNTIME-ABI-V1:...:END`を取得する。
publisherは入力の実宣言を導出し、native全memberの一致を要求する。layoutに宣言overrideは置かない。
manifest v3の署名domainは`routerd2 signed extracted runtime manifest v3\x00`。
受信側は署名・全payload hashの後で、命令identityとABIがmanifestに一致することを検査する。
実行中supervisorの宣言も選択manifestと照合する。旧v1/v2 readerはなく、release identityのV1とは別versionである。

宣言は4KiB、state component32、sorted/unique name、reader/writer version1..65535へ制限する。
writerは自分のreader範囲内。scanはbounded overlapでstreamし、複数valid record・過剰marker候補・進捗のないreaderを拒否する。
構造のsame/deep copy、chunk境界、canonical/duplicate/alias/unknown fieldとformat上下限を局所試験する。
最終fuzzは11.054秒・23,991実行PASS。有限実行の検査であり、全入力の証明ではない。

current profileは`routerd-native-v1`、configuration semantics `resource-program-native-1`、schema2。
stateはconfiguration codec1、events SQLite6、network journal2、release journal1、snapshot1、SSH identity codec1。
configuration/identityの番号はcodec契約名で、既存fileへheaderを挿入したものではない。
明示的なformat versionは実装側定数と突き合わせる。declaration package/loggerのproduction依存へcompiler/controlを持ち込まない。

純粋`CheckTransition`はtrusted callerの観測schemaと、running/target両writerの読み戻し可能性を検査する。
missing/duplicate componentやsemantics不一致、unknown transitionを拒否する。version0は検査済みabsenceの明示factであり、観測を省略した意味ではない。
実file inventoryの収集、state移行、identity/lease/owner/revocationの復元やkernel/engine policyは実装しない。
現prepare/boot/direct rollback/fallbackは同じ宣言だけを許し、generic callbackのsuccessでformat変更を通さない。
異なる宣言はstageして観測できるが、activationには別のqualified migration workflowが必要である。
この拒否を、移行や旧resource契約全体のparity完了と数えない。

## REDと局所検証

API未実装、format違いのprepare/direct rollbackを通す動作、unqualified boot/fallbackを通す動作をREDで固定した。
manifest claimと実ABIの不一致、mixed-native publisherの出力前拒否、ABI欠落/重複、旧v2/no ABIの拒否、実signed storeとgeneration保持を検査する。
最終関連Raceはrelease7.499秒、parser1.077秒PASS。旧973c2df/5f7cfffの実6 ELFも新parserで5.57秒の期待FAILになった。
前版binaryは実行せず、両archのidentity/ABI検査で欠落を確認した。元SHA/sourceを保持する。

初回`make check`は54.91秒FAIL。release CLIの独立toy ELFだけが新ABI recordを持たなかった。
実装の検証を緩めず、fixtureへ宣言を加えた。CLI package2.634秒PASS、全体check/Race/vet/Python30/6 static ELF build183.80秒、clean13.90秒PASS。

## uncached QEMU失敗とhashを保つmemo

memo前のnative Race7 gateはPASS。一方QEMU signed PID1は、起動確定witnessの後でhost238.56秒/guest199.85秒FAIL。
`failed preflight changed release authority: context deadline exceeded`だった。これは状態比較を完了できなかった失敗で、authorityが変化した証拠ではない。
元manifest/serial/host logを保存し、単にgate期限を延長しない。

Storeは全payloadを各openでhashし、その署名manifestを検証する。成功したELF/identity/ABI解析だけを、
同Store内でmanifest digest＋artifact nameのkeyへmemo化する。keyはcommand/release/arch/ABI等の全期待値を結び付ける。
変更した署名宣言は別key、変更したpayloadはmemo照合前のfresh hashで拒否する。payload byteはcacheせず、最大256件で消去する。
新Storeは空のmemoで検査する。restartを跨ぐtrustや検査省略には使わない。

最初の2 nativeを検査し、同じhashed setで解析を繰り返さず、異なる再署名ABIと同inode payload改変を拒否するRaceを追加した。
memo RED0.770秒、関連Race6.530秒PASS。probeの否定試験は期待したrevision mismatch/ENOENTを要求し、timeout等を期待拒否と数えない。
最終全体check/build207.53秒、clean17.43秒PASS。memo initial/clean6 SHAは一致し、独立loggerの2 SHAはmemo前後でも同一。
CPU/RAM、probe125秒、host240秒は増やさない。これだけで初回timeoutの各処理時間の寄与を完全に説明したとは扱わない。

## 最終実binary/daemon/kernel試験

| native Race gate | 結果 |
| --- | --- |
| TestBuiltRuntimeCatalog | 29.87秒PASS |
| TestPublishBuiltRelease | 13.97秒PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 10.45秒PASS |
| TestDaemonConfigurationAndRecovery | 9.30秒PASS |
| TestDaemonStartupSelectionAndRecovery | 3.53秒PASS |
| TestDaemonConditionalStartupRecovery | 3.53秒PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 32.18秒PASS |

catalog/publisherは実amd64/arm64各3 ELFで、ABIとmanifest v3の署名/stageを検査する。arm64を実行したとは扱わない。
実daemonはroot所有0700 stage・fresh user/network namespaceで走らせる。二WANは独立peerの実TCP/UDP/PCAP、切替/復旧、期限切れ、旧通知、worker復旧、confirmed rollbackを確認する。

最終QEMU signed PID1はhost189.71秒/guest173.72秒PASS。
新v3の実catalog、実行supervisorの宣言照合、実controlでpreflight中503、stale revision/実snapshot欠落の拒否、
prepared replay/409/cancel後commitを確認した。pairはboot前にcancelし、paired loader/paired trial healthのgateではない。
永続ext4二kernelはhost228.06秒/guest191.40秒、二WAN/PBRはhost95.79秒/guest80.75秒PASS。
既存runtime-only fallbackで設定/snapshot provenance、SSH公開identity、stable event ID/cursorを保持する。
関連3個別gateであり、全suiteや対象実機の完了とは数えない。

QEMUは外部NIC/host port/共有host mountなし。signed PID1/storageは1024MiB/2CPU、通常daemonは512MiB/2CPU、storageはprivate regular-file disk256MiB。
host network/block device/mount/clockは変更しない。

## 残作業

宣言はpublisherがbyteへ結び付けた主張であり、コードが全て正しく実装することの証明ではない。
実state/absence inventory、実target compiler・engine/profile/kernel capability、state移行、rotation/revocation/owner/leaseを戻さないpolicyが必要である。
同format updateの試験をformat-changing migrationの完了と扱わない。operator paired prepare、paired loader、exact tuple continuous health/confirmation、fallback試行policyと実paired PID1/kernel gateは未完了。
参照runtime破損時のfallback-before-Open、write-time電源断・ENOSPC/fsync、reference-aware snapshot pruning、same-revision ACK/startup切替監査、独立bootstrap、全engine/ABI/権限分離も残る。
今回timeoutの詳細寄与と以前のhealth false component/再発条件は未確定として保持する。
当版OCI/APK、arm64実行、規模/長時間・対象実機/NIC/性能と全残work packageの受入を継続する。parity/完了flagは変更せず、全体ゴールはactive。

[provenance](2026-10-25-runtime-abi/provenance.json) にsource・元raw SHA・private PCAP SHAを保持する。
developer path、ANSI/改行、公開kernel cert IDを正規化し、ephemeral private SSH key hashを省略する。
公開fingerprintと元SHAは保持し、ELF/initramfs/disk/PCAP/config snapshot本体はcommitしない。
証跡directory内の`sha256sum --check SHA256SUMS`で検査できる。
