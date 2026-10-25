# 実event SQLite formatの検査と宣言済みstate familyの照合

2026-10-25 UTC。product `5ddf2582dba200e4d6eed5dd6f213d4a47f49f54`、先行テスト `7d30cd5`。CORE-06／DIST-01の部分実装。
最終Health power fixture `7dd1d740ef52a3a32e359c3bb06ad6d7ce27a812`、先行回帰テスト `12cbedb`。
全計画・operator paired boot・対象実機受入は未完了。

## 実装

`events.Store.InspectFormat(ctx)`は、実writerが既に所有しているSQLite connectionを使い、application_id・user_version・journal_mode・非内部schema catalogを一つのSELECT/read snapshotで取得する。
別のwritable/初期化storeやBEGIN IMMEDIATE transactionを作らない。metadataはID、version、mode、object数、catalog SHAだけであり、SQL本体やevent/producer/consumerのbodyは返さない。
row、cursor、registration、outbox、archive、checkpoint、migrationを変更しない。SQLite固有のWAL shared-memory bookkeepingまで無書込みと主張せず、全filesystem byte不変の証明とは扱わない。

SQL読取りの前後で、pinned directory内のmain DBとlockのinode、および関連fileの所有権/linksを確認する。
消失したDBを初期化せず、別inodeに置換されたpathを旧connectionのcached successで隠さない。closedとcancelを拒否し、pool connection待ちの期限切れ後もstoreを使えることを確認する。
DDLは64 object、各8KiB、全体64KiBまで。これはSQLite全体のCPU/メモリ使用や全stored rowの整合性を保証する上限ではない。

通常openは既存のread-only authentication probeで、必須table/indexのSQL定義まで認証してからSQLite RW connectionを開く。
既存の運用時障害復旧試験ではancillary triggerを意図的に保持するため、通常openはそれらを既存どおり扱う。
更新資格のInspectFormatは全catalog一致を要求し、追加table/view/triggerも拒否する。operator extension許可や例外設定ではない。
両経路は同じheader/catalog readerと必須定義の照合を使うが、通常openの成功を更新資格の成功と数えない。

内部object除外はliteralな`sqlite_` prefixにする。旧LIKEではunderscoreがwildcardとなり、sqlitex_unexpected tableまで隠せた。
schema6の既知13 objectのdigest `2bc436d24ae4fcc8ce21c43419888b4619f7cd6199893e771c21342c860570e4`を別の試験で固定し、DDL変更でversion/ABI契約の更新を忘れないようにする。
新headerやprototype schema移行readerを追加したものではない。

private controlの`GET /v1/event-store/format`はquery/pathを受け取らず、configuration mutex外でfreshな実DB検査を行う。
利用不可/closed/形式不一致は503、query指定は400。health cacheを代用せず、event-storeが途中で入れ替わった場合も拒否する。
healthの通常読取りへSQL I/Oは追加しない。

## 宣言済み6 familyと実prepare fixture

署名済みtarget descriptorを受け取るprepare callbackで、実configuration record/snapshot、event format API、network writerとdisk、release journal、SSH disabledの検査済みabsenceを集める。
configuration journal codec1は既存のimplicit identity/adoption/outbox契約へ名前を与える定数であり、新しいfile headerではない。
current compilerのsemanticsと実snapshot schemaを含む6 familyのfactsを、candidateとknown-good fallbackの両入力についてCheckTransitionへ渡す。
current runtime declaration、targetの署名declaration、観測state versionと両writerの読み戻しを照合し、declared_state_formats=all_sixをwitnessへ記録する。

これは現在宣言された6 familyの現formatの部分受入。protocol/engine固有の全state、target executable compilerの実行、kernel/capability、identity/lease/owner/revocation policyや全workerの同時snapshotではない。
fixtureはpairをboot前にcancelする。operator prepare/paired loader/exact tuple trial healthのgateは未完了。

## 先行失敗と局所試験

旧readerはversionを変えずALTER TABLEやDROP INDEXしたDBを受理し、1.79秒で期待FAIL。API欠落は0.42秒のcompile FAIL。
新control APIの未実装404は0.229秒FAIL。LIKEによるsqlitex table見逃しは0.058秒FAILで固定し、GLOBのliteral prefixへ修正した。
初回focused0.734秒、metadata read witness0.062秒PASS。
関連Raceはevents30.281秒、control57.247秒、config32.734秒、runtimeabi1.076秒、guest1.085秒PASS。
最終focused Raceはevents3.623秒、control1.396秒PASS。

query_only=ONでも検査でき、total_changes・cursor・登録/usage等を増やさないことを確認する。
変更済みheader/version/DDLや追加trigger/table、main DB消失/置換、closed/cancel、イベント同時追加、SQLite connection待ちのdeadlineを検査する。
通常openの必須DDL認証失敗ではmain database bytesを変えず、ancillary障害triggerを持つ通常再起動は維持しつつ更新資格を拒否する。
これらのformat metadataを、全data body整合性・全副作用のatomic snapshot・移行やrevocationの認可と扱わない。

## Health power試験の失敗・局所再現・修正

最初のHealth powerはhost217.43秒／guest167.10秒でproduce phaseの起動待ちが期限切れとなりFAILした。
電源断を実行する前の失敗であり、復旧成功として扱わない。元manifest、host/serial log、private disk/initramfsを保存した。
故障注入triggerはHealth observationのtarget配送を止める。試験probeが起動確認から進まず、後に64件のlocal queueが満杯になり、workerのcapacity errorと再起動が起きた。
private diskをread-only debugfsで調べると、supervisorのready記録と正しいrelease/platformが存在した。

