# BFD→BGP: ACKの観測時刻と消費遅延

2026-10-24 UTC。コードcheckpoint: `959ba98`。

固定commitのQEMU aggregateで、BFD Down時にBGP workerがadmin ACK timeoutで再起動した。
経路撤去はできたが、同じworkerでBFD復旧を処理する受入条件で停止した。
このrunの後半や全20 gateを合格扱いにしない。

## 修正

ACKには受信時刻を持たせず、gateがqueueから消費した現在時刻で期限を判定していた。
GoBGPの `ADMIN_DOWN + IDLE` を確認した時点のCLOCK_BOOTTIMEを保持し、
operationの開始・期限・bootに結び付けて判定する。
受信済みACKをcontrol更新やtimeout判定より前にdrainする。

adminの500ms制限、control freshnessの500ms制限は保持する。
期限内にACKを観測できない場合のworker停止も保持する。
disable後の復旧には、消費時点より新しいBFD受信とwithdrawalの適用ACKを必要とし、
遅く消費したACKだけでは古いUpを有効にしない。

## 検証済み

| 検証 | 結果 |
| --- | --- |
| 受信時刻の境界 | 期限前ACKを遅く消費するケースは有効。開始前・期限一致/超過・future・別bootは拒否 |
| consumer停止の再現 | disable要求後にgateを停止し、その間にACKを受信・queue保持。時計を期限後へ進めて再開しても誤停止せず、古いUpでは再開しない。[Race log](2026-10-24-bfd-admin-ack/queue-race.log) |
| negative control | 消費時刻で判定する旧方針を一時的に再挿入すると同じ再現試験が失敗。変更は直ちに復元した。[期待したFAIL](2026-10-24-bfd-admin-ack/consumer-policy-negative.log) |
| 全体check/build | PASS。unit、Python、Race Detector、vet、static amd64/arm64。[log](2026-10-24-bfd-admin-ack/make-check-build.log) |
| QEMU実daemon | PASS、54.41秒。初期Downのtransport抑止、UpでBGP/広告/経路、UDPだけ停止した撤去、同じBGP workerでの復旧、BFD worker停止中の有限期限、fresh receiveによる復旧。[log](2026-10-24-bfd-admin-ack/qemu-bfd.log)、[manifest](2026-10-24-bfd-admin-ack/qemu-manifest.json) |

QEMU単独runは同じ実装のcommit前run。
sourceとELFのhashを記録し、公開artifactのhashは [SHA256SUMS](2026-10-24-bfd-admin-ack/SHA256SUMS) に保存した。

この再現試験は、queue後の消費遅延というコード上の問題を固定する。
先行aggregateに他の遅延要因がないことまで、この単独runだけで断定しない。
全20 QEMU gateを修正後の固定commitで再実行中。
NET-07の全契約、性能・実機受入、全実装計画の完了は引き続き未達。
