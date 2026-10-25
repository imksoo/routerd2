# SSH host identityのatomic保存とread-only検査

2026-10-25 UTC。product `cfced2eda357b7cb4e3c1d86b0e12035cff27453`、先行テスト `171dce0`。CORE-06／DIST-01の部分実装。
全計画・signed paired boot・対象実機受入は未完了。

## 実装した動作

従来HostKeyは正式名へ直接書き、既存fileを無制限に読み、permission/link/複数PEMやtrailing contentを検査しなかった。
`management.InspectHostKey`はtrusted parentから実fileを読んで、presence、codec version1、公開algorithm、public-key fingerprintだけを返す。
owner-only0400/0600、regular、単一link、非空64KiB以内を要求し、一つのsupported非暗号化private key以外を拒否する。
parse errorはkey内容をechoしない。private body/hashはmetadataへ出さない。scratchを可能な範囲でclearするが、暗号化保存/完全消去とは扱わない。

inspectionはkey/directory/lockを作らず、syncせず、keyを修復/置換しない。
checked existing parent内のleaf ENOENTだけが観測済みabsenceで、parent欠落や不正/読めないkeyはerror。
初期化する`HostKey`とは別操作であり、観測はlost established identityの再生成を認可するtokenではない。
公開済みkeyの生成・復元履歴やrotation/revocation ledgerを新設したものではない。

networkの既存nofollow/trusted-owner/nonwritable-directory walkを`internal/statefs`へ移した。
root/worker owner、group/other非writable（root-owned sticky祖先は例外）の条件とdescriptor pinningを共有し、WireGuard secret directoryの0700条件は変更しない。
identity pathはclean absolute path。fileをnonblocking/no-followで開いてから型と所有権を調べるのでFIFO/deviceでblockしない。
既存supported unencrypted encodingは変換せず、OpenSSH形式と0400 keyを試験する。ABIのidentity1はcodec宣言であり、新しいfile headerは挿入しない。
実codec定数をruntime ABI declarationの試験へ結び付け、production metadata packageへmanagement/compiler依存は入れない。

初回keyは既存のEd25519 PKCS#8 PEMで作り、pinned parent内のexclusive random0600 temporary→file fsync/close→RENAME_NOREPLACE→directory fsyncの順で公開する。
並行creatorの敗者も正式fileを再読し、捨てた自身の生成keyを返さない。
公開後のerrorはErrIdentityUncertain。正式fileは残し、再実行で検証・parent syncする。既存の不正keyは上書き/修復しない。
process exitで公開前のprivate temporary orphanが残っても、正式keyとして使わず、再実行で削除しない。

## REDと局所検証

旧readerのsymlink/hardlink/0644/parent symlink/writable parent/second PEM/trailing受入は1.47秒の期待FAIL。
新API欠落は0.40秒compile FAIL。storage host checkerがhost_identity_inspected=falseを受理するREDも保存し、両bootのstrict trueを必須へ直した。
最初のfocusedは0.273秒PASS。関連Raceはmanagement1.825秒、network25.961秒、runtimeabi1.263秒、guest1.273秒、storage guest1.294秒PASS。
最終補助focusedはmanagement1.508秒、storage guest1.295秒PASS。追加した0400ケースを含む最終sourceの全試験は下記whole checkで検査した。

実inspectionはinode/mode/mtime/bodyを変更しない。missing parentとleaf absenceを区別し、cancel・FIFO・empty・oversize・corruptを拒否する。
返したsignerが実際に署名/検証でき、scratch clearで壊れないことを確認する。
最初のcreatorをfile-syncedで止め、正式名absenceを検査し、第二creatorを成功させ、両者が同じwinnerを返すことを確認する。
created/written/file-synced/published/directory-syncedで子processをExit73させ、公開境界・再実行・orphan保持を検査する。
error注入でも公開前後のuncertain分類とkey保持を検査する。これらはprocess-exit/境界errorの検査であり、実fsync failure、書込み中VM power cut、ENOSPCや物理電源断の受入ではない。

## 最終当版の実binary/daemon/kernel検証

clean sourceで`make check build-linux RELEASE_ID=identity-inspection-v1`は242.32秒PASS。
format、全unit、Race Detector、vet、Python30、amd64/arm64各3 static ELF buildを含む。source/command/6 SHAはbuild manifestへ保存する。
arm64 ELFの署名/staging検査を、arm64実行として扱わない。

| native Race gate | 結果 |
| --- | --- |
| TestBuiltRuntimeCatalog | 27.26秒 PASS |
| TestPublishBuiltRelease | 12.66秒 PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 11.0秒 PASS |
| TestDaemonConfigurationAndRecovery | 9.35秒 PASS |
| TestDaemonStartupSelectionAndRecovery | 3.48秒 PASS |
| TestDaemonConditionalStartupRecovery | 3.48秒 PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 30.91秒 PASS |
| TestIsolatedKernelForwardingNATAndDSLite | 21.39秒 PASS |
| TestIsolatedKernelWireGuardOwnershipAndTraffic | 9.9秒 PASS |
| TestIsolatedBootAddressLifetime | 8.5秒 PASS |

QEMU signed PID1はhost176.15秒/guest161.95秒PASS。
実prepareは設定candidate/fallback・network実file/workerと、SSH disabledの実absenceを検査し、ssh_identity=inspected_absentをwitnessへ記録する。
preflight中503、stale revision/実snapshot欠落拒否、prepare中409・同一再送ACK・cancel後commit再開も確認し、pairはboot前にcancelする。paired起動/healthの証明ではない。
永続ext4二kernelはhost222.33秒/guest186.38秒PASS。
両bootのhost_identity_inspected=trueを要求し、実management keyのpublic fingerprintとSSH peerを照合する。runtime fallbackでも同じidentity/config/snapshot/eventを保持する。
二WAN/PBRはhost99.09秒/guest82.74秒、WireGuardはhost25.17秒/guest19.28秒PASS。
実TCP/UDP/PCAPの切替/復旧・旧通知・失効・worker再起動・confirmed rollbackと、WireGuardのUDP transport上の暗号化TCP・large prefix・再起動/rollbackを確認する。

外部NIC/host port/共有host mountなし。signed PID1/storageは1024MiB/2CPU、通常daemon/WireGuardは512MiB/2CPU、storageはprivate regular disk256MiB。
CPU/RAM/deadlineを増やさず、host network/mount/clock/block deviceを変更しない。

## 残作業

operator rotation/revocation/owner policy、他の実state/absence inventory、target compiler/kernel/engine/profileとのqualificationを接続する。
inspectionを全identity lifecycle、全state移行・snapshotやlease freshnessの完了と数えない。
operator paired prepare、coordinated loader、exact tuple continuous health/confirmation、bounded conditional fallbackと実signed paired boot gateは未完了。
参照runtime破損時のfallback-before-Open、write-time電源断/ENOSPC/fsync、reference-aware pruning、startup/ACK監査、独立bootstrap、全engine/ABI/権限分離を継続する。
OCI/APK当版、arm64実行、全残work packageと対象実機/NIC/性能・長時間受入は未完了。前版health false/timeoutの未確定原因も保持する。
work package/parity/全体完了flagを変更せず、全体ゴールはactive。

[provenance](2026-10-25-identity-inspection/provenance.json) は元raw SHAとprivate PCAP SHAを保持する。
developer path、ANSI/改行、公開kernel cert IDを正規化し、ephemeral private SSH key hashを省略する。公開fingerprintを保持し、binary/disk/PCAP/private key/snapshot本体はcommitしない。
証跡directory内で`sha256sum --check SHA256SUMS`を実行できる。
