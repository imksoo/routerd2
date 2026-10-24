# Health永久削除後の履歴回収

2026-10-24 UTC。product checkpointは `238bc589283399808e36a68d14fb50f4ae981d5b`。
CORE-03 / CORE-04 / NET-01の部分checkpoint。全work packageと対象実機の受入は未完了。

従来はworker側にdurable queueがあっても、HealthCheckを永久削除すると送信するworkerがなくなり、
再有効化まで履歴を配信できなかった。今回は所有登録をworker起動前に永続化し、削除後は実controlが
登録済みqueueを排他的に回収する。履歴を現在のworker/fact/Derived assertionへ復元する経路ではない。

## 実装

`control-health-relays` producer stateに、state root内の生成済みqueueを所有するservice名だけを保存する。
新しいqueueを初期化し、登録を保存してからworkerを作る。登録保存失敗はworker起動前にcandidateを拒否する。
最大1,024登録。pathは `HealthEventDirectory` が生成し、任意pathやfilesystem走査は受け付けない。
候補適用失敗から元設定へ戻した場合も、候補の登録を元の正本に対する回収待ちとして表示する。

回収には正本sourceからのHealth削除、fresh supervisorで当該health workerが存在しないこと、
local storeの排他lock取得が必要。sourceの置換とはcontrolのauthority lockで直列化し、
disk IO中はruntime mutexを保持しない。別Roleへ置換中の旧health processもstore lockで保護する。
一回で最大8 queueを巡回し、各queueは最大16 eventの一pageを処理する。busyな先頭queueで後続を飢餓させない。

target保存、local ACK、空queueの登録解除の順に進める。targetのpartial保存やACK保存失敗は元のpendingを残す。
local ACK完了後に登録解除だけが失敗しても、再起動後に空queueの登録解除を再試行し、履歴を再公開しない。
ファイルや履歴の削除は行わない。`OpenExisting` はmissing directory/databaseを作らず、foreign/unsafe stateを拒否する。
`LookupProducer` は未登録producerを作らないため、未使用componentが保存状態を増やさない。

`health.protocol_events` に `registered_relays` / `retiring_relays` を追加した。
回収待ちと保存失敗は不健全にし、trial確定を防ぐ。active producerのerrorをsource削除時に除いても、
retirementのerrorは消さない。削除commitは正本を保存した200応答のままhealthy=falseを返せる。
現在のproducerを登録・更新せず、削除したinstance/Derived現在値は404のままになる。

## productと実daemonの検証

| 経路 | 結果 |
| --- | --- |
| 全体check/build | clean `238bc58`、`make check build-linux` 131.74秒、exit 0。unit/Python14/Race/vet/static amd64・arm64。PASS |
| 実SQLite/Race | active ownerのlock、unregistered stateの非採用、control再生成、partial target容量失敗、登録解除失敗、missing queue非置換、8件巡回。PASS |
| control/runtime Race | source削除とsupervisor不在の両条件、unknown supervisor拒否、fact非更新、pendingのhealth/ACK反映、rejected candidate復旧時の可視化。PASS |
| 最初の実daemon | 新しい永久削除sequenceと登録失敗の起動前拒否を含み11.79秒PASS |
| 期限修正後の実daemon | 150秒childが実際に渡ることをlogで確認し、全sequence12.15秒PASS |
| 小さな新旧比較 | targetに未配信のIDを選び、Health永久削除後にworkerを再有効化せず配信。新product1.68秒PASS。旧205f02fのhash検証済みELFは10秒timeoutでFAIL |
| 修正後QEMU | 拡張gate113.66秒、二WAN Health/PBR97.30秒、管理/実SSH41.82秒、EventRule63.62秒、BFD/BGP59.82秒。すべてPASS |
| 既存storage回帰 | 独立した2回のkernel bootでtrial→baseline fallback、設定/journal event ID・cursor/SSH鍵/marker保持、PID 1 pinned exec。120.84秒PASS |

実daemonでは、target DBがHealth INSERTを拒否している間にqueueの元ID/内容を読み、Health/Derivedを
正本から永久削除した。retiring=1の状態をcontrol再起動で維持し、旧workerのPOSTを409で拒否する。
target回復後には、登録解除だけを実SQLite triggerで拒否して再度controlを再起動した。
解除後は元2件が一度ずつ完全一致で存在し、registered/retiring=0、Health instanceとDerived現在値は404になった。
別candidateの所有登録保存失敗では、新Health workerが作られず、authoritative revisionが変わらないことも確認した。

