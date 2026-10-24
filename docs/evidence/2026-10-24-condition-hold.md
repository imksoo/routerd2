# 条件の for: 安定時間・通知間の障害・実パケット

2026-10-24 UTC。コードcheckpoint: `cb550a9e9c325281eb17da38624bfbf236a549af`。
CORE-03の部分実装であり、全計画・実機受入の継続ゴールは未完了。

## 実装済み

`when` / `ready_when` の条件nodeに、正の有限durationの `for` を指定できる。
factだけでなく `all` / `any` / `not` の結果にも適用する。
その式が連続してtrueだった時間を保持し、指定時間に達するとtrueにする。
待機中はindeterminate、既知falseは直ちにfalse。待機を `not` でtrueへ反転しない。
旧generic evaluatorの待機falseとの違いをnativeの明示仕様とし、strict parityへ算入しない。

履歴は一時的で、現在の宣言にある条件nodeの数に制限する。通知数に応じて増やさない。
snapshotの観測・期限・producer情報を使い、pure plannerへimmutableな評価結果を渡す。
各protocol通知の受理時にも更新し、250msの投影処理の間に終わったfalse→trueも検出する。
現在時刻はCLOCK_BOOTTIMEで、timerだけでも待機解除する。
同値の新しい観測は鮮度を更新するが、安定時間を数え直さない。

false、unknown、producer期限やmax_ageの空白、参照producerの交代、boot/source/operator revision変更で
対象の安定時間を数え直す。成功したkernel projection generationの増加だけでは数え直さない。
履歴を永続化して再起動前の待機済み状態を復活させることはしない。
statusには `when_hold` / `ready_when_hold` を追加した。

## 検証済み

| 検証 | 結果・証跡 |
| --- | --- |
| 単体試験 | 境界一致、同値の観測更新、false/expiry/freshness gap、producer交代、boot/source/revision、時計逆行、immutable snapshot、削除後の履歴解放 |
| 式の組合せ | nested hold、ORの成立operand交代、unknownとのOR、pendingのNOT。[追加race log](2026-10-24-condition-hold/nested-race.log) |
| coordinator | 受理callback間にfalse→trueを挟む再現試験。新しい通知なしのtimer復旧。YAMLの保存・再読込、不正duration拒否 |
| 全体check・build | PASS。全unit、Python 13件、Race Detector、vet、static linux/amd64・linux/arm64。[log](2026-10-24-condition-hold/make-check-build.log) |
| clean実daemon gate | PASS、28.32秒、`dirty: false`。[manifest](2026-10-24-condition-hold/result.json)、[log](2026-10-24-condition-hold/test.log) |
| QEMU実daemon gate | PASS、83.05秒、外部NICなし。[manifest](2026-10-24-condition-hold/qemu-manifest.json) |

実daemonのprimary条件は `max_age: 2500ms, for: 1500ms`。
WAN-A障害からHealthyへ戻った直後に、resource APIが `when_hold` と非activeを返し、
実TCP/UDPがWAN-Bで応答することを確認する。待機後はAへ戻る。
health/control/network worker再起動、producer期限の短縮、producer期限より短いmax_age、
再送、confirmed rollbackとforeign object保護も同じ必須gateで検証した。

[milestones](2026-10-24-condition-hold/milestones.json)、
[WAN-A PCAP](2026-10-24-condition-hold/wan0.pcap)、[WAN-B PCAP](2026-10-24-condition-hold/wan1.pcap)、
[公開subsetのhash](2026-10-24-condition-hold/SHA256SUMS) を保持する。
生statusのboot ID等は公開subsetへ含めず、manifestにhashを残した元artifactは
`dist/health-pbr-test/20261024T115300Z/` に保持した。
QEMUは同じ実装のcommit前runであり、実機受入とは区別する。

## 残作業

CORE-03の一般status reference、全protocolの個別readiness/lifecycle、CORE-04のdurable event等を継続する。
固定commitの別worktreeで全QEMU protocol群とrelease/storageをaggregate実行中であり、
この文書では未完了扱いとする。対象実機・NIC・性能条件と、その受入も残っている。
全work packageとparityの完了判定は変更しない。
