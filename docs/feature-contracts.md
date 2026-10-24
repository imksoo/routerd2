# 最初の ResourceProgram に使う機能契約

この文書と `internal/featurecontract` は、元の **routerd** から必要な振る舞いを抽出するための証拠である。
**routerd2 の旧書式との互換契約ではない。** routerd2 は未リリースであり、native schema、journal、内部 API は新設計へ直接置換できる。

対象は Interface・address・route を起点とする resource 実行基盤である。
この段階の fixture は、条件判定、依存、所有物の削除について、元実装の純粋関数を実行する。
全 resource kind の機能同等、実パケットの転送、プロトコルの時系列動作を証明するものではない。

## 参照元と実行方法

参照元は `go.mod` が固定する
[`imksoo/routerd@1329ef54f296507eda79bbbc3b853783dd27251e`](https://github.com/imksoo/routerd/tree/1329ef54f296507eda79bbbc3b853783dd27251e)。
fixture の `SourceTruth` / `SourceReady` はこの実装に対する期待値である。
`NativeRequirement` は routerd2 で維持・強化する要件であり、二つを同一視しない。

```sh
go test -mod=readonly ./internal/featurecontract
```

このテストは clock を固定し、in-memory の state store と純粋な GC planner を使う。
ネットワーク、実ホストの state、外部コマンド、systemd、journald は使用しない。
元 routerd の import は `source_test.go` に限られる。実行時の parser、旧仕様 adapter、互換 daemon は追加しない。

| 証拠 | Fixture | 呼び出す参照実装 |
|---|---|---|
| 条件、三値論理、時間境界 | `ConditionCases()` の 16 ケース | `ResourceWhenMatches`、`ResourceWhenIndeterminate` |
| unknown 時に acquisition を開始しない | 同じ 16 ケースの filter 判定 | `FilterRouterByWhen`、`FilterRouterByResolvedWhen` |
| 必須・任意依存、複数依存 | `DependencyCases()` の 6 ケースと conjunction | `DependencyReady`、`DependenciesReady` |
| 所有権に基づく削除範囲 | `OwnershipCases()` の 6 ケース | `PlanArtifactOrphans`、`PlanDeleteTargetGC` |

コードの参照先:

- [条件と resource filtering](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/resourcequery/when.go)
- [依存と status reference](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/resourcequery/resourcequery.go)
- [所有物の GC plan](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/lifecycle/gc_plan.go)
- [削除対象の eligibility](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/lifecycle/artifact_teardown.go)

## 条件判定から引き継ぐもの

`all` と `any` は三値として扱う。known false は unknown と区別する。
`all(false, unknown)` は false、`all(true, unknown)` は unknown、
`any(true, unknown)` は true、`any(false, unknown)` は unknown になる。
unknown の依存が一つでもあれば必ず全体を unknown にする、という評価にはしない。

元実装では通常の filter は indeterminate の resource を残す。
一方で lease や session を獲得する daemon 用の resolved filter は残さない。
コードコメントは、VRRP role が未確定の BACKUP が起動時に DHCPv6-PD を開始することを具体的な理由として挙げている。
routerd2 では「宣言を保持すること」と「副作用を許可すること」を別に扱い、未確定の宣言を捨てずに activation を待たせる。

`for` は安定時間の境界を含む。30 秒を指定したとき、30 秒未満では開始せず、ちょうど 30 秒で成立する。
入力 event が来なければ永久に待つ実装にせず、境界時刻で再評価する必要がある。

### そのまま移植しない振る舞い

| 論点 | 参照実装の結果 | routerd2 の要件 |
|---|---|---|
| 未観測値 | key が無い場合、通常の equality 判定は false | 未観測と既知の不一致を区別し、未観測は unknown とする |
| 明示 unknown | `StatusSet` かつ文字列 `unknown` の不一致は indeterminate | 文字列 sentinel に依存せず、fact の validity と値を分ける |
| `for` と `maxAge` | generic evaluator は両方に state の `Since` からの age を使う | 値が変わっていない時間と、最後の有効な観測からの時間を独立して管理する |
| 長時間同じ値の再通知 | 最近再通知されても `Since` が古いと generic `maxAge` は false | 再通知は freshness を更新し、変化していない値の hold time は維持する |
| 再起動前の status | generic evaluator だけでは以前の master/ready 値が true になり得る | 現在の boot・producer・config generation の生きた観測であることを別に確認する |
| retained `observed.phase` | `observed.phase` が上位の `phase` より優先される | readiness は現在の適用成功と結び付け、古い phase の文字列だけで依存を解放しない |

`max-age-at-boundary` は参照実装が境界の一致を受け入れることを固定する。
routerd2 の timer の精度・丸め方までこのケースが規定するわけではない。
native の expiry 境界、再評価時刻、status reason は実装と試験で明示する。

## boot freshness の根拠

参照実装も、すべての gate に generic evaluator だけを使っているわけではない。
[`VXLANTunnelController.gateState`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/controller/chain/vxlan_tunnel.go)
は追加の freshness check を行う。

- 観測日時が controller の `StartedAt` より前なら unknown にする。
- 明示 `maxAge` が無ければ 30 秒を freshness 上限にする。
- `UpdatedAt`、または object status の観測日時で freshness を確認する。
- unknown を許可する条件も forwarding の許可には使わない。

この振る舞いには参照元の
[`TestVXLANTunnelGateRejectsPersistedPreBootStateAndUnknownMatch`](https://github.com/imksoo/routerd/blob/1329ef54f296507eda79bbbc3b853783dd27251e/pkg/controller/chain/bridge_vxlan_test.go)
がある。本カタログのテストはこの private controller を呼んでおらず、generic evaluator に boot guard がないことを直接確認する。

routerd2 では、個別 controller ごとの freshness 追加を繰り返す設計にしない。
有限の typed fact に boot identity、producer identity、config generation、順序情報、観測日時・期限を持たせ、受理する場所で失効と古い publication の排除を行う。
永続 status は説明・復旧の材料として保持できるが、復元しただけでは現在の forwarding や新しい lease/session の取得を許可しない。
これらの live fact transport、時間による再評価、再起動時失効は次の CORE-03 / CORE-04 の実装・試験対象である。

## 依存と所有権

最初の static graph では、resource identity を保持した上で参照先の存在、重複、循環を検証し、適用順を決める。
これは実行時 readiness の代替にはならない。上流 resource が定義されているだけで下流を Ready にしてはいけない。
live readiness を導入する段階で、成功した apply generation と fact の freshness を参照する。
元実装の optional dependency をそのまま全 resource の公開 schema に追加する義務はない。

所有権 fixture が確認する原則は次のとおり。

1. kernel に存在するというだけでは routerd2 の所有物としない。
2. 現在も必要な所有物は残す。
3. 削除対象は、以前に所有していたもののうち不要になった identity に限定する。
4. 一つの owner を削除・無効化しても他の owner の address・route を消さない。
5. delete target が空なら何も消さない。

参照実装の eligibility には、DS-Lite 固有の IPv4 address や `VirtualAddress` owner の IPv6 address など、機能固有の判定がある。
fixture は source の純粋な planner を実行するため IPv6 `VirtualAddress` の artifact を使用するが、
routerd2 はこの address list や owner 名の部分一致を共通の所有権判定として移植しない。
native は typed identity と正確な owner を持ち、apply の成功後に所有状態を確定する。

address・route の撤去と、物理 interface の破棄は同じ操作ではない。
resource が false、unknown、削除になったときにどの所有物を撤去するかは、resource ごとの lifecycle として定義する。
失敗した操作を成功済みとして ledger から消すことや、観測しただけの foreign object を一括削除することは認めない。

## 完了範囲の読み方

この変更で完了するのは、上記の選択した source behavior を再現可能な fixture として残すこと。
以下は別の実装・受入が必要であり、このテストの成功によって完了したとは扱わない。

- native 三値 evaluator、条件による start/update/remove、timer による失効。
- protocol fact の publication、producer 再起動・boot・config generation の fencing。
- namespace 内の Interface/address/route の実操作と foreign object 保護。
- 適用失敗からの復旧、confirmed rollback、runtime の切替と journal の整合。
- HealthCheck → 経路選択 → kernel FIB / nftables → 実パケットによる切替確認。
- 残りの resource・CLI・API・Web・観測・更新機能の再構成。

新しい機能ケースを増やすときも、source の結果と native の採用要件を別に記録する。
機能と障害復旧を確認するために元の routerd を参照し、未公開 routerd2 の過去の形式を保存する目的には使わない。
