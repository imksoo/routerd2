# ResourceProgram と設定 transaction

routerd2 の設定は `schema_version: 2` の **ResourceProgram** である。
保存する正本は operator が編集した resource の宣言で、kernel の観測値や生成済み worker 設定ではない。
resource の ID、依存関係、owner、provenance と入力順を、そのまま candidate・running・確認付き commit の復旧用設定に保持する。

## 設定例

```yaml
schema_version: 2
resources:
  - id: wan
    interface:
      name: eth0
      mtu: 1500
  - id: wan-address
    owner: wan
    depends_on: [wan]
    address:
      interface: eth0
      cidr: 192.0.2.1/24
  - id: default-v4
    owner: wan
    depends_on: [wan-address]
    route:
      destination: 0.0.0.0/0
      gateway: 192.0.2.254
      interface: eth0
      metric: 100
  - id: forwarding
    forwarding:
      ipv4: true
```

`id` は program 内で一意の operator identity で、interface 名や service 名とは別に選べる。
英数字で始まる 1–128 文字で、英数字と `. _ : / -` を使える。
たとえば `id: wan` と `interface.name: eth0` を独立に持てる。
内部 importer が生成する ID は `interface/eth0`、`service/wan` などである。
アドレスや経路には kernel object の identity から生成した安定した suffix を使い、route の gateway 変更だけでは ID を変えない。

一つの resource には、次の payload のうち一つだけを指定する。

| Payload | 意味 |
| --- | --- |
| `interface` | interface の UP、MTU、master、alias、RA/autoconf 設定 |
| `address` | 一つの host/prefix、対象 interface、任意の address options |
| `link` | bridge、VLAN、VRF、VXLAN、macvlan、WireGuard などの既存 native link intent |
| `route` | 静的経路と ECMP next hops |
| `policy_rule` | policy routing rule |
| `forwarding` | IPv4/IPv6 forwarding の管理 |
| `firewall` | 順序を持つ既存 ACL/NAT intent |
| `ds_lite` | 明示的な固定 endpoint の kernel tunnel |
| `wireguard` | 既存 WireGuard device/peer/key-file intent |
| `vrrp_guard` | VIP の既存 input guard intent |
| `service` | 既存 DHCP4/6、RA、DNS、VRRP、DS-Lite discovery、BGP、BFD の typed worker 設定 |
| `event_rule` | durable journalの履歴pattern評価と、同journalへの安定ID付き出力 |
| `derived_event` | 有限な現在statusのpredicate評価、安定時間とdurableなassert/retract履歴 |

`interface.addresses` と `interface.address_options` には設定しない。
アドレスは独立した `address` resource とし、options は `address.options` に置く。
これにより同じ interface の設定と複数のアドレスを別の宣言へ帰属させられる。
明示的な `interface` resource がないアドレスは、必要最小限の UP 設定を生成する。
ほかの native field と protocol の制約は引き続き検証する。

## EventRule

```yaml
schema_version: 2
resources:
  - id: commits
    event_rule:
      pattern:
        operator: count
        topic: configuration.committed
        threshold: 2
      emit:
        topic: routerd.operator.commits
        attributes:
          count: "${count}"
```

`event_rule` は固定routerdのtyped pattern/emitを使う。all_of、any_of、sequence、window、
absence、throttle、debounce、countをcompile時に検証し、inactiveな不正定義も副作用前に拒否する。
live input producerは設定commit/confirm/rollback、DerivedEventのassert/retract、HealthCheckの
probeごとのpassed/failed/timeoutである。他のraw protocolイベントは未接続。
新規activationはjournal末尾から始め、過去の履歴を初期集計へ取り込まない。
集計状態・入力cursor・出力jobは同時保存し、control再起動後に再開する。
出力は `show observations` で確認でき、作用実行やprotocol leaseにはならない。

定義が変わらない無条件ruleは、無関係な設定更新やlogging worker停止で集計を失わない。
削除・定義変更・明示した `when` / 依存条件の失効では旧timer/outputを公開しない。
条件のfalse→trueがcontrolの確認間隔内に終わっても、新しいactivationとして扱う。
条件付きruleはcontrol再起動後に現在の有限観測から取得し直す。
自身の `ready_when` は他resourceの依存条件であり、自分のstart条件ではない。

