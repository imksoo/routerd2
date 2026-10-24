# Health queueの二度の強制電源断と三回起動

2026-10-24 UTC。製品コードは `f5e9cc5` のまま、試験fixtureは `9e14ce8`。
CORE-04 / NET-01の復旧受入の部分checkpoint。全計画・対象実機の受入は未完了。

## 何を確認したか

実appliance PID 1・supervisor・control/network/logging/management・Health workerと、
同じ256 MiBのext4 diskを三回の実kernel bootで使った。guestは外部NICなし、1 GiB RAM、2 vCPU。
実loopback TCP probeの送信待ちを実SQLite triggerで作り、二つの境界でhostからQEMUをSIGKILLした。
既存の正常poweroffによるstorage試験を、電源断の証拠へ読み替えていない。

| boot | 境界と結果 | 受理済みevent | local pending | 登録/回収待ち | QEMU終了 |
| --- | --- | --- | --- | --- | --- |
| produce | target保存を拒否し、実Health processをSIGSTOPして全queueを固定 | 2件、target未保存 | 2 | 1 / 0 | host SIGKILL、-9 |
| registry | 元2件と、このbootで新たに受理した2件を配送。local ACK後の登録解除だけを拒否 | 4件、cursor 4/5/6/7 | 0 | 1 / 1 | host SIGKILL、-9 |
| recovered | 空queueと回収待ち登録を再発見。登録障害を解除して健全性を回復 | 同じ4件・内容・cursor | 0 | 0 / 0 | 正常poweroff、0 |

三つのboot IDはすべて異なる。所要時間は24.65秒・26.61秒・23.10秒、合計 **74.36秒PASS**。
二つのcutではguestが子process停止・store close・sync・unmount・poweroffを行わず、
hostが生きているQEMUをkillしたことと終了status -9を検証した。

最初のqueueを固定するのは、cutまでに受理された全件を比較対象にするためである。
途中の未commit transactionを受理済みと数えず、凍結後の全pendingの元IDとevent全内容のSHAを取る。
次のbootで追加受理された分も、sourceからHealthを削除してworkerが消えた後の全queueへ含める。
元の観測時刻・metadataを含む内容が一致し、targetには各IDが一度だけ存在することを比較する。
二度目のcut後は、各eventの元cursorまで一致することを要求する。

sourceは最初のcut後もrevision 1のまま復旧した。二回目のbootで実control commitを使って
Healthと現在のDerivedを削除し、revision 2を保存した。三回目もrevision 2を維持し、
Health processが存在せず、現在instanceと削除済みDerivedは404のままだった。
元supervisor/workerの履歴POSTと現在値通知は409で拒否された。保持したEventRuleは回収履歴を消費した。

## witnessが永続化を肩代わりしない構成

最初のinitializerだけが、workerの存在しない段階で新diskをmountし、bootstrapとtarget拒否triggerを準備する。
そのdiskをunmountしてから、実 `routerd boot` へPID 1のままexecする。
二回目・三回目のinitializerはdiskを先にmount・clean・repairせず、製品の `Boot.StateVolume` に任せる。
各bootでactual PID 1と全core workerの実行inodeをfresh ELFと照合し、state directoryが実virtio diskのext4であることを確認した。

cut時の期待値はguest diskへ書かず、UARTでhostへ渡す。hostは次回test initramfsの
`/test/expected.json` に前回witnessを入れるため、guest側のwitness fsyncで製品の漏れを隠さない。
archiveはこのtest-only fileのためbootごとに変わり、それぞれのhashと実行中の不変性を確認する。
kernelとELFは同じ入力を維持した。cut間にhostのdisk fsyncも挿入していない。
最初の空filesystem formatのfsyncは観測が存在する前だけ、guestのsync/poweroffは最終合格後だけである。

試験はfactory command pairを明示する `Boot.ModulePath` を使う。
署名付きruntime選択・fallbackは既存storage gateの別契約であり、この試験の追加で完了扱いにしない。

## test-firstとローカル検証

先にhostの三boot比較とguestの内容/cursor比較のtestをcommitし、未実装FAILを保存した。
host側には、正常終了・boot再利用・権限識別子変更・pending隠蔽・event欠落/変更/重複・cursor変更の拒否を追加した。
有効そうなwitnessでもphase自体がfailedなら拒否することを、実際の誤受入FAILから修正した。
また、UARTの未完行ではcutを実行しないことをtest-firstで固定した。
Go側は実event全内容とcursorの独立比較、欠落/重複/内容変更/cursor変更・0・重複cursorを検証した。
関連Race/vetとPython全21 testが合格した後にQEMUへ進み、最初の三boot実行で74.36秒PASSした。
最終clean checkpoint `1fbb2d5` の `make check build-linux` は17.24秒PASS。
既存suiteのcache利用はlogにそのまま示し、未実行の新規runtime試験として数えない。
静的amd64/arm64 buildと、専用identityを埋めたQEMU ELFのhashは別々に保存する。
EventRuleの非同期publicationは、同じprobe context期限内で到達を待つ。

再現手順・安全境界は [fixture README](../../images/health-power-test/README.md)、
実コマンドは `python3 scripts/qemu-health-power.py --output dist/goal-validation/health-power-qemu`。
`make qemu-health-power` も同じ専用runnerを呼ぶ。runnerはhost disk引数を受け付けず、
新規0600 regular fileのdescriptorだけをformat/QEMUへ渡し、host mountやloop deviceを使わない。

## 残作業と検証の限界

これは動作中host上でguest VMのprocess/device memoryを失わせる試験であり、PVE hostの電源断や
host filesystem cacheの喪失、物理controllerの故障を模倣しない。あらゆる書き込み命令間のcutを網羅した受入でもない。
保持・archive・容量alarm・filesystem予算、規模/長時間運用、他producer/provider、製品版間migration/完全DB rollback、
作用/idempotency・federation、他work packageと対象実機/NIC/性能の受入は継続する。
この限定した二境界の合格を全計画や実機利用可能性の合格にしない。parityは変更しない。

読んだ実装: `internal/{events,protocolevents,storage,boot,runtimeconfig,control,supervisor,runtimeplan}/`、
`cmd/routerd/{main,bootstrap_linux,release_boot}.go`、`images/storage-test/guest/`、
`images/health-power-test/guest/`、`scripts/{qemu-storage,qemu-network,qemu-health-power}.py`、
`Makefile` と適用 `../AGENTS.md`。

[全体check](2026-10-24-health-power/check-build.log)・[build manifest](2026-10-24-health-power/build-manifest.json)、
[Python21](2026-10-24-health-power/tools.log)・[Go Race](2026-10-24-health-power/history-race.log)、
[三boot manifest](2026-10-24-health-power/qemu-manifest.json) と
[produce](2026-10-24-health-power/qemu-produce.log)・[registry](2026-10-24-health-power/qemu-registry.log)・
[recovered](2026-10-24-health-power/qemu-recovered.log) のserialを保存した。
原logのSHA/正規化は [provenance](2026-10-24-health-power/provenance.json)、
公開版のSHAは [SHA256SUMS](2026-10-24-health-power/SHA256SUMS) を参照。
