# 条件付き設定復旧とpaired release journal

2026-10-25 UTC。product checkpoint `c14c7640b5f24c4c9e95704d3fbd6804c54fabc8`。
CORE-02／CORE-06／DIST-01の部分実装。全計画と対象実機の受入は完了していない。

## 実装した境界

固定runtimeのstartup参照に、failed generationとfailed snapshotを持つ条件付きrecoveryを追加した。
許すのは、候補採用前のprepared baseline、またはexact candidateの採用直後だけである。
採用前はpublic revisionとknown-good文書の一致、採用後はexact adoption receiptと後続commitがないことを要求する。
unconfirmedや別の採用記録、後続commit、同revisionでも異なるbaseline文書を拒否する。
一般的なrevision範囲やoperator入力predicateは受理しない。

復旧も新しいpublic revisionへ採用し、`recovered`で候補採用済みだった分岐を記録する。
元snapshot revisionへcounterを戻さない。同一recoveryの再実行は後続commitとreceiptを保持する。
after-rename失敗の再試行でもrevision/eventを重複しない。通常Save/Stageもreceipt分岐の改変を拒否する。
known-good snapshotは検証するが、採用後の失敗候補payloadを再読取しない。候補が失われても、そのexact durable receiptを使って復旧できる。
nested recovery fenceはJSONの内容で比較し、supervisorがdeep copyしてcontrol専用の固定envへ渡す。

release storeの内部`PrepareOptions.Startup/KnownGood`は、候補runtime、設定参照、条件付きfallbackを同一journal世代へ保存する。
config store identityを固定し、candidate/recoveryの二世代を予約する。cancel/failureでwatermarkを戻さない。
`BeginBoot`のexec再入やretryでも同じ設定意図を維持し、`FailBoot`はprevious runtimeとconditional configurationを一緒に選択する。
runtime-onlyの確認ではpaired trialを確定できず、`ConfirmConfiguration`はexact receiptと同じactual revisionを要求する。
confirmed pairの直接rollbackは拒否し、`PrepareRollback`がprevious runtimeを新しいpaired trialとして予約する。
runtime/configの両watermarkを下げない。

**これは参照の型・journal transactionの内部APIであり、実snapshotの互換性やlive healthを自己検証するcoordinatorではない。**
configuration confirmationもcallerが観測した値の検査であり、API自体がhealth probeを行うわけではない。
operator向けCLI/SSH prepare、稼働中writer fence、paired loaderとhealth confirmationの接続は未実装。
現appliance commandはpaired intentが選択されると明示的に拒否し、設定を無視してruntimeだけを起動しない。
[設定契約](../startup-config.md)、[release内部APIの境界](../releases.md#runtimeconfiguration-journal-primitives-not-an-activated-boot-flow) を参照。

## 検証

| 試験 | 結果と範囲 |
| --- | --- |
| test-first RED | recovery/paired API未実装、失敗候補payloadへの不要な依存、固定guest suite未登録の失敗を保存 |
| 初期fixture失敗 | release fixtureが要求する同じ文字数のIDを新試験が使っていなかった。元失敗を保存し、試験データを既存fixture契約へ修正 |
| paired journal | 7保存境界でerrorを注入し、完全な旧版か、runtime/config/fallbackの揃った新版だけが公開されることを検査。SID・generation・fallback fence不一致も公開前に拒否 |
| 二つの実store | 署名ELF fixtureを扱う実release storeと実config journalをdisk上で再読取し、候補採用前後の両fallbackを接続。payload実行やPID1 acceptanceではない |
| whole check/build | `make check build-linux RELEASE_ID=paired-startup-v1` 171.30秒PASS。unit/Race/vet/Python30、amd64/arm64各3 ELF |
| clean確認 | 9.80秒PASS、cacheを含む。6 ELF SHAは初回checked productと一致 |
| 前版実バイナリー | startup対応済み1048243の3 commandはrecovery入力を拒否し、21.41秒の期待失敗。oracleが新動作を要求することを確認 |
| 最終native catalog / publisher | Race harness27.11秒／12.76秒PASS。実amd64/arm64の署名・ELF・stage。arm64を実行したとは扱わない |
| 最終native DHCP / SSH | 10.80秒／9.55秒PASS。元期限・worker復旧と、実SSH編集・snapshot保存・confirmed rollback・control SIGKILL |
| 最終native startup / conditional recovery | 3.48秒／3.48秒PASS。失敗候補snapshotを削除して復旧し、再実行、後続commit保持、候補未採用の復旧、stable event3件を確認 |
| 最終native二WAN/PBR | 30.91秒PASS。独立peer、実TCP/UDP・capture、障害切替/復旧、旧通知、期限切れ、worker復旧、confirmed rollback |
| QEMU conditional recovery / startup | guest25.82秒／23.80秒、host38.90秒／34.39秒PASS |
| QEMU SSH / 二WAN | guest28.02秒／78.34秒、host38.12秒／88.33秒PASS |
| QEMU永続ext4二kernel | guest計147.50秒／host182.63秒PASS。既存signed runtime fallbackと、設定・snapshot・SSH公開identity・event ID/cursorを保持 |

nativeはroot所有0700のprivate stage、fresh user/network namespace、Race harnessを使う。
新QEMU recovery/startupは、専用guestの隔離namespaceでhost-modeの実daemonを起動する試験である。
storageのPID1は既存runtime-only起動であり、新しいpaired設定採用を別kernel間で検証したものではない。
通常guestは512MiB/2CPU、storageは1024MiB/2CPU・private regular-file disk256MiB。
外部NIC、host block device/mount、host network/clock変更はない。既存deadline/RAM/CPUは増やしていない。
5つの関連個別gateであり、全suite一括acceptanceではない。

release/configrefのdependency出力も保存した。production dependencyにはconfig compiler、ResourceProgram、controlを持ち込んでいない。
release testだけが実config storeを利用する。独立した小さなbootstrapの完成を意味しない。

## 残作業

paired coordinatorを実装し、実snapshot・target capability/ABI/state/失効policyのpreflight、稼働中config writerのfence、
operatorのprepare、exact actual tupleのhealth/confirmation、fallbackの起動試行policyを結ぶ。
内部参照を保存できることを、実signed PID1起動・復旧の完了と数えない。

候補runtimeの破損時fallback、compatible state、startup採用を伴う実PID1/kernel boot、書込み中の電源断・実ENOSPC・
fsync syscall故障、snapshot参照を保護するpruneも未検証/未実装。保存境界のerror注入を実電源断と扱わない。
前回QEMUのhealth false内訳・再発条件も未確定として引き継ぎ、強いreadiness条件と診断を保持する。
当版OCI/APK、arm64実行、bootstrap、全engine/ABI/権限分離、全残work package、対象実機/NIC・長時間/性能の受入を継続する。
work package/parityを完了へ変更せず、全体ゴールをactiveに維持する。

[provenance](2026-10-25-paired-startup/provenance.json) はsource、元artifact SHA、private PCAP SHAを記録する。
公開用にdeveloper path・ANSI・改行・公開kernel証明書識別子を正規化し、試験用SSH private-key hashを省略した。
公開fingerprintと元logのSHAを保持する。ELF、initramfs、disk、PCAP、設定/snapshot本体はcommitしない。
証跡directory内で `sha256sum --check SHA256SUMS` を実行して整合性を確認できる。