timer期限は元の観測時刻から計算する。未処理入力との比較では、期限より後の入力の前に
timerを評価し、期限と同時の入力を優先する。保存済みtimerも現在の定義・activationにfenceする。
外部作用の権限判定は別契約であり、historical eventだけでは許可しない。

HealthCheckのprobe eventは `routerd.healthcheck.SERVICE_NAME.passed` / `.failed` / `.timeout`。
phaseが同じでもprobeごとに記録する。worker側のprivateな送信待ちqueueへ保存してから送信し、
logging停止、ACK消失、worker/control再起動で同じID・元の観測時刻・内容を再送する。
queueは最大64件かつencoded state 64KiBで、保存済み分を捨てずに容量不足を返す。
local保存に失敗した結果を現在のfactとして公開せず、workerを停止する。
control側の部分保存はACKせず、再送時に保存済みprefixをdedupする。
`health.protocol_events.last_error` はproducerごとの保存失敗を集約し、不健全/trial確定不可として返す。
現在のproducerを登録したりfact期限を延長したりする経路ではない。
過去のboot/configの履歴には元のproducer metadataを保持する。履歴の遅延配信を現在のUpとして扱わない。
queueの所有登録はworker起動前に保存する。Health resourceを永久削除した後は、登録済みのqueueだけを
controlが回収する。fresh supervisorから当該health workerが消え、storeの排他lockも取得できることが必要。
target保存とlocal ACK後、queueが空になったことを確認して所有登録を外す。登録解除が失敗しても
control再起動後に再試行し、元eventを重複公開しない。missing storeを空で置換したり、未登録の状態を
走査・採用・削除したりしない。現在のworker/fact/Derived現在値を復元する経路ではない。
`health.protocol_events` の `registered_relays` / `retiring_relays` は件数を返す。
回収待ちと保存失敗は不健全でtrial確定不可となるが、削除のcommit自体は成立したrevisionを保持する。
候補適用失敗から元設定へ戻した場合も、候補の残queueを回収待ちとして扱う。
[実TCP・再起動・DB失敗の証跡](evidence/2026-10-24-health-protocol-events.md) を参照。
[永久削除後の回収と新旧比較](evidence/2026-10-24-health-retirement.md) に後続の検証を記録する。

宣言を残した条件inactiveのHealth queueも、workerの選択bindingとfresh supervisor上のhealth processが
両方ない場合に回収する。回収中はruntimeのmodule reconcile排他を保持して再有効化を防ぎ、
現在factを受理する状態mutexは保持しない。既にreconcile中ならcontextの期限まで待ち、
期限切れは回収失敗として可視化する。毎回TryLockで黙って回収を飛ばす経路は使わない。
宣言済みqueueは空になっても所有登録を保持し、将来のworkerが同じqueueを使える。
全候補にhealth bindingが選択されている場合は回収を明示的に拒否して終了し、reconcile排他や
supervisor IOを使わない。停止候補がある場合だけ、期限付きの所有確認と排他回収を行う。
`stopped_pending_relays` は検出済みの未回収件数を返し、複数pageの途中は不健全表示を維持する。
排他回収で空を確認したqueueだけが元workerの配送errorを解除する。reconcile排他の待機・supervisor照会中はcontrolのsource lockも解放し、通常の履歴送信を止めない。
callbackでsource lockを取れない場合は次回へ回し、設定更新との逆順ロックを防ぐ。両方を取った後に
正本とregistryを再読し、所有観測にない名前のpendingは解除しない。設定/revisionは変更せず、
停止中の現在instanceは不在、Derived入力はunknownのまま。新しい現在値は再有効化したworkerから受理する。

`/v1/health` の `event_rules` はrevision、ruleごとのid/lifetime/cursor/versionと
出力outboxのpending有無、bounded errorを返す。保存/publication失敗時はhealthを不健全にし、
正本の設定と未処理状態を保持して再試行する。API応答が503でも、設定がrollbackされたと解釈しない。
一つのsourceは最大128 rule。schedulerは200ms周期・bounded batchで、規模・遅延の性能受入や
長期運用用の外部退避・保持期限/世代終了は未完了。
容量観測・warning/confirm guard・固定tipの履歴export/verifyは実装したが、
exportだけではevent履歴本体の領域を空けない。完了outbox payloadは別予算のSHA-256記録へ自動回収し、
元の重複抑止を保って枠/SQLite pageを再利用する。event本体も同じWAL内でgzip領域へ移し、全履歴の読取/再送を保ってhot枠を回収する。
外部退避・履歴/IDの世代終了・保持期限・長期運用の受入は未完了。
[操作と保存契約](event-storage.md) を参照。

