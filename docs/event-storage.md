# Event journalの容量確認と履歴export

管理shellの `show event-store` とprivate control APIの `GET /v1/event-store` は、
global event journalの容量を読み取る。event、job、consumer/producer登録の件数・payload byteと上限、
最初/最後のcursor、最も遅いconsumer cursor、DB/WALのsize、DB/WAL/SHMの実割当量、
state filesystem全体の容量と使用可能byteを返す。登録やcursorを変更する操作ではない。

jobの完了記録も重複抑止に必要な容量として数える。`pending=0` と容量が空いていることは同じではない。
件数/byte上限の80%以上で `pressure: warning`、上限以上で `exhausted` となり、
`events_rows`、`jobs_bytes`、`registrations_rows` 等の理由を併記する。
filesystemは使用可能領域が16 MiB未満または全体の10%未満でwarning、0でexhaustedとする。
使用可能byteは `statfs.Bavail` であり、root予約領域まで空きと数えない。

`GET /v1/health` の `event_journal.storage` は、約1秒間隔で更新するbounded cacheを返す。
health pollingではSQLiteやevent payloadを読まない。`observed_at` は最後の集計成功時刻で、
観測に失敗した場合は前回値を残して `last_error` を表示する。freshな確認には `show event-store` を使う。
集計自体も大きなpayload scanを行わず、transactionalなusage counterとcursor indexを使う。
集計中にcontrolの設定排他を保持しない。

warningだけでは健全性を落とさず、状態変化をlogへ出す。exhaustedまたは集計失敗は
`State.event_store_error` とhealthへ反映する。confirm時はcacheに頼らず容量を再確認し、
満杯の状態でtrialを確定しない。容量不足があっても、期限切れrollbackのsource復元を妨げず、
その履歴はauthoritative file outboxに保持して後で配信する。
警告logは既存logging経路を使うため、別filesystem/外部collectorへの永続alert保証ではない。

## 新規の履歴archiveを作る

appliance上のlocal commandで、既存のdirectory内に新しいファイルを指定する。

```sh
routerd events export --output /path/to/history.jsonl
routerd events verify /path/to/history.jsonl
```

必要なら `--runtime-dir DIR` と `--timeout 10m` をexportに指定できる。既定の期限は2分、最大1時間。
exportはprivate APIから容量と上端cursorを一度取得し、1 page最大16件でそのcursorまでを取り出す。
新しいAppendは継続でき、exportの開始後のeventは次回分となる。
長いSQLite transactionやread snapshotを保持せず、遅い出力の間もstore lockを保持しない。
途中で履歴が欠けた場合は失敗し、黙ってcursorを飛ばさない。

出力は0600のtemporary fileへ書き、全件とfooterが完成した後にfileをfsyncし、
同じdirectory内へ置換禁止のatomic renameで公開する。既存ファイル、symlink、
export中に別操作が作った出力先も上書きしない。扱えた失敗ではtemporary fileを削除し、
不完全な最終ファイルを公開しない。強制終了で残ったhidden temporary fileは成功したarchiveではない。
rename後のdirectory fsyncに失敗した場合は、公開済みファイルを消さず、完了の不確かさをerrorで返す。

formatはversion 1のJSON Linesで、header、cursor付きevent、footerからなる。
元のID、全内容、観測時刻、expiry、historical metadataを保持し、header/recordsのSHA-256、件数、
canonical event byte数をfooterに持つ。verifyはframe、ID重複、cursor順、上端、件数/byte、
checksum、footer完了と末尾EOFを確認する。SHAは破損検出であり、発行元の認証や署名ではない。
archiveは履歴を含むprivate dataなので、必要な保護を維持して保管する。

## 保存契約と残作業

exportもverifyもsourceのeventを削除せず、consumer cursorやoutboxをACKしない。
archiveはevent履歴のprefix copyで、consumer/producer state・outbox・設定journalのbackupではない。
importやrollback、現在のUp・kernel権限の復元にも使わない。
DB schemaは4のままで、旧prototype向けの互換readerは追加していない。

この実装だけでは満杯のjournalの領域は空かない。archiveの自動退避、hot領域の回収、
dedup用記録の保持契約、retention policy、総filesystem予算、外部alert、規模・長時間受入は残る。
完了済みjobを無条件で消したり、既存IDの再送を新規eventに変えたりする回収は行わない。
workerごとの送信待ちqueueの容量と、ここで表示するglobal journalの容量は別である。