`verifyExecutables`は全workerのinodeをrouterd-modulesと比較しており、既に独立したrouterd-loggingを拒否していた。
3種類の異なる実file inodeとprocess catalogを渡す局所試験で、この正常構成の拒否を期待FAILとして再現した。
実factory role catalogから各roleの実行fileを選び、PID1・全workerのfresh inode、ready、release、必要coreの存在を検査するようfixtureを修正した。
別roleのinode、欠落したfile/core、unready、異releaseの拒否をRaceで確認し、実行fileの検査を省略していない。
起動待ちerrorへPID/platform/release/ready/stoppingと、inode誤りへmodule/role/PID/pathを加えた。

製品codeは変更せず、最終clean sourceの全体check/Race/vet/Python30/buildは11.85秒PASS。
修正前後のamd64/arm64各3製品ELFのSHAは6つとも同じ。Health powerだけを最小範囲で再検証し、host138.54秒／guest74.21秒PASS。
CPU/RAM/210秒のphase期限は従来のまま。元失敗のmanifestとlog、局所RED/Race、最終三phaseの証跡を併記する。
別の過去checkpointで起きたhealth falseやtimeoutまで、この原因で説明したとは主張しない。

## 最終当版の実binary/daemon/kernel検証

clean sourceの`make check build-linux RELEASE_ID=events-inspection-v1`は226.03秒PASS。
format、全unit、Race Detector、vet、Python30、amd64/arm64各3 static ELF buildを含む。source/command/6 SHAはbuild manifestへ保存する。
arm64の署名/stagingを、arm64実行として扱わない。

| native Race gate | 結果 |
| --- | --- |
| TestBuiltRuntimeCatalog | 27.36秒 PASS |
| TestPublishBuiltRelease | 13.31秒 PASS |
| TestDaemonDHCPv4OriginalLeaseAuthority | 10.35秒 PASS |
| TestDaemonConfigurationAndRecovery | 9.55秒 PASS |
| TestDaemonStartupSelectionAndRecovery | 3.58秒 PASS |
| TestDaemonConditionalStartupRecovery | 3.28秒 PASS |
| TestDaemonHealthPBRFailoverAndRecovery | 30.91秒 PASS |
| TestDaemonEventArchiveAndCapacity | 7.94秒 PASS |
| TestDaemonDerivedHealthCurrentStatusAndDurability | 13.11秒 PASS |
| TestDaemonEventRuleDurabilityAndLifecycle | 30.17秒 PASS |
| TestDaemonStoppedHealthHistoryWithoutReactivation | 5.28秒 PASS |

native11と最初のQEMU4 gateはproduct source `5ddf258`、最終Health powerはfixture `7dd1d74`。製品6 ELFは同一であるが、全gateの同時・同一source実行ではない。

| QEMU gate | host全体 | guest | 結果 |
| --- | --- | --- | --- |
| release-boot | 175.35秒 | 161.08秒 | PASS |
| storage | 223.28秒 | 185.58秒 | PASS |
| daemon-health-pbr | 98.78秒 | 84.87秒 | PASS |
| daemon-event-archive | 44.59秒 | 34.03秒 | PASS |
| health-power | 138.54秒 | 74.21秒 | PASS |

signed PID1は実controlのprepare中503、stale revision/snapshot欠落拒否、prepared409/replay ACK/cancel後commitと、実6 family format照合を確認する。
永続ext4二kernelはSSH identity/config/snapshot/eventとruntime-only fallbackを確認する。二WANは実TCP/UDP/PCAPで切替/復旧・旧通知・失効・worker再起動・confirmed rollbackを確認する。
archiveはcold/hot履歴とcompaction/consumer/producer再起動を確認する。
Health powerは実SIGKILL二回と三つのkernel bootで、元のHealth queue/ACK・retirement・raw archive・completed output prefixの保持/復旧を検査する。
これは実queue/ACK境界のpower cutであり、全stateの書込みsyscall中・ENOSPC/fsync failureや物理電源断の完了ではない。

外部NIC/host port/共有host mountなし。signed PID1/storage/Health powerは1024MiB/2CPU、通常daemonは512MiB/2CPU。
永続diskは新規private regular file256MiB。既存CPU/RAM/deadlineを増やさず（Health powerは従来210秒/phase）、host network/mount/clock/block deviceを変更しない。

## 残作業

全protocol/engine state inventory、target compiler/kernel/profileとidentity/lease/owner/revocation policy、operator paired prepare/loader/exact tuple health/fallbackを接続する。
参照runtime破損時のfallback-before-Open、全stateのwrite-time電源断/ENOSPC/fsync、reference-aware pruning、startup/ACK監査、独立bootstrap、全engine/ABI/権限分離を継続する。
OCI/APK当版、arm64実行、全残work packageと対象実機/NIC/性能・長時間受入は未完了。前版health false/timeoutの未確定原因を解決済みとは扱わない。
work package/parity/全体完了flagを変えず、全体ゴールはactive。

[provenance](2026-10-25-events-inspection/provenance.json) は元raw SHAとprivate PCAP SHAを保持する。
developer path、ANSI/改行、公開kernel cert IDを正規化し、ephemeral private SSH key hashを省略する。公開fingerprintを保持し、binary/disk/PCAP/private key/snapshot本体はcommitしない。
証跡directory内で`sha256sum --check SHA256SUMS`を実行できる。

公開前にindex全体をgitleaksで検査した。14件は実completed_jobsの出力dedup identity（認証情報ではないSHA256 emission ID）、1件は元mainと同じ無効base64のPEM否定fixtureと照合した。scanner exit1と15件の検出を隠さず、[検査と分類](2026-10-25-events-inspection/reviewed-scan-findings.json)へ記録する。実credentialの検出はなく、ruleやfixtureの抑制変更はしていない。
