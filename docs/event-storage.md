# Event journalの容量確認と履歴export

管理shellの `show event-store` とprivate control APIの `GET /v1/event-store` は、
global event journalの容量を読み取る。hot event、圧縮済みevent、履歴全体、job、consumer/producer登録の件数・byteと上限、
最初/最後のcursor、最も遅いconsumer cursor、DB/WALのsize、DB/WAL/SHMの実割当量、
state filesystem全体の容量と使用可能byteを返す。登録やcursorを変更する操作ではない。

jobは待機中と未回収の完了payloadを `jobs` に数え、回収済みの完了記録は別の `tombstones` に数える。
`pending=0` と容量が空いていることは同じではない。
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

## 完了jobの枠を自動回収する

controlは約1秒周期で、consumer/producerそれぞれ最大32件、合計64件の完了jobを回収する。
同じ周期の設定排他は保持せず、完了flagのindexからbounded batchを読み取る。待機中payload、
consumer cursor/state/version、producer state/version、event履歴と現在factは変更しない。

完了jobの名前空間・名前・key・元event cursorまたは初回state versionと、正規化payloadのSHA-256を
`completed_jobs` へ保存してから元outbox payloadを削除し、両方の容量counterを同じWAL/FULL transactionで更新する。
失敗時はそのbatchをrollbackする。tombstone予算に収まるprefixだけを確定でき、1件も収まらない場合は
元の完了payloadを保持して容量errorを返す。同一内容の再送とACKは引き続き成功し、
同じkeyの内容変更・consumerの元cursor変更は拒否する。古いcursorだけを戻しても作用を再実行しない。
作用成功後から完了ACKまでのcrashには、引き続き作用先の冪等性が必要である。

重複抑止記録は自動で忘れない。既定上限は100万件/256MiBで、`tombstones_rows/bytes` の80/100%を表示する。
byte予算はUTF-8のname/key長と41byte（kind・position・SHA）で、SQLiteの物理size予算とは別である。
元payloadは回収後にこの表から復元できず、SHAは内容照合用であって署名ではない。
解放したSQLite pageは後続jobへ再利用するが、DBファイルを縮小したり、filesystemへ即座に空きを返したりする処理ではない。

回収失敗は `event_journal.storage.compaction_error` と `last_error` に残し、不健全/confirm拒否へ反映する。
read-onlyのfresh inspectionが成功しただけではwrite失敗の警告を消さず、次の回収成功で解除する。
小さな重複抑止記録も有限なので、この上限やevent履歴の上限に達すれば運用継続はできない。

## Event payloadを圧縮領域へ自動退避する

controlは約1秒周期に最大16 eventを、同じSQLite WAL/FULL内の圧縮領域へ移す。
元のevent row・ID・cursorを残す。gzipのround tripとSHA-256の確認中はDB接続/書込transactionを保持せず、
確定時に元bodyを再確認し、圧縮bodyの保存、
元payloadの解放、両容量counterの更新を一つのtransactionで確定する。
失敗ではbatchをrollbackし、元payloadを保持する。圧縮予算に収まるprefixだけを確定でき、
1件も収まらない場合は容量errorとなる。期限による削除やcursorのACKではない。

未読consumerも同じ履歴APIから全内容を読める。pending outboxとtimer用の元cursor参照も維持する。
同一IDの再送は圧縮先を検証して同じcursorでACKし、内容変更は拒否する。
圧縮bodyの欠損、CRC/長さ/SHA不一致、追加streamや展開量超過は読取と再送ACKを拒否する。
検証済みの展開bodyは4MiB payload/4,096件以下のLRU cacheに保管し、同じ履歴のpollingで毎回再展開しない。
cacheを使う際も、SQLiteから読んだ圧縮byte・元size・SHAが全て一致することを確認する。
cacheはcheckpointやauthorityではなく、eviction/Close/restartで消え、欠損や破損を隠さない。
現在のUpやproducer authority、設定意図を履歴から復元しない。

容量表示は `events` が元payloadを持つhot領域、`archived_events` が圧縮領域、
`history` が両方を含む全履歴である。first/tip/slowest cursorは全履歴上の位置を保つ。
圧縮領域の既定上限は100万件/256MiBで、byte予算はgzip長+48byte（cursor・元size・SHA）となる。
全履歴は100万件/1GiBの元event byteを上限にし、export format1の検証上限内に保つ。
両方に80/100% warning/exhaustedを設け、archiveへ移しても全履歴のchargeを解除しない。
`events` の枠とSQLite pageは再利用できるが、DBファイルを縮小したり、別媒体へbackupしたりする処理ではない。

失敗は `event_journal.storage.archive_error` と `last_error`、不健全/confirm拒否に反映する。
read-only inspectionだけではwrite失敗の警告を消さず、次の退避成功で解除する。
保存領域と履歴全体の予算は有限で、外部退避・保持期限/世代終了・filesystem全体の運用は引き続き必要である。

## 新規の履歴archiveを作る

appliance上のlocal commandで、既存のdirectory内に新しいファイルを指定する。

```sh
routerd events export --output /path/to/history.jsonl
routerd events verify /path/to/history.jsonl
```

必要なら `--runtime-dir DIR` と `--timeout 10m` をexportに指定できる。既定の期限は2分、最大1時間。
exportはprivate APIから全履歴の件数/元byteと上端cursorを一度取得し、1 page最大16件でそのcursorまでを取り出す。
元payloadと圧縮payloadの混在、出力中の退避でも、同じ論理prefixを保存する。
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
DB schemaは6。D-09に従って未リリースprototypeの1/2/3/4/5はread-onlyで拒否し、互換readerや移行は追加しない。

exportは履歴のcopyである。完了jobとevent payloadのhot枠回収は追加した。
外部archive退避、履歴/IDの世代終了と保持期限、retention policy、
総filesystem予算、外部alert、規模・長時間受入は残る。重複抑止記録を無条件で消したり、
既存IDの再送を新規eventに変えたりする回収は行わない。
workerごとの送信待ちqueueの容量と、ここで表示するglobal journalの容量は別である。
