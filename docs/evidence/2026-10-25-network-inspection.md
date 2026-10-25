# 実network journalの厳密な読取り・保存・read-only検査

2026-10-25 UTC。product `e5971e6727b1125f82729cdea870e2cd74e23825`、先行テスト `a2a14a7`。CORE-06／DIST-01の部分実装。
全計画、signed paired bootと対象実機受入は未完了。

## 変更

従来のnetwork readerは無制限`ReadFile`と通常`json.Unmarshal`であり、重複/未知/alias fieldやリンクされたjournalを受理していた。
先行試験でこの受入と未実装のinspection APIをREDで固定した。実readerとinspectionを別の検証規則にせず、同じデコーダと保護file読取りへ揃えた。

`network.InspectJournal(ctx,path)`は、一つの実recordの存在、format version、revision、attempted revision、保存済みstatusと完全payload SHAを返す。
所有権body、secret path/hashやlease factは返さない。kernelを観測/変更せず、期限を書き換えず、lock/directoryを作らず、fsyncもしない。
親を検査できた後のENOENTだけが`present:false`であり、親の欠落/不正permissionや読めないrecordはerrorである。
これは現在のkernel health、freshなUp、全workerのatomic snapshotや将来writeの認可tokenではない。

実worker methodは実fileを再読し、writer busyなら即時拒否、closedなら拒否、存在を知っていたjournalの消失を拒否する。
private network socketの`GET /v1/journal`は引数やpath/queryを受け取らない。成功応答は有限metadataだけで、busy/不正/消失等は503、未対応backendは501、query指定は400。
署名付きPID1のprepare fixtureは実worker応答を直接disk読取りと比較し、version2・present・保存済みappliedと同じSHAを要求する。
公開前に設定candidate/fallbackも検査し、pairはboot前にcancelする。実paired loader/healthの受入とは数えない。

## 保存と容量

journalとlockはworker-owned0600・regular・単一link。全path componentをpinned descriptorとnofollowで開き、root/worker以外のownerやgroup/other writeを拒否する（root-owned sticky祖先は例外）。
最終directoryはworker-ownedかつgroup/other非writable。新規directoryは0700。readableな0755 parentも、0600 payloadとtrusted非writable祖先の条件下で許す。
Engineはdirectoryを保持し、同じdescriptorでtemporary file→file fsync→rename→directory fsyncを行う。
親pathが置換されても既存writerをforeign storeへ向けない。rename後syncが失敗しても、公開済みfileの存在を忘れない。

読取り/書込みはencoded64MiB、200万JSON値、depth64を共有する。書込み側も読取り不能な大きさ/構造を公開しない。
未知/重複/case alias、複数JSON値、unsupported version、invalid statusと既存の不正所有権recordを拒否する。
設定の既存strict shape実装を`internal/strictjson`へ移し、双方から使う。operator map keyとcustom JSON型の意味は保持する。
network format2のbyte shapeとBOOT期限契約は変更せず、旧prototype format1のreader/migrationは作らない。
これらの上限は実機性能や最大メモリ使用量の保証ではなく、全state移行やwrite-time power cut/fsync障害の受入でもない。

## 失敗・局所試験

reader REDは2.40秒の期待FAIL、API REDは0.33秒のcompile FAIL。
初回focusedは最終directoryに0700を必須として0.296秒FAIL。Goの`testing.TempDir`がprivate parent下のchildを0777＋umaskで作ることも確認した。
journalのdirectory契約をworker-owned・group/other非writableへ整理し、unsafe writable/symlink/foreign pathの拒否は保持する。修正focused0.322秒PASS。
secret snapshot directoryの0700条件は変更しない。元FAILを保存し、fixtureを一括chmodして成功にしたとは扱わない。

