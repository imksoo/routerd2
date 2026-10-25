# 確定済み設定snapshotとstore identity

2026-10-25 UTC。product/fixture checkpoint `1fb91bc789a62845c68152c0b1ac495825a9882b`。
CORE-02／CORE-06／DIST-01の部分実装。全計画と実機利用の受入は完了していない。

## 実装した境界

設定journalに永続するランダム128-bit `store_id`を追加した。初回seedと同じdurable recordで公開する。
通常commit、confirmed rollback、worker再起動でもidentityを変えず、Save/Stageでの別identityへの置換を拒否する。
healthとbounded configuration metadataも同じidentityを返す。
未公開のidentityなしjournalは受理せず、壊れたjournalをseedで上書きしない。互換readerはない。

`routerd config snapshot --expected-revision N`は、private control Unix socketのAPIを通して、
現在の確定済みrevisionを明示保存する。controlのwriter mutexをcommit/confirm/rollbackと共有する。
古いrevision、unconfirmed、未reconcileを拒否し、保存だけでkernel/worker/journal revisionを変更しない。
通常commitは従来どおり設定を永続保存する。snapshotは別のmutableな正本ではない。

snapshot format1はstore identity、revision、ResourceProgramをcanonical JSONへ収め、その全bytesをSHA-256で特定する。
2MiB/file、最大128 entry（complete payload最大256MiB）で、満杯でも既存snapshotを忘れず、
同一の現在revisionの再送は既存payloadを検証・directory syncして成功する。
owned0700 directoryとowned0400 regular fileを使い、symlink/hardlink、改変、provenanceの不一致を拒否する。
exclusive temporary fileの同期後にno-overwrite renameとdirectory fsyncを行う。
公開後のsync失敗はuncertainとして返し、再試行で既存payloadを上書きしない。

設定中のcredentialはsnapshotでも機微なままであり、payload本体は公開証跡へ入れていない。
有限lease、learned route、health Up、event cursorやSSH identity fileをsnapshotに取り込まない。
[操作と保存契約](../startup-config.md) を参照。

## 検証

| 試験 | 結果と範囲 |
| --- | --- |
| test-first RED | API未実装のcompile失敗、Saveによるidentity置換、snapshot witness未検査を保存 |
| 関連unit | 別store、古いrevision、unconfirmed、改変、path/alias、hard link、128件容量、large document、identity維持がPASS |
| 最終clean全体check | `make check build-linux RELEASE_ID=startup-snapshot-v1` 99.04秒PASS。unit/Race/vet/Python28、static amd64/arm64各3 command。cacheを含む |
| 前版APKの実negative | 新Race harnessを前版の実APKへ適用し1.38秒で期待失敗。snapshot CLIが未実装である箇所を検出 |
| 実署名catalog / publisher | native Race harness27.01秒／12.66秒PASS。実amd64/arm64 artifactを署名・stage・検証。arm64を実行したとは扱わない |
| native実DHCP | BOOT期限・再送・worker/control復旧10.35秒PASS |
| native実snapshot CLI / SSH | 9.30秒PASS。実Unix API、確定済み保存、unconfirmed409、confirmed rollback、control SIGKILL後のidentityとsnapshot維持 |
| native二WAN/PBR | 31.17秒PASS。独立peer、実TCP/UDP、capture、障害切替・復旧、旧通知、期限切れ、worker復旧、confirmed rollback |
| QEMU永続ext4二boot | guest計150.12秒／host184.44秒PASS。異なるkernel boot、実PID1とruntime fallback、snapshot store/revision/digestと設定・SSH公開identity・event ID/cursorを保持 |
| QEMU snapshot CLI / SSH | guest28.42秒／host41.70秒PASS。実CLIのsnapshot markerをhostでも要求 |
| QEMU二WAN/PBR | guest81.53秒／host91.66秒PASS。実daemon・packet・復旧の個別gate |

nativeはroot所有0700のprivate stageにchecked3 commandを置き、Race harnessからfresh user/network namespaceを使った。
QEMU storageは既存1024MiB/2CPUと256MiB private regular-file disk、通常daemonは512MiB/2CPUとprivate scratchを使った。
RAM・CPU・既存期限を増やしていない。外部NIC、host block device、host filesystem mount、host network/clock変更はない。
storageのbaseline/trialは同じsourceから異なるrelease identityで作成した。過去prototypeとのABI互換を証明する試験ではない。
3つの関連QEMU個別gateであり、全suite一括受入ではない。当版のOCI/APK再build・実行も行っていない。

## 残作業

snapshotの保存・検証までを実装した。bootによる明示startup選択、authoritative journalへの採用、
runtimeとconfigurationの組をgeneration/attempt/confirmation/fallbackへ結ぶtransactionは未実装。
現在のbootは引き続きcommitted journalを使い、snapshot参照を起動入力として受理しない。

snapshot書込み途中の電源断、実ENOSPC、fsync失敗の故障注入、参照を保護するpruneも未検証/未実装。
今回の二bootは正常poweroff後の永続性検証であり、書込み中の電源断試験とは数えない。
独立した小さなbootstrap、全config/state/kernel ABI、engine/dependency/権限分離、全残計画、arm64実行、
対象実機/NIC・長時間/性能の受入を継続する。work packageやparityの完了へ読み替えない。

[provenance](2026-10-25-config-snapshots/provenance.json) にsource、元log SHAとprivate PCAPのSHAを記録した。
公開用にdeveloper pathを正規化し、試験用SSH private-key hashを省略した。公開fingerprintと元証跡のSHAは保持する。
ELF、initramfs、disk、PCAP、snapshot payload本体はcommitしない。
公開artifactの検証は、この証跡directoryで `sha256sum --check SHA256SUMS` を実行する。

公開前scanの初回4件はkernelが表示したAlpine公開X.509証明書のsubject identifierだった。
実credentialではないことを[判定記録](2026-10-25-config-snapshots/public-certificate-scan-review.json)へ保存し、
公開用ログの該当識別子だけを省略した。scan抑制を追加せず、正規化後の公開証跡scanは検出0件。
