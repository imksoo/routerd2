# BFD観測時刻の比較と通知集約をまたぐhold

2026-10-25 UTC。product checkpoint `95b2f43a666669905e4132b1959c3e5075e2ba83`。
CORE-04の現在値provider／CORE-03の条件継続性の部分修正。全work packageと対象実機の受入は未完了。

## 問題と実装

前回のBFD providerはpeerの遷移counterでflapを検出したが、`observedAt` の文字列比較には不十分だった。
workerが時刻A→B→Aを作り、中間Bをpublisherのmailboxが集約すると、peer状態が同じままなので、
Aへの `equals` が途中の不一致を無視して古いholdを完成させた。
同じ問題は `any_of` の一致値A→不一致B→別の一致値Cでも起きた。

workerはsnapshotを作る時点で時刻値の変更を数え、`timestamp_changes` を通知へ含める。
同じ時刻値の再表示では増やさず、uint64で飽和する。変更を数える場所は通知集約の前である。
controlは同じworker identityでcounter巻戻しや、counterを進めない時刻変更を拒否する。
新generation/epochは別のauthorityとして扱い、旧通知・古いassertionの権限は復活させない。
未公開snapshot形式としてこのcounterを必須とし、旧formatのfallbackは追加しない。

値比較用の有限な `EqualityContinuity` を、通常のpresence継続性と分けた。
compilerは `equals` を使う参照（`any_of`内も含む）で強いproofを選び、
pending hold・input identity・runtimeの継続性観測が同じproofを使う。
`notEmpty`だけなら、非空の時刻の定期更新をまたいでholdを継続できる。
同じ参照を存在確認と比較の両方で使う場合は、厳しい比較側を選ぶ。
このため別のbranchがtruthを保っていても保守的なholdやり直しはあり、legacy parityとは数えない。
counter飽和では時刻fieldはunknown、独立したphaseの有限なproofは維持する。
producer authority、元のBOOTTIME観測時刻・受信期限・supervisor期限は延長しない。

[利用契約](../resource-program.md#derivedevent)、[BFD](../bfd.md#current-status-in-derivedevent)、
[評価器](../../internal/derived/README.md) を参照。

## test-firstとローカル再現

先にworkerの実snapshot生成から評価器へ渡すtestをcommitした。
旧コードでA→B→AとA→B→Cの両方が、古いholdからassertを出す **FAIL** となる。
presence-onlyの更新は元からPASSであり、その挙動も維持する。
[未修正の実挙動](2026-10-25-bfd-timestamp/predicate-red.log) を保存する。

runtimeの受理cache／metadata経路も別のtestへ固定し、未実装fieldのcompile FAILを保存した。
新generationを除くcounter巻戻し・counterなしの時刻変更を拒否し、拒否した更新が受理済みsourceを変えないことを確認する。
同じ値が本当に変わらず続くと、新holdが成立する。飽和／同一snapshotの再表示／無関係なphaseも検証した。
[metadata RED](2026-10-25-bfd-timestamp/runtime-metadata-red.log) と
[関連Race](2026-10-25-bfd-timestamp/focused-race.log) を参照。
BFD1.321秒、Derived1.880秒、runtime8.552秒、resource compile2.185秒PASS。

実daemon fixtureもproduct前にcommitし、既存の100msを **650ms** のholdへ強化した。
実workerのcounterが正であり、Upのまま定期publishをまたいで増えることを要求する。
SHA照合済み旧 `d376489` pairは、実workerがcounterを出さないため **4.43秒FAIL**。
[旧ELF/結果](2026-10-25-bfd-timestamp/before-binaries.json) と
[旧daemon excerpt](2026-10-25-bfd-timestamp/native-old-red-excerpt.log) を残す。
これはmetadataのない旧daemonの拒否証拠であり、A→B→A自体の再現証拠は上のunit logである。

clean `95b2f43` の `make check build-linux RELEASE_ID=95b2f43a6666` は **115.48秒PASS**。
全unit・Python23・Race Detector・vet・static amd64/arm64 buildを含む。
[check](2026-10-25-bfd-timestamp/check-build.log) と
[開始/終了commit・dirty=false・ELF SHA](2026-10-25-bfd-timestamp/build-manifest.json) を保存する。

## 実daemon・QEMUの検証範囲

nativeはchecked amd64 ELFとSHAが一致し、root所有0700 stagingからRace harnessで実行した。
実supervisor/control/network/BFDと独立UDP peerを使い、650ms hold、二度のUpを数えるdurable EventRule、
raw Upを保ったままの期限切れ、同一再送で更新しない期限、worker/control SIGKILL、旧通知409を要求する。
Health/Derived・BFD/BGP・二WAN Health/PBRも同じpairで再検証した。

| gate | native | QEMU |
| --- | --- | --- |
| BFD→Derived/EventRule・650ms hold・counter・expiry/replay/recovery | 13.46秒PASS | 44.16秒PASS |
| Health→Derived/raw履歴・worker/queue復旧 | 13.06秒PASS | 95.07秒PASS |
| BFD→BGP session/FIB・UDP-only loss・SIGSTOP expiry | 23.14秒PASS | 53.39秒PASS |
| 二WAN Health→PBR・実TCP/UDP/PCAP・confirmed rollback | 33.25秒PASS | 98.27秒PASS |

[native manifest](2026-10-25-bfd-timestamp/native-manifest.json) と各test logを保存する。
QEMUは同じproduct/fixture/builder、512MiB/2vCPU・外部NICなし・private64MiB scratchを使う。
鮮度、operation、child/parent fixtureの期限は変えていない。release pairとQEMU test identityのELFは別buildである。
最終4 guestの二ELF・test binary・init・kernelのSHA一致を確認した。
関連4 gateの再検証であり、全suiteの新aggregate合格ではない。

## 残作業と境界

時刻の反転・前進と通知集約はsimulated clockを用いた実worker snapshot生成／評価器で検証した。
実daemonでは通常のkernel clockとperiodic publishで継続性を確認した。
host/guestのCLOCK_REALTIME変更やNTP fault、物理clock・対象実機/NIC・性能／長時間受入は実行していない。

BGPの既存snapshot／旧BGPRouter・BGPPeer status契約も読んだが、BGP providerはまだ実装していない。
他provider、構造化selector／Derived連鎖、BFD raw eventのdurable queue、旧BFD/BGP resource/profile全体の契約、
effects/idempotency・federation、storageのretention/外部退避/世代終了・総予算と全残計画を続ける。
work package全体の完了状態とparity flagsは変更しない。

読んだ実装・原log SHA・変換・環境・private PCAP/snapshotのSHAは
[provenance](2026-10-25-bfd-timestamp/provenance.json) に記録する。
公開artifactのSHAは `SHA256SUMS` に保存する。ELF/initramfs/PCAPとprivate staging詳細はcommitしない。

公開前の変更全体・新commit履歴の [secrets scanは0件](2026-10-25-bfd-timestamp/secrets-scan.json)。
