# DerivedEventの有限snapshot・永続化可能なhysteresis

2026-10-24 UTC。code checkpoint: `8342440`。

## 実装済み

`internal/derived` にpure evaluatorを追加した。既存typed specをcopyしてcompileし、
現在のimmutable snapshotからassert/retract、initial policy、両方向hysteresisを計算する。
戻り値の状態・出力はproposalであり、atomicなversion付き保存が成功するまで公開できない。
出力IDはprogram、scope、入力ID、遷移counterに固定する。

status参照は現在scope/producerの有限観測だけを使う。欠落、未知、期限切れ、未来時刻、
旧scope、期限ゼロは不明とし、保存済みasserted=trueから現在のUpを再生しない。
再評価だけで元の観測時刻を更新しない。ORの鮮度と期限は同じwitnessから採り、
異なる観測を組み合わせて鮮度を作らない。現在statusの期限はpending timerでも制限する。

hysteresisはBOOTTIMEで判定し、壁時計のjumpでは成立しない。同値更新は期限を保持し、
値の反転、不明、lease gap、producer変更、scope変更は保留を取り消すか数え直す。
scope/originはaccepted producer registryから与える必要があり、文字列を渡すだけでは認証にならない。
callerは投影間のflapも含め全受理通知を直列に渡し、見逃した区間の後はincarnationを変える必要がある。

固定routerdの既知値predicateと即時出力を直接比較した。元engineでretract_whenが
動作に使われていないことも比較対象に含めた。未知値を空文字のfalseへ変換する旧動作は採らず、
三値として扱う。詳細な契約、上限、callerの責務は
[component README](../../internal/derived/README.md) に記載する。

## 検証済み

| 検証 | 結果 |
| --- | --- |
| assert/retract、initial、group条件、再試行ID、入力mapのcopy | PASS |
| lease期限一致、旧scope、producer交代、unknown bootstrap、lease gap | PASS |
| hysteresis期限一致、同値renew、反転、壁時計jump | PASS |
| 観測時刻の保持、OR witnessの鮮度/期限整合 | PASS |
| pinned routerd直接比較 | emit 2 × retract 2 × initial 2の既知値経路。status/出力が一致 |
| 実SQLite consumer fixture | pending holdのreopen、outbox挿入後のtimer保存失敗で全体rollback、同一ID再試行、旧version拒否、再open後に履歴を保持しつつ期限切れinputは不明。PASS |
| clean `8342440` | `make check build-linux` exit 0。unit/Python14件/Race Detector/vet/static amd64・arm64。PASS |

[関連Race log](2026-10-24-derived-evaluator/derived-race.log)、
[全体check/build](2026-10-24-derived-evaluator/make-check-build.log)、
[sourceとELF hash](2026-10-24-derived-evaluator/manifest.json)、
[provenance](2026-10-24-derived-evaluator/provenance.json)、
[SHA256SUMS](2026-10-24-derived-evaluator/SHA256SUMS) を保存した。

## 未実装・未検証

これは**componentまで**であり、DerivedEvent resourceのcompile/lifecycle/status、実daemon、
acceptedなtyped live status provider、timer scheduling、current-source付きpublicationへは未接続。
QEMUや実packetでDerivedEventが動いたという証跡ではない。

保存部のconsumer APIはcursorゼロでのjobを拒否する。履歴がまだない初回起動での
DerivedEvent出力にも使える、input cursorに依存しないproducer state/outboxが必要である。
fixtureは既存input cursorを持つ構成だけを検証した。形式的なeventを捏造してこの差を隠さない。
effectの権限/idempotency、federation、大規模性能、CORE-04全体と他作業単位、
対象実機/NICでの利用可能性は引き続き未完了。parity判定は上げない。