## DerivedEvent

```yaml
schema_version: 2
resources:
  - id: wan-check
    service:
      name: wan
      health:
        interface: eth0
        source_address: 192.0.2.2
        protocol: tcp
        target: 192.0.2.1
        port: 443
        interval_ms: 1000
        timeout_ms: 500
  - id: wan-derived
    derived_event:
      topic: routerd.operator.wan
      inputs:
        - field: "${HealthCheck/wan-check.status.phase}"
          equals: Healthy
      emitInitial: true
      hysteresis: 2s
```

`derived_event` は固定routerdのtyped specを使う。参照のresource名は `service.name` ではなく
ResourceProgramの `id` である。`service/wan` のような `/` を含むIDも使える。
上の例では対象への経路とアドレスは別resourceまたは既存の管理経路で用意する。
status参照だけでは暗黙のstart/readiness依存辺を作らない。停止中のproviderはunknownになる。

現在対応するproviderはHealthCheckで、scalar fieldは `active`、`phase`、`lastResult`、
`lastCheckedAt`、`lastTransitionAt`、`consecutivePassed`、`consecutiveFailed`、`failureCount`、
`message`、`lastSuccessTime`、`lastFailureTime`、`firstFailureTime`。
観測済みの空optional fieldと観測なしは区別する。他Kind、構造化field、DerivedEvent連鎖は
compile時に拒否し、対応済みとは数えない。inactiveな宣言も全sourceで型検証する。

現在のboot/source/control、worker generation、producer epoch、sequenceと一致し、元の観測時刻が
未来でなく、producer期限・supervisor観測期限が残るsampleだけを読む。pollやHTTP GETでは
観測時刻や期限を延長しない。unknownは保留中のholdを取り消し、asserted booleanを返さず、
retractionを捏造しない。保存済みUpは現在の有限観測の代用にならない。worker/controlの
authorityが変わった後は、古いUpをretraction待ちとして表示しない。

hysteresisはBOOTTIMEで両方向に適用する。runtimeは受理した通知ごとにpredicate truthと
authorityの連続性を記録し、投影間のflapやlease gapをまたいだhold成立を防ぐ。
同じtruthの連続更新は期限を保つ。評価は現在statusのsampled reconciliationであり、
すべての短いprotocol遷移を漏れなく履歴化する契約ではない。

producer stateと出力jobは同時保存する。新activationは初回出力を空journalにも保存できる。
無条件かつ同じ定義の宣言はcontrol再起動で初回eventを重複させず、現在値だけは取得し直す。
定義変更・削除・条件失効では旧activationの出力を公開しない。
自身の `when` / 依存条件の有効期限も現在値を制限する。

`show derived wan-derived` はread-onlyの `GET /v1/derived/{id}` を読み、state、元の観測時刻、
有効期限とpublication待ちを返す。inactive/unknownではassertedを省略する。
現在のinput identityと最大5秒のevaluator leaseも必要で、GETはleaseを更新しない。
保存/評価失敗は503、未宣言IDは404。履歴へのfallbackはしない。
`/v1/health` の `derived` はrevision、active件数、pending有無とbounded errorを持つ。
エラー時は不健全にし、confirmed trialの確定を防ぐが、commit済みsourceを捨てない。

最大128 DerivedEvent、全体で参照1,024個、1回の評価16宣言・publication8件のbounded処理である。
他provider、raw protocol event、作用の権限/idempotency、federation、retention、規模・性能受入は残る。
[実daemon・QEMUの証跡と範囲](evidence/2026-10-24-live-derived.md) を参照。

## graph、owner と検証

`depends_on` は同じ program の ID を参照する。
compiler は明示された辺に加え、program 内で管理する link/interface への型付き参照から必要な辺を補う。
重複 ID、複数 payload、参照先の欠落、自己依存、循環、同一 kernel object の二重宣言を拒否する。
同じ route slot の二重所有は、デフォルト table/metric や destination を正規化して判定する。

`owner` を省略した resource は自身の ID を owner とする。
指定する場合は同じ program 内の ID が必要で、owner の循環も拒否する。
`provenance` は任意の `source`、`resource`、`revision`、`path` を持つ記録である。
これらをファイル読み出しや executable 指定として解釈しない。

