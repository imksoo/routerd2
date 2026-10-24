# 履歴cursorに依存しないproducer state/outbox

2026-10-24 UTC。code checkpoint: `cb2bc11`。

## 実装済み

`events.ProducerCheckpoint` / `ProducerTransition` で、観測producerの状態と安定IDの
出力jobを同時保存する。履歴input cursorを持たず、journalが空の初回起動でも使える。
DerivedEventの初回出力を保存するために、形式的なinput eventを捏造する必要はない。

producerとconsumerはAPIと保存tableを分け、同名でも相互の状態・jobを更新しない。
`ProducerPending` は最初のstate versionとkey順にbounded pageを返す。
`ProducerComplete` は冪等で、同一key/contentの再送は完了tombstoneを保持する。
既存keyのcontent変更と旧versionからの状態更新は拒否する。

登録件数・state bytes・outbox件数/bytesは既存consumerと予算を共有する。
片方のAPIに切り替えて上限を迂回できない。容量不足やSQL失敗は状態と出力を部分確定しない。
設定本体のfileと同時保存する既存config producer outboxは、そのatomicityを維持するため別経路のまま。

event DB schemaは4。accepted D-09に従い、未公開prototypeの1/2/3をread-onlyで拒否する。
既存DBの自動変換・削除は行わない。製品版間のmigration/fallback契約を完了したものではない。
APIの詳細とcallerの責務は [保存部README](../../internal/events/README.md) を参照。

## 検証済み

| 検証 | 結果 |
| --- | --- |
| test-first | API未実装でcompile FAILを確認し、test commitを先行 |
| 空journalの初回出力 | 実Derived evaluatorと実SQLite producer APIを組み合わせ、入力履歴0件のままstate/output保存・reopen。期限切れ時に現在Upを復元しない。PASS |
| producer/consumer分離・共通budget | 同名consumerからproducer jobを完了できず、共有登録/state/job上限で全体rollback。PASS |
| 実SQLite state保存失敗 | outbox挿入後のUPDATE triggerを失敗させ、状態/version/jobに部分確定なし。再試行PASS |
| 実SQLITE_FULL | page上限でstate拡張を失敗させ、error 13、状態/version/jobのrollbackと容量復帰後のwriter正常性を確認。PASS |
| 並行claim | 16競合から1件だけ確定。旧version拒否、完了tombstone保持、変更content拒否。PASS |
| 実child SIGKILL・WAL復元 | producer state/version/jobも復元。Race DetectorでPASS |
| 未公開schema拒否 | 1/2/3をread-onlyで拒否し、DB bytesを維持。PASS |
| clean `cb2bc11` | `make check build-linux` exit 0。unit/Python14件/Race/vet/static amd64・arm64。PASS |
| 実daemon二WAN | clean buildのELFをprivate root-owned stagingへ置き、sudoから隔離namespaceを起動。TCP/UDP・PCAP、worker/tree再起動、旧通知、max_age/expiry、hold、confirmed rollback・foreign object保持がRace込み30.09秒PASS |
| QEMU実SSH/control再起動 | 同じcheckpointのfresh ELF、control SIGKILL後も設定履歴一致。45.74秒PASS |
| QEMU二回起動 | schema 4で既存の設定/event ID/cursor/SSH/sentinelを保持。署名trial→baseline fallback、別kernel boot ID、file全体hash一致。137.18秒PASS |

[関連Race log](2026-10-24-producer-state/related-race.log)、
[全体check/build](2026-10-24-producer-state/make-check-build.log)、
[source/ELF hash](2026-10-24-producer-state/manifest.json)、
[実二WAN manifest](2026-10-24-producer-state/health-pbr-manifest.json)・[log](2026-10-24-producer-state/health-pbr.log)、
[WAN-A PCAP](2026-10-24-producer-state/wan0.pcap)・[WAN-B PCAP](2026-10-24-producer-state/wan1.pcap)、
[management manifest](2026-10-24-producer-state/management-manifest.json)・[log](2026-10-24-producer-state/management.log)、
[storage manifest](2026-10-24-producer-state/storage-manifest.json)・[log](2026-10-24-producer-state/storage.log)、
[provenance](2026-10-24-producer-state/provenance.json)、
[SHA256SUMS](2026-10-24-producer-state/SHA256SUMS) に保存した。

## 未実装・未検証

native二WANのharnessは文書だけが未commitの状態でbuildし、product/test sourceは
`cb2bc11` と同じ。daemon ELFはclean buildのものを使った。
現在のprivate path検査では、非root user namespaceでhost root所有の`/tmp`が
unmapped UIDになるため、このrunは上記の明示的なsudo staging経路を使った。
通常ユーザーからのstrict helperをこの経路へ対応させる整備も残る。安全なpath検査は緩めない。

producer APIを使うlive controllerはまだない。上記QEMU試験は新schemaでの既存controlと
設定event保持の回帰試験であり、DerivedEventのdaemon実行やproducer checkpointの
QEMU電源断試験ではない。新producer APIの異常終了復元はhost上の実SIGKILL/WALまで。

保存versionはlive sourceの認証にならず、現在のsource/producer authorityをcaller側で
fenceする必要がある。出力publication/作用後、Complete前のcrashによる再送には、
作用先でも安定IDによるdurable idempotencyが必要。exactly-once実行を主張しない。

EventRule/DerivedEventのresource compile/lifecycle/status、accepted typed live status、
daemon scheduling/source-fenced publication、作用/federation、retention/容量alarm、
製品版migration、全実装計画と対象実機/NIC/性能受入は残る。parity判定は上げない。
