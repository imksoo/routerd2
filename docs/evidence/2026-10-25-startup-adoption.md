# 固定runtimeでのstartup snapshot採用

2026-10-25 UTC。product `1048243d21d64510aa7bc6bd347f4fff81dee17c`、
readiness fixture `f60b76169724899fbc39a42fc2a76602a9037dae`。
CORE-02／CORE-06／DIST-01の部分実装。全計画と対象実機の受入は完了していない。

## 実装した境界

固定runtimeのboot `startup`は、選択世代、expected configuration revisionとsnapshot参照を持つ。
control workerが既存authoritative journalとsnapshotのstore identityを照合し、設定をcompileしてから、
新しいpublic revisionとして一度だけ採用する。元snapshotのrevisionへcounterを巻き戻さない。
採用世代・参照・採用revisionとstable IDを持つ`configuration.startup_selected` eventを、同じdurable recordへ保存する。

同一選択の再実行は現在のjournalを使い、後続commitを再importで上書きしない。
後続のunconfirmed commitも保持し、通常のcontrol再起動rollbackへ渡す。
古い世代、同じ世代で内容を変えた要求、別store、古いexpected revision、未確認状態を拒否する。
既存outboxを保持し、満杯なら採用前に拒否する。Save/Stageも採用済み世代の消去・改変を拒否する。
明示選択では、journalやsnapshotの欠落・破損をseedや空設定で置換しない。

pureな`configref`はcompiler/kernel/runtimeへ依存しない。参照JSONは1024byteに制限し、重複・case alias・未知fieldを拒否する。
typed boot/journal decodeも同じ検査を通す。supervisorは選択をcopyしてcontrolだけへ渡し、
親環境や`Options.Env`が予約済み選択envを注入できない。無関係なworkerには空値を渡す。
worker再起動でも固定した同じ選択を使う。任意path/argvの実行APIは追加していない。

固定runtimeの採用を実装した段階であり、`startup`とsigned `release`の同時指定は拒否する。
signed runtimeと設定の組をprepare・attempt・confirmation・fallbackへ結ぶtransactionは未実装。
独立した小さなbootstrapを実装したとも扱わない。[操作・保存契約](../startup-config.md) を参照。

## 検証

| 試験 | 結果と範囲 |
| --- | --- |
| API／typed JSON／固定suite RED | 未実装API、typed decoderでの検査迂回、未登録guest suiteの失敗を保存 |
| journal publication境界 | file sync後、rename前後、directory sync後の4境界でerrorを注入。旧版/新版の完全recordと、再試行時の単一revision/eventを確認 |
| 世代・backpressure | 再実行、後続commit、unconfirmed保持、別store、古い世代、同世代改変、outbox満杯、通常writeでの採用世代消去を試験 |
| 全体check/build | clean `make check build-linux RELEASE_ID=startup-adoption-v1` 110.17秒PASS。unit/Race/vet/Python29、amd64/arm64各3 ELF。cacheを含む |
| fixture修正後 | 同じclean全体check/build10.84秒PASS。6 ELF SHAは修正前と完全一致 |
| 前版実バイナリー | snapshot対応済み1fb91bcの実3 commandへ新oracleを適用し、bootのstartup未実装で20.71秒の期待失敗 |
| 最終native署名catalog / publisher | Race harness27.31秒／12.41秒PASS。実amd64/arm64 artifactを署名・stage・検証。arm64実行ではない |
| 最終native DHCP / SSH | Race harness10.65秒／9.55秒PASS。元期限・worker復旧と、実SSH編集・snapshot CLI・confirmed rollback・control SIGKILL |
| 最終native startup | Race harness3.48秒PASS。A→B commit、snapshot A採用、control SIGKILL、後続Cの全daemon再起動保持、未確認D→C rollback、新世代B採用とstable event2件 |
| 最終native二WAN/PBR | Race harness31.12秒PASS。独立peer、実TCP/UDP・capture、障害切替/復旧、旧通知・期限切れ・worker復旧・confirmed rollback |
| 最終QEMU startup | guest24.85秒／host36.49秒PASS |
| 最終QEMU SSH / 二WAN | guest29.41秒／82.13秒、host39.53秒／92.66秒PASS |
| 最終QEMU永続ext4 | 二kernel boot計149.32秒／host183.74秒PASS。既存signed runtime fallback、設定・snapshot・SSH公開identity・event ID/cursorを保持 |

nativeはroot所有0700のprivate stage、fresh user/network namespace、Race harnessを使用した。
QEMUのstartupは、専用guest内の隔離namespaceでhost-modeの実daemonを起動する試験である。
新しいstartup採用を、実appliance PID1や別kernel bootで確認したとは扱わない。
storageのPID1は従来のcommitted起動で、startup採用generationを二boot間で検証するfixtureではない。
通常guestは512MiB/2CPU、storageは1024MiB/2CPUとprivate regular-file disk256MiB。
外部NIC、host block device/mount、host network/clock変更はなく、既存の全体期限やRAM/CPUを増やしていない。

## 初回QEMU失敗と判定の限界

初回startupはguest22.65秒／host36.03秒でFAILした。
unconfirmed restart rollback後にpublic revision6とkernelのalias Cは一致したが、control healthがfalseだった。
その時点のhealth component別errorはログに含まれておらず、具体的な原因・再発条件は未確定。
製品の復旧不良と断定せず、同時に完全に説明できた一時状態とも扱わない。

同じ製品と診断用Race harnessを1 CPUに限定したnative40回は102.43秒PASSで、失敗を再現しなかった。
supervisor Readyはworker readinessであり、設定全体のhealth/projection readinessとは別の契約である。
fixtureは瞬間的な一回の読取りから、revision、kernel alias、採用世代、control health、
同revisionのready projectionが揃うまでのboundedな観測へ変更した。不一致の内訳も記録する。
75秒のchild、110秒のinit、150秒のhostというstartup試験の全体期限は維持した。
最終QEMUはこの強い条件を通過したが、そのrunでは最初の不健全状態自体は観測されていない。
製品code/6 ELFは変更せず、初回失敗と40回のnative記録を残す。

## 残作業

signed runtime/configurationのatomic prepare、世代とattempt、採用前後を区別する安全なfallback、
実際に消費した選択とcurrent revisionのhealth confirmation、trial中のcommit fenceを実装する。
snapshotをcaptureしただけで起動対象になるわけではなく、オンラインrestoreやSSH prepareも未実装。

初回health falseの内訳・再発条件、実PID1/kernel rebootでのstartup採用、書込み中の電源断・実ENOSPC・
fsync syscall失敗、参照を保護するpruneも残る。publication hookのerror注入を実電源断やI/O故障と数えない。
小さなbootstrap、全config/state/kernel ABI、外部engine/dependencyと権限分離、全残計画、
当版OCI/APK、arm64実行、対象実機/NIC・長時間/性能の受入は未完了。
DIST-01は実装着手済みとしてin_progressへ更新し、完了にはしない。全体ゴールはactiveを維持する。

[provenance](2026-10-25-startup-adoption/provenance.json) にproduct/fixture、元artifactとprivate PCAPのSHAを記録した。
元ログを保存し、公開用はdeveloper path・ANSI・改行と公開kernel証明書識別子を正規化した。
試験用SSH private-key hashは省略し、公開fingerprintと元証跡のSHAを保持する。
ELF、initramfs、disk、PCAP、設定/snapshot本体はcommitしない。
公開証跡directoryで `sha256sum --check SHA256SUMS` を実行して整合性を確認できる。