`when` と `ready_when` は `{fact: service.wan.active}`、`all`、`any`、`not` の型付き三値条件式を受け付ける。
`when` が true の resource だけを投影し、false / indeterminate の resource とその依存・owner subtree は保留する。
`ready_when` は自身の起動を止めず、依存先への準備完了を制御する。省略時は network transaction の成功が境界になる。
サービスのプロセス準備完了を依存条件にするときは、明示的に `ready_when: {fact: service.NAME.ready}` を指定する。

現在の実行時 fact は `service.NAME.ready` と `service.NAME.active`。
ready は supervisor の現在 generation / config digest と有限の観測期限を検査する。
active は DHCP4 / DHCP6-PD / VRRP / BGP / DHCP server の受理済み snapshot から作り、
Health の有限 sample も同じ active fact に変換する。
operator revision、boot ID、producer generation / epoch / sequence と CLOCK_BOOTTIME の期限を照合する。
未観測、期限切れ、worker 世代違いは indeterminate であり、`not` でも成功へ変換しない。
supervisorの生存中は、削除・再追加したworkerにも以前より大きいgenerationを割り当てる。
control workerの再起動によるserviceの停止・再取得でもgenerationを再利用せず、古い通知が新しい観測を上書きしない。
条件付き設定では protocol callback は観測の保存を行い、250ms の coordinator loop が
変更 worker の停止→network ACK→新 worker 起動の順で適用する。
確認付き commit は ready_when と適用済み projection の一致も検査する。

fact leafには `max_age` を指定できる。
例えば `when: {fact: service.health-a.active, max_age: 3s}` は、producer期限と世代が有効でも
観測から3秒を超えたsampleをindeterminateにする。ちょうど3秒は有効。
正の有限durationのみを受理し、`all` / `any` / `not` 自身への指定は拒否する。
Healthの観測時刻はproducerのCLOCK_BOOTTIME、その他の対応済みactive factは
新しいleaseを受理した単調時刻、ready factはsupervisorのpoll時刻である。
同じlease・sampleの再送や投影の再実行では観測時刻を更新しない。
`not` の内側でもstaleはindeterminateを保つ。
[実daemon・packet受入](evidence/2026-10-24-condition-max-age.md) を参照。

`for` は条件式が連続してtrueだった時間を指定する。
例えば `when: {fact: service.health-a.active, max_age: 3s, for: 2s}` は、
有効な観測を継続して得た状態で2秒経過してからresourceを起動する。
`for` は `all` / `any` / `not` のnodeにも指定でき、正の有限durationを必須とする。
待機中はindeterminate、既知falseは直ちにfalseであり、NOTでも待機を成功にしない。
偽や未確定、観測の期限切れ、producer・boot・source・operator revisionの交代で数え直す。
同値の観測更新や成功したkernel投影だけでは数え直さない。
protocol callback間に短いfalseを挟む場合も履歴に反映し、新しい通知なしでもtimerで解除する。
`when_hold` / `ready_when_hold` をstatusに表示する。
[安定時間の実daemon・packet受入](evidence/2026-10-24-condition-hold.md) を参照。

`show resources` で、coordinator が最後に選択したresourceごとの `active`、
三値の `when` / `ready`、保留理由と依存先を確認できる。
`when_false` / `when_indeterminate`、`dependency_false` / `dependency_indeterminate`、
`ready_when_false` / `ready_when_indeterminate` を区別する。三値のJSON表現は
`0=indeterminate`、`1=false`、`2=true`。`ready` は条件の評価値であり、独立したkernel readbackではない。
併記する `phase` / `last_error` と `show network` で適用結果を確認する。

APIは `GET /v1/services/resources`。ID順の最大128件と全件数、operator revision、
projection generation、`observation_id`、`next_offset` を返す。
継続ページには `offset=N&observation_id=DIGEST` を指定する。
readinessだけが変わってnetwork generationが同じ場合も、観測が変われば409で拒否する。
CLIは全ページを同じ観測で取得してから表示し、途中の409で混在した結果を出力しない。
source payload、provenance、秘密鍵、protocol snapshot本体はこのAPIに含めない。