[全体check](2026-10-24-health-retirement/check-build.log)・[ELF hash](2026-10-24-health-retirement/build-manifest.json)、
[関連Race](2026-10-24-health-retirement/wired-race.log)・[追加foreign/retirement Race](2026-10-24-health-retirement/relay-race.log)、
[実daemon](2026-10-24-health-retirement/native.log)・[期限修正後](2026-10-24-health-retirement/native-budget.log)、
[直接比較PASS](2026-10-24-health-retirement/direct.log)・[旧product FAIL](2026-10-24-health-retirement/direct-red-excerpt.log)・
[旧ELF hash](2026-10-24-health-retirement/before-binaries.json) を保存した。
旧ELFは先行合格QEMU archiveから対象二つだけを取り出し、archiveと各ELFの既存SHAを照合した。

## QEMUとfixtureで検出した失敗

最初のQEMUは外側170秒に対して隔離childが既定75秒のままで、追加したcontrol再起動待ち中に
Go test全体timeoutとなった。childを既存の明示timeout helperで150秒にし、parent170秒/host200秒以内に収めた。
元の1秒max_age、hysteresis、各操作の10/20秒等は変更していない。

期限修正後はSQLiteの `database or disk is full` と、保存・EventRule処理のdeadline超過が出た。
cleanup前に測ると、guest rootfsは207,179,776 bytes、image展開分は約199MB、空きは1,171,456 bytesだった。
fixtureデータは約5.1MB、WAL二つは約2.9MBと2.1MBで、物理MemAvailableには約189MiBの余裕があった。
ファイルdataとimmutable imageが同じ小さなrootfs quotaを競合していた。

ローカルのprivate mount namespaceで同じrelay working-set処理を実行した。
pendingは一件、targetは論理容量より十分小さいまま、6MiB tmpfsでは175件目に実SQLITE_FULL・空き0でFAIL。
64MiBでは400件がPASSした。[6MiB](2026-10-24-health-retirement/working-set-6m.log)・
[64MiB](2026-10-24-health-retirement/working-set-64m.log) を比較する。
短い実daemon sequenceは6MiBでもPASSしており、これを容量不足の再現成功とは数えない。
最初のlocal quota試行は長いTMPDIRによるUnix socket path制限で起動前に失敗し、これもFULL再現とは別に保存した。

test専用PID 1で、空の `/tmp` に64MiB tmpfsを明示してimageのrootfsと分離した。
scratch容量はserialとmanifestへ出す。製品boot/storage、SQLite FULL fsync・保持/容量制限・観測鮮度、
VM RAM512MiBとvCPU2は変えない。fixture/init/compiler/vetとPython14をlocalで確認してからQEMUへ戻した。
この修正後の拡張QEMUは113.66秒でPASSし、元ID/内容の回収、登録解除失敗と再起動、起動前の保存失敗拒否を完走した。
失敗した3回を合格の集合へ混ぜず、[QEMU manifest一覧](2026-10-24-health-retirement/qemu-manifest.json) と
[失敗manifest一覧](2026-10-24-health-retirement/failed-qemu-manifest.json) に分ける。
関連回帰と二回起動storageの結果もmanifestに記録し、未実行の全22系統の新aggregate合格とはしない。
[二回起動の比較summary](2026-10-24-health-retirement/storage-summary.json) は鍵識別子を省略し、
比較の真偽とELF/kernel hashだけを残す。新relayの強制電源断受入ではない。

product/integrationの実装は `238bc58`。後続のfixture timeout・直接比較・診断・working-set fixtureと
test-only scratch初期化は別checkpointとしてprovenanceに記録する。QEMUはfresh ELF/initializerをbuildし、
native buildとhashを分ける。全体checkを後から追加したtestの実行証拠へ流用しない。

## 残作業

条件inactiveのまま残る宣言済みqueueの全回収、他protocolのraw event、一般status provider/構造化field/Derived連鎖、
real queueのQEMU abrupt power-loss/reboot、released version間のmigration/完全DB rollback契約は残る。
retention/archive/容量alarm・filesystem全体の予算・規模/遅延、privileged effectの権限/idempotency、federation、
全work packageと対象実機/NIC/性能受入も継続する。実機利用可能や全計画完了と判定せず、parityを上げない。

原logのSHAと抽出/正規化は [provenance](2026-10-24-health-retirement/provenance.json)、公開版は
[SHA256SUMS](2026-10-24-health-retirement/SHA256SUMS) に保存する。workspace/private stage、Go pointer、
公開kernel cert identifierを正規化し、failure dumpは指定したexcerptにする。
読んだ実装は `internal/{events,protocolevents,serviceconfig,control,runtimeplan}/`、
`internal/integration/{bgp,derived,health_retirement}_test.go`、`internal/supervisor/`、
`images/kernel-test-init/main.go`、`scripts/{qemu-network,qemu-storage}.py`、`internal/boot/boot_linux.go` と適用 `../AGENTS.md`。