関連全体Raceはnetwork24.657秒、config33.746秒、control56.855秒、guest1.083秒PASS。
追加した保存上限・親差替え・同時書換え・busy/closed・実disk API・shared node/depth拒否を含む最終focused Raceはnetwork7.139秒、control4.468秒、strictjson1.020秒PASS。
既存writer lock保持中も検査でき、diskのapplyingをin-memory degradedへすり替えず、inode/mode/mtime/body/entry一覧とfake kernel書込数が変わらないことを確認する。
失効済みBOOT fenceの元bytesを保持し、巨大recordの保存拒否で既存journalを置換しない。nativeの期限試験とは別の局所検証である。

## 最終当版の検証

clean sourceで`make check build-linux RELEASE_ID=network-inspection-v1`は233.27秒PASS。
format、全unit、Race Detector、vet、Python30、amd64/arm64各3 static ELF buildを含む。source/command/6 SHAはbuild manifestへ記録する。
arm64 ELFの署名・staging検査を、arm64実行として扱わない。

| native Race gate | 結果 |
| --- | --- |
| TestBuiltRuntimeCatalog | 27.86秒 PASS |
| TestPublishBuiltRelease | 13.16秒 PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 10.15秒 PASS |
| TestDaemonConfigurationAndRecovery | 9.5秒 PASS |
| TestDaemonStartupSelectionAndRecovery | 3.53秒 PASS |
| TestDaemonConditionalStartupRecovery | 3.48秒 PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 31.21秒 PASS |
| TestIsolatedKernelForwardingNATAndDSLite | 21.69秒 PASS |
| TestIsolatedKernelWireGuardOwnershipAndTraffic | 9.8秒 PASS |
| TestIsolatedBootAddressLifetime | 8.95秒 PASS |

QEMU signed PID1はhost170.46秒/guest156.28秒PASS。
`candidate_inspected=true fallback_inspected=true network_journal=verified`を要求する。
実controlのpreflight中503、stale revision/実snapshot欠落拒否、prepare中409・同一再送ACK・cancel後commit再開も確認する。
永続ext4二kernelはhost210.26秒/guest175.56秒PASS。
二WAN/PBRはhost93.17秒/guest78.54秒PASS。
実TCP/UDP/PCAPで切替・復旧、worker再起動、旧通知、期限切れ、confirmed rollbackを確認する。
WireGuardはhost25.24秒/guest19.48秒PASS。UDP transport上の暗号化IPv4/IPv6 TCP、large prefix、再起動/rollbackとforeign state保持を検査する。

外部NIC/host port/共有host mountなし。signed PID1/storageは1024MiB/2CPU、通常daemon/WireGuardは512MiB/2CPU、storageのprivate regular diskは256MiB。
既存CPU/RAM/deadlineを増やさず、host network/mount/clock/block deviceを変更しない。

## 残作業

他の実state/absence inventory、target/compiler/kernel/engine/profileとidentity/lease/owner/revocation policyを接続する。
network metadataをlease freshnessやkernel所有権そのものの資格証明として使わない。
operator paired prepare、coordinated loader、exact tuple continuous health/confirmation、bounded conditional fallbackと実signed paired boot gateが未完了。
参照runtime破損時のfallback-before-Open、write-time電源断/ENOSPC/fsync、reference-aware pruning、same-revision ACK/startup切替監査、独立bootstrap、全engine/ABI/権限分離を継続する。
OCI/APK当版実行、arm64実行、全残work packageと対象実機/NIC/性能・長時間受入は未完了。前版health false/timeoutの未確定原因も解決済みと扱わない。
work package/parity/全体完了flagは変更せず、全体ゴールはactive。

[provenance](2026-10-25-network-inspection/provenance.json) は元raw SHAとprivate PCAP SHAを保持する。
developer path、ANSI/改行、公開kernel cert IDを正規化し、ephemeral private SSH key hashを省略する。公開fingerprintを保持し、binary/disk/PCAP/snapshot本体はcommitしない。
証跡directory内で`sha256sum --check SHA256SUMS`を実行できる。
