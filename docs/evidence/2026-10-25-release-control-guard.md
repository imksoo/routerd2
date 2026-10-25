# 選択済みsigned runtimeと設定writerの排他制御

2026-10-25 UTC。product `e3451efa0b775f98248c13123ea84a78ba819fef`、storage fixture `234827d762174b12dff5141eec039c50b4b6ffdf`。
CORE-02／CORE-06／DIST-01の部分実装。全計画と対象実機の受入は未完了。

## 実装した境界

signed bootで検証したroot/trust-file/manifest digestをsupervisorがcopy/固定し、control専用のcanonical private envへ渡す。
継承envとOptions.Envでは上書きできず、他workerには空値を渡す。operatorのboot設定fieldではない。
controlは保護されたtrust fileとrelease storeを開き直し、設定open/adopt前に選択runtime/startup・既存store identityをlock内で照合する。
古いruntime、別store、未採用startupは拒否する。standalone modeは既存の起動契約を保つ。

`WithState`は現在のjournalと参照runtimeを検証してから、callbackの終了までprocess mutexとfile lockを保持する。
controlのmutex→release lockの順序を固定し、stage→kernel/protocol apply→Saveと失敗時のrestoreを同じlock内で完了する。
confirm、期限切れrollback、restart reconciliationも選択を照合する。check後にlockを離してapplyする経路は作らない。
競合はHTTP503、選択不一致やpaired freezeは409。callback内で同storeやEngineを再帰呼出ししない。

paired pendingはprepare直後からtrialまで、新規commitと未確認recordの変更を止める。
同一request/hashのdurable ACKは適用を繰り返さず、cancelまたはexact paired confirmで新規commitを再開する。
confirmed configurationの同revision再適用はprepare中も許す。runtime-only pendingの既存commit契約は保つ。
これはrevision/confirmation writerの境界であり、same-revision event outbox ACKや動的protocol effectsを全て止めるものではない。

## 局所試験と実起動

API/transport未実装RED、期限切れrollbackのfreeze漏れREDを保存し、実装より前にtest commitを置いた。
実signed release storeと実config journal、control engineとnetwork fakeを接続し、apply中のprepare拒否、file lockの別handle間排他、
callback失敗・cancel時のunlock、prepare/cancel/trial/confirm、旧runtime・別SID・未採用receipt拒否、復元とconfirmed rollbackを検査した。

初回実装はunused importでcompile FAIL。次の試験は、取消済みpublisher sequenceを再利用して拒否された。
importを修正し、試験はwatermarkを戻すことなく新しいsequenceを使う。両失敗の元logを残す。
全体check/Race/vet/Python30/6 static ELF build193.31秒、clean9.55秒PASS。fixture変更後11.50秒PASS。
両arch計6 SHAは一致し、release/configrefのproduction dependencyはcompiler/controlを含まない。

| native Race gate | 結果 |
| --- | --- |
| TestBuiltRuntimeCatalog | 29.87秒PASS |
| TestPublishBuiltRelease | 13.91秒PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 10.25秒PASS |
| TestDaemonConfigurationAndRecovery | 9.51秒PASS |
| TestDaemonStartupSelectionAndRecovery | 3.59秒PASS |
| TestDaemonConditionalStartupRecovery | 3.68秒PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 30.92秒PASS |

nativeはroot所有0700 stageとfresh user/network namespaceを使う。amd64は実行、arm64は署名・ELF・stageまでで、実行受入ではない。
二WANは独立peer、実TCP/UDP・PCAP、切替/復旧、旧通知・期限切れ・worker再起動・confirmed rollbackを検査する。

前版0d35db7の実3 commandを、新しい独立probeとともに専用QEMUで起動した。
PID1・実worker・health confirmationまで進んだ後、paired prepare中の新規commitが成功し、116.28秒で期待FAILになった。
修正版は同じoracleを151.34秒host/136.79秒guestでPASS。
実controlのrevision1→commit2→snapshot→raw paired rollback prepare→同一再送2→新規409→cancel→commit3、
SID一致と予約generation11の保持をwitnessに要求する。hostは実行witnessとcommit-fence witnessの両方を必須にする。
raw intentはboot前に取り消す。これはpaired loaderやpaired trial healthの試験ではない。

## 初回storage失敗と限定したfixture修正

初回の永続ext4 gateはhost137.04秒、first kernelで`committed routing configuration is not healthy`によりFAIL。
ログにhealth componentはなく、原因や今回の製品変更との因果を確定できない。元manifest/serial/host logを保持する。

probeはSSH commit直後の一回health読取で失敗すると、既存のhealth・event保存・producer ACK待ちへ進まなかった。
局所HTTP fixtureでrevision2/services applying→ready、outbox pending→ACKの合法な途中状態を作り、一回判定の拒否を再現した。
その一回判定を除き、元の150秒probe/240秒host上限内で全条件を待つ。恒久不健康はdeadlineで失敗し、
revision、service phase/revision、route issues、各health errorを保存する。RAM/CPU/deadlineと製品は変えない。
局所Raceの回復/恒久失敗ケース1.292秒PASS。これで元のhealth falseの内訳を説明し切ったとは扱わない。

同じ6製品SHAの修正fixtureでは、永続ext4二kernelが229.31秒host/192.60秒guestでPASS。
SSH設定、snapshot SID/revision/digest、SSH公開identity、stable event ID/cursorを保ち、既存runtime-onlyのbounded fallbackを確認した。
QEMU二WAN/PBRも97.97秒host/83.34秒guestでPASS。
この3個別gateを全suiteや対象実機の完了とは扱わない。

QEMUは外部NIC/host port/共有host mountなし。storageだけprivate regular-file disk256MiB、1024MiB/2CPU、
通常daemonは512MiB/2CPU。host network/block device/mount/clockは変更しない。

## 残作業

operator prepareとcoordinatorの実snapshot/target capability・kernel/config/state ABI/失効policy preflight、
paired loader、exact actual tupleのhealth confirmation、fallback試行policyと実signed paired PID1/kernel bootは未接続。
commandのpaired intent明示拒否を維持する。内部APIで保存できることを運用可能な更新と数えない。

参照runtime破損時にreadState/Openがfallback選択前に失敗する問題、書込み中の実電源断・ENOSPC/fsync syscall失敗、
snapshot参照を保護するprune、same-revision ACKと起動切替の監査も残す。
全snapshot/ABI/engine/権限・独立bootstrap、当版OCI/APK、arm64実行、規模/長時間と対象実機/NIC/性能受入は未完了。
今回と以前のhealth falseの具体的component/再発条件は未確定として保持する。
work package/parityの完了状態は変更せず、全体ゴールをactiveに保つ。

[provenance](2026-10-25-release-control-guard/provenance.json) にproduct/fixture source、元raw SHA、private PCAP SHAを記録した。
公開用にdeveloper path・ANSI/改行・公開kernel certificate IDを正規化し、ephemeral private SSH key hashを省略した。
公開fingerprintと元logのSHAは保持する。ELF/initramfs/disk/PCAP/設定snapshot本体はcommitしない。
証跡directory内の`sha256sum --check SHA256SUMS`で検査できる。