source の inactive 部分も完全に検証する。一般 status reference、
Health の動的 targetSource、全 protocol の個別 readiness 契約はまだ残っている。
unknown field と再利用 DNS 型の非空 `when` は引き続き拒否する。
SSH candidate で `delete resources[OWNER]` を実行すると、明示 owner の子・孫 resource を連鎖削除する。残存 resource から削除先への `depends_on` があれば変更を拒否する。YAML 全体編集と API の任意の更新に同じ自動削除はまだ適用しない。

compiler の ownership 表は、生成された intent がどの resource に帰属するかを示す。
kernel に元から存在した object を採用する権限は与えない。
実物の削除可否は、引き続き network worker の既存 ownership journal と観測で判断する。

## 適用前に見る

```sh
routerd config validate startup-config.yaml
routerd config plan startup-config.yaml
```

`plan` は `native`、`order`、`dependencies`、`ownership` を JSON で出力する。
graph と native の検証をすべて終えてから出力するため、失敗した source の部分 plan は出さない。
kernel、worker、journal を変更せず、WireGuard などの secret file も解決しない。
入力文書が持つ値は出力に含まれるので、export と同様に設定情報として扱う。

resource の YAML 上の並びを変えても、同じ graph の実行順は決定的である。
ただし firewall rule、候補 endpoint、DNS source など、一つの payload 内で意味を持つ順序は保つ。
既存 typed builder のアドレス順は、IPv4 primary address の意味を変えないよう依存辺として表す。

## SSH の編集

```text
routerd> configure
routerd(config)# edit resources[wan].interface
routerd(config:resources[wan].interface)# set mtu 1400
routerd(config:resources[wan].interface)# top
routerd(config)# set resources[wan-address].address.cidr "192.0.2.10/24"
routerd(config)# diff
routerd(config)# validate
routerd(config)# commit confirmed 60
routerd(config)# confirm
routerd(config)# exit
```

resource の selector は `id`、その内部の名前付きリストは従来どおり各要素の `name` で指定する。
`resources[service/pd].service.dhcp6.delegations[lan]` のように組み合わせられる。
ID に `/` や `.` があっても selector 内では一つの identity として扱う。
Tab 補完、候補の分離、型検証、diff、YAML 全体編集、冪等 commit は同じ入口を使う。

## 保存、失敗、再起動

control の一つの transaction は次の順に進む。

1. source 全体を validate/compile し、request ID と operator revision を確認する。
2. source program と、確認付き commit なら直前の source program を pending record に保存する。
3. 変更対象の worker を停止し、完全な network projection を適用してから新しい worker を起動する。
4. 成功した source を authoritative journal に保存し、応答する。

適用失敗や保存失敗は、前の source から kernel と worker 構成を再生成して復元する。
確認期限が切れた場合と未確認のまま control が再起動した場合も、直前の source 全体を新しい operator revision として復元する。
pending record が残っているだけでは、その candidate が適用済みだと判断しない。
rollback 自体が失敗した場合は degraded を残す。

初回 seed は journal がない場合にだけ取り込む。既存 journal の破損や未知 schema を seed で上書きしない。
boot entry による任意 startup store/revision の選択、projection generation の永続復元、
多 binary runtime manifest の選択は別の残作業であり、この checkpoint では追加していない。

routerd2 の試作 `schema_version: 1` とその保存 record は読み込まない。
元製品 routerd の resource YAML を取り込む `config migrate` は別の入口で、今回から新 ResourceProgram を出力する。
その review mapping にある `network.*` / `services/*` は生成する engine intent の位置を示す。

## 試験対象

- `internal/resourceprogram`: graph、owner、正規化した二重所有、決定性、native intent 全種、8,192 routes、入力を変更しないこと。finite fact / 三値式の局所試験（boot/config/projection、epoch、sequence、expiry、欠落、並行アクセス）。
- `internal/config` / `internal/control`: source 保存、pending、request ID、確認期限、再起動 rollback、大きな record、prototype 拒否。
- `internal/runtimeplan`: source からの address/route 更新・削除、別 owner の保持、network ACK 前に worker を開始しないこと、worker 復旧。
- `internal/cli` / `cmd/routerd`: ID selector、typed edit、候補の分離、確認付き commit、plan の出力と失敗時の無出力。
- isolated daemon/QEMU: 本物の network/control worker、SSH、kernel reboot 後の source と projection の復元。

元 routerd の条件・依存・削除契約の根拠は [feature-contracts.md](feature-contracts.md) を参照。
この checkpoint は ResourceProgram の初期実装で、全 resource 契約の同等性を完了したものではない。
