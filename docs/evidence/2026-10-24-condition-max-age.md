# 条件の max_age: 観測時刻・再送・実パケット

2026-10-24 UTC。コードcheckpoint: `e46ce226d683450e7411f91bbba4cdf7e5f7538f`。
全計画完了・対象実機受入を目標として継続中のCORE-03の部分実装である。

## 実装

`when` / `ready_when` のfact leafで、`max_age: 2500ms` のように正の有限durationを指定できる。
観測からちょうど指定時間までは有効、それを超えるとindeterminateになる。
producerの有限期限、boot/source/workerのfencingも引き続き必要である。
`not` もstaleをtrueにしない。旧generic maxAgeとの相違は
[機能契約](../feature-contracts.md) に記録したnative要件に従う。

HealthはproducerのCLOCK_BOOTTIME観測時刻を保持する。その他の対応済みprotocolは
新しいleaseを受理したときの単調時刻を使い、同じleaseのtransport再送を新しい観測にしない。
同一inactive snapshotに合成する有限期限も、sequenceだけの増加では延長しない。
supervisorのready factはpoll時の観測時刻を使う。
投影ごとに観測時刻を現在時刻へ置き換える処理を除去した。

## 検証済み

| 検証 | 結果・証跡 |
| --- | --- |
| evaluator | 境界一致、境界超過、producer expiry、NOT、immutable snapshot、invalid durationを単体試験 |
| source | YAML保存・再読込でmax_ageを保持。不正なdurationを拒否 |
| coordinator | HealthとDHCP leaseのsequence再送、実際のlease更新、inactive再送を試験 |
| 全体check・静的build | PASS。unit・Python 13件・Race Detector・vet・linux/amd64とlinux/arm64。[log](2026-10-24-condition-max-age/make-check-build.log) |
| clean実daemon gate | PASS、`dirty: false`。独立2 WAN、実TCP/UDP、PCAP、mark counter、worker再起動、expiry、rollbackを含む。[manifest](2026-10-24-condition-max-age/result.json)、[log](2026-10-24-condition-max-age/test.log) |
| QEMU | 同じ追加ケースを外部NICなしのAlpine kernelでPASS、61.66秒。[manifest](2026-10-24-condition-max-age/qemu-manifest.json) |

追加したpacketケースではHealth workerをSIGSTOPし、supervisor-readyのまま待つ。
2分のproducer期限が残った実Healthy sampleが、利用側の2.5秒制限でprimaryを失うことを確認する。
新しいsequenceで同じsampleを受理した後も、複数のreconcile tickを跨いでTCP/UDPはbackupへ流れる。
Health workerの再起動と新しい観測でprimaryへ戻る。
1.5秒へ短縮したproducer期限の既存ケースも別に保持し、二つの失効原因を混同していない。

[milestones](2026-10-24-condition-max-age/milestones.json) は生statusから必要な世代・期限・経路だけを抽出した。
[WAN-A](2026-10-24-condition-max-age/wan0.pcap) / [WAN-B](2026-10-24-condition-max-age/wan1.pcap) は隔離fixtureのアドレスだけを含む。
manifestはVM内の生statusのhashも含み、生artifactは `dist/health-pbr-test/20261024T113944Z/` に保持した。
公開subsetのhashは [SHA256SUMS](2026-10-24-condition-max-age/SHA256SUMS)。
QEMUは同じコード差分のcommit前実行であり、clean mainの配布物や実機受入を主張しない。

## 残作業

`for` の安定時間、一般status reference、全protocol個別readiness/lifecycle、CORE-04のdurable event、
完全なEgressRoutePolicy等は引き続き実装計画の対象である。
自然な2分のsample silence、全QEMU suiteの同一tree aggregate、release/storage、
対象機種・NIC・性能の実機受入もこのcheckpointでは完了扱いにしない。
既存parity判定とwork packageの完了状態は引き上げない。
