# ResourceProgram と設定 transaction

routerd2 の設定は `schema_version: 2` の **ResourceProgram** である。
保存する正本は operator が編集した resource の宣言で、kernel の観測値や生成済み worker 設定ではない。
resource の ID、依存関係、owner、provenance と入力順を、そのまま candidate・running・確認付き commit の復旧用設定に保持する。

## 内部での設定の所有と投影

controlが受理した設定は、その設定世代の間は読み取り専用とする。compilerとruntimeは
同じ宣言を参照する。
設定変更では新しい宣言を渡す。HTTP/worker境界では従来どおりJSONをdecodeする。

設定受理時に依存graph、出力の所有権、EventRule/DerivedEventのprogramとidentityを
一度計算する。runtimeは宣言・Plan・digest・revisionを一つのconfiguration値で保持する。
観測更新の `Plan.Project` と `Plan.Lifecycle` はこのgraphを使い、設定を再コンパイルしない。
`Project` は選択されたNative入力と各resourceの状態を返す。
EventRule/DerivedEvent controllerも計算済みprogramを受け取り、同じrevisionとidentityなら
永続登録を再読しない。DerivedEventの実行期限は更新する。

worker設定、canonicalな子process用bytes、digestは、条件除外中のworkerや生成予定のRAも含めて
設定受理時に一度準備し、同じconfigurationに保持する。通常投影はその中から起動対象を選び、
設定を再生成・JSON化・hashしない。supervisorへ渡すidentityと保存する子process設定は
この同じbytesから得る。設定切替では新しいplanを準備し、継続可能な実workerと現在の観測を選ぶ。
BGP・VRRPの型付き設定、delegated RAの固定templateも再利用する。RAのprefix・期限・DNSは
観測由来のlive入力であり、immutable workerへ埋め込まない。

protocol投影ではnetwork intentをJSON化して複製しない。変更するinterface・address・
address options・routeの集合を所有し、追加だけを行うlink・tunnel・guardは元の配列へ
書き込まない。残りの固定設定は読み取り専用で共有する。writer内部のsnapshot複製には
JSON往復が残っている。

条件式のduration解析・hold identity計算、観測snapshotの型付き保持、読み取りと
continuity更新の分離は残っている。

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
| `egress_route` | 有効な候補の優先度で選ぶ一つの経路。同順位を kernel ECMP にまとめる |
| `policy_rule` | policy routing rule |
| `forwarding` | IPv4/IPv6 forwarding の管理 |
| `firewall` | 順序を持つ既存 ACL/NAT intent |
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

## Health と連動する経路選択

`egress_route` は一つの destination/table/metric を所有する。現在の条件が真の候補のうち、
`preference` が最大のものを選び、同順位の next hop を kernel ECMP へ渡す。
`next_hop.weight` は選択した集合内の相対重み（1〜256）であり、候補の優先度とは別である。
利用可能な候補が無ければ経路を撤去し、resource の理由を `no_ready_egress` とする。
別の選択 process や状態台帳は作らず、通常の型付き条件と network writer を使う。

以下は WAN address と、各 WAN に拘束した Health サービス・probe 用経路を既に宣言した構成の断片である。
完全な再実行用 setup は `internal/integration/egress_route_test.go` にある。

```yaml
- id: internet-egress
  egress_route:
    destination: 0.0.0.0/0
    table: 100
    metric: 100
    candidates:
      - preference: 120
        when: {fact: service.health-a.active, for: 300ms}
        next_hop: {interface: wan0, gateway: 192.0.2.1, weight: 1}
      - preference: 120
        when: {fact: service.health-b.active}
        next_hop: {interface: wan1, gateway: 203.0.113.1, weight: 2}
- id: lan-egress-rule
  policy_rule: {family: 4, priority: 1000, table: 100, source: 198.18.0.0/24}
```

candidate の `when` は通常の有限 fact・`all`/`any`・`max_age`・`for` を使う。
`for` は回復後の継続条件で、旧 advisory selector の最終切替時刻に基づく hysteresis とは異なる。
選択した候補の元期限の最小値を経路へ渡し、control が止まっても writer がその期限で撤去する。
新しい観測が無いまま writer を再起動しても期限を延長しない。
route と rule の所有は別であり、table を指定しただけで policy rule は生成しない。

compiler contract は native-47、network journal は12。既存 route の ECMP 実装を使う。
`make check-health-pbr` は独立二 WAN・実 worker・Linux kernel と capture で、重み1:2、
新規 TCP/UDP、障害除外・復旧、上位 preference、confirmed rollback、control/Health 停止中の
writer 交換と元期限撤去を確認した。今回の動的経路選択は IPv4 で実行した。
既存接続継続、IPv6 動的選択、policy ごとの mark/connmark hash、旧 EgressRoutePolicy 全体の import と実 NIC は未検証・未完了である。

### IPv4 の送信元専用 hash

`forwarding.ipv4_ecmp_hash: source` は、その network namespace の **全 IPv4 ECMP 経路**を
送信元アドレスだけで振り分ける。同じ送信元の宛先・TCP/UDP・ポートが変わっても、
同じ候補集合の中では同じ WAN を使う。rule ごとに別の hash を選ぶ機能ではない。

```yaml
- id: forwarding
  forwarding:
    ipv4: true
    ipv4_ecmp_hash: source
```

Linux の custom hash policy と source-address field mask を network writer が所有する。
有効化では mask → policy、解除では元 policy → 元 mask の順で適用し、元値を既存 journal に保持する。
空欄は管理しない指定であり、解除時は他者が変更していない元値を復元する。
再起動・失敗時の書込み記録と復旧も既存の setting owner を使う。旧試作形式の reader は追加しない。

PVE Linux6.8/amd64 の実 worker、独立二 WAN と三つの宛先で、送信元ごとの TCP/UDP 経路固定、
両 WAN の利用、障害切替・復旧、kernel 値の解除/rollback 復元、writer 交換と元期限撤去を確認した。
試験の capture 上限は4096 frameを維持し、必要な両経路を確認した後は同じ通信を増やさない。

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

対応するproviderはHealthCheck・BFD・BGPRouter。HealthCheckのscalar fieldは `active`、`phase`、`lastResult`、
`lastCheckedAt`、`lastTransitionAt`、`consecutivePassed`、`consecutiveFailed`、`failureCount`、
`message`、`lastSuccessTime`、`lastFailureTime`、`firstFailureTime`。
観測済みの空optional fieldと観測なしは区別する。
BFDは `phase`、`peerStates`、`peers`、`observedAt` の4 fieldを読み、例えば
`${BFD/service/edge-bfd.status.phase}` に `equals: Up` を指定する。
`phase` は `Up` / `Down` / `Degraded` / `Pending`、`peerStates` は旧形式のmap文字列、
`peers` は旧形式のpeer map文字列を要素とするJSON配列、`observedAt` はUTC RFC3339Nanoである。
peerごとの構造化selector、他Kind、DerivedEvent連鎖はcompile時に拒否する。
inactiveな宣言も全sourceで型検証する。

BFDのlocal状態観測は元のBOOTTIMEから最大5秒で失効する。Up/Initを含む観測はさらに
全該当peerの元パケット受信期限とBFD producer自身の元観測期限で制限し、
現在のsupervisor観測期限も超えない。BFD→BGPには別のHTTP受信leaseを作らず、
短いpoll失敗だけでは有効な元の観測を取り消さない。配送が止まれば元の期限で失効する。
古いraw `UP` の失効はunknownとなり、Down eventを捏造しない。
通知を集約した間のflapはpeer遷移counterでholdをリセットする。全peerのcounterを使うため、
集約phaseが同じでもpeerの変化で保守的にholdをやり直す。counterが飽和した観測はunknownになる。
値は1 field最大64KiB・snapshot全体2MiBに制限し、元の大きな値をdurable producer stateへコピーしない。

BFD `observedAt` の値変更はworkerで通知集約の前に数える。`equals`（`any_of`内も含む）は
この記録を使い、途中で不一致になってから元の時刻へ戻る場合や、別の一致時刻へ移る場合に古いholdを引き継がない。
`notEmpty`だけなら定期更新をまたいでholdを継続できる。同じ参照を値比較と存在確認の両方に使う場合は
厳しい値比較側を選ぶため、集約truthが変わらなくても保守的にholdをやり直すことがある。
時刻変更counterの飽和では `observedAt` はunknownになり、独立したphaseの有限証拠は保持する。

BGPRouterはnative BGP serviceの `phase`、`establishedPeers`、`backend`、`observedAt` を読む。
例は `${BGPRouter/service/edge-bgp.status.phase}` に `equals: Established` を指定する。
宣言した全peerの観測が必要で、全Establishedなら `Established`、一部なら `Degraded`、
全非Establishedなら `Down`、peer宣言なしなら `Pending`。全Establishedでも未解決FIBは `Degraded` となる。
`backend` は `gobgp`、`establishedPeers` は数の文字列、`observedAt` はUTC RFC3339Nanoである。

元のBGP経路/BFD依存内容と同じ投影のnetwork ACKが必要で、変更した経路の適用待ちはunknownになる。
経路内容が同じheartbeatはACKを再利用し、holdを毎回取り消さない。
観測時刻はengine walkの前にBOOTTIMEで固定し、元の最大5秒とfresh supervisor期限を超えない。
UTCの補正、遅いwalk/apply、同じ観測の再送や表示で期限を延長しない。
停止したworkerのraw Establishedは残っても、現在値はunknownとなり、Down eventを捏造しない。
BGP通知は最初の1件から現在のsupervisor instance/worker generation/digestと照合する。

`acceptedPrefixes` を選択済みFIB候補数で代用しない。受理RIB、構造化peer/prefix、
再収束/GR、他の旧BGPRouter/BGPPeer fieldとresource契約は未実装であり、未対応参照はcompile時に拒否する。
これは現在のnative session/FIB観測のproviderで、BGP全体のsemantic parityではない。

現在のboot/source/control、worker generation、producer epoch、sequenceと一致し、元の観測時刻が
未来でなく、producer期限・supervisor観測期限が残るsampleだけを読む。pollやHTTP GETでは
観測時刻や期限を延長しない。unknownは保留中のholdを取り消し、asserted booleanを返さず、
retractionを捏造しない。保存済みUpは現在の有限観測の代用にならない。worker/controlの
authorityが変わった後は、古いUpをretraction待ちとして表示しない。

hysteresisはBOOTTIMEで両方向に適用する。runtimeはHealth通知ごとのpredicate truthと
authorityの連続性、およびBFDの独立cacheの遷移counterを記録し、投影間のflapやlease gapをまたいだhold成立を防ぐ。
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
BFDのraw packet/遷移をすべて永続履歴へ保存するproducerは別の未実装範囲である。

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
active は DHCP4 / DHCP6-PD / VRRP / BGP / DHCP server / PPPoE / AFTR の受理済み snapshot から作り、
Health の有限 sample も同じ active fact に変換する。
operator revision、boot ID、producer generation / epoch / sequence と CLOCK_BOOTTIME の期限を照合する。
VRRPは元のObservedNanos/ExpiresNanosをfactとVIP address lifetimeへ渡し、UTCのvalid_untilは表示だけに使う。
BACKUPも1秒ごとに有限なfalse観測を更新し、INITIALIZE後は同じproducer世代からのActive再送を拒否する。
このVRRP authority受入は無条件serviceの実daemon試験であり、条件付きVRRP callbackのkernel ACK順序やHA-01全体の受入ではない。
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

`for` は条件式が連続してtrueだった時間を指定する。
例えば `when: {fact: service.health-a.active, max_age: 3s, for: 2s}` は、
有効な観測を継続して得た状態で2秒経過してからresourceを起動する。
`for` は `all` / `any` / `not` のnodeにも指定でき、正の有限durationを必須とする。
待機中はindeterminate、既知falseは直ちにfalseであり、NOTでも待機を成功にしない。
偽や未確定、観測の期限切れ、producer・boot・source・operator revisionの交代で数え直す。
同値の観測更新や成功したkernel投影だけでは数え直さない。
protocol callback間に短いfalseを挟む場合も履歴に反映し、新しい通知なしでもtimerで解除する。
`when_hold` / `ready_when_hold` をstatusに表示する。

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

## protocol所有interfaceを使うworker

VRRPのVIP interface、PPP unit、DS-Lite tunnelを他のserviceで使う場合、consumerから
所有serviceのresourceへの直接の`depends_on`または`owner`を指定する。
さらにconsumerの`when`、または依存先の`ready_when`が、対応する
`service.<name>.active`の真を必須にしなければならない。
`ready`だけではBACKUPも含むため、interface所有権の条件にならない。
`all`・`any`・`not`は三値論理で検査し、無関係なOR分岐だけで起動できる条件は拒否する。
consumerはinterface/address/UPを再所有しない。static link/interface/routeの所有権制限は継続する。
AFTRのactiveは現在のsupervisor登録と元のsource/DNS期限で制限する。変更のないstatic
endpointへ架空のheartbeatや受信時刻のleaseを加えない。DS-Lite tunnelを別のDS-Liteの
underlayには使えないが、所有者に従うHealth等のconsumerは使える。

条件付きVRRPの通知は受理だけでは成功にならない。coordinatorは依存workerの
終了を確認してからnetwork workerへVIP/VMACを適用・撤去し、そのACKを待って
通知元へ応答する。初回Ready待ちと最終終了待ちの間もこの処理を進める。
ACTIVE成功には元boot IDと絶対期限を保持するVIP投影が必要であり、期限切れ・
旧世代・上書きされた通知や適用失敗を成功として返さない。

このsource受入範囲の追加に伴い、署名されたconfiguration contractは
`resource-program-native-24`とする。PPPの有限`.active` factで依存workerを起動し、PPP unitの
UP/addressはpppdが所有する。`forwarding.ipv4_rp_filter`はLinux all/defaultを明示して管理する
（0:無効、1:strict、2:loose、省略:管理しない）。撤去時は記録した元の値へ戻す。
PPPは交渉済みIPv4とIPv6 link-localを別々に観測し、有効なfamilyの既定経路をwriterが管理する。
`pppoe.default_route`（省略時true）・`table`・`metric`は両familyに適用し、IPv6CPが未完了ならIPv6既定経路を作らない。
PPP上のDHCPv6 clientも同じ永続identityを使い、Ethernet MACがない場合の自動生成はDUID-UUIDとする。
BFDの明示`multihop: true`はinterfaceを指定せず、
宣言済みの送信元アドレスとUDP 4784のrouted endpoint pairを使う。single-hopは従来の
interface・隣接route・TTL/Hop Limit 255の検証を維持する。BGPのmultihop参照も同じ
endpoint pairへ束縛し、両transportはdevice bindingを指定しない。条件付きlink・address・routeと依存先の準備条件の
元のboot期限をwriterへ渡す。期限切れのowned linkをDOWNにし、管理address・routeを撤去する。
address／routeを直接条件付きにした場合も物理interface上で失効する。
この内部期限はoperator resourceへ記述できない。network journalはformat 9とする。
VRRPの明示version、IPv4 v2の秒単位広告・
任意のPASS認証、unicast peer、宣言resource IDを参照するtrackのpenalty・連続回数・
元の観測のmax ageを受け入れる。現在のtrack providerはHealth・BFD・BGPで、
`when`が明示falseなら有限のneutralとして直前の判定・回数を保持する。
indeterminateと期限切れはunknown／penaltyに戻す。参照は宣言全体で検証し、
条件付きproviderが投影から除かれても選挙は継続する。tracking判定は実適用応答で返し、
worker設定の再生成や再起動で優先度を変更しない。設定と応答の両端を同じ契約へ置き換える。
以前のprototype compilerとの更新・fallbackは
既存のABI検査で拒否する。format migrationや旧compilerとの互換性を表す変更ではない。


元のprotocol通知は期限切れでも書き換えない。状態APIの`snapshot`はpublisherの最後の観測であり、
現在の利用可否とは別である。BGP・VRRP・DHCP serverの`ownership_expired`は現在の期限判定を示す。
DNS出力・条件・経路はそれぞれ現在有効な観測だけを使う。BGPとDHCP serverは元のBOOTTIME期限を使う。
DHCP serverの型付き観測から、元のboot ID・観測時刻・期限をそのままDNS workerへ渡す。
DNSはA/PTR応答時に期限と個別clientのUTC lease期限を確認し、TTLを短い方で制限する。
controlにもDNSにも受信側で作る所有期限はない。個別clientの割当・保存のUTC依存は残る。

## 追加MACとインターフェースの所有期限

追加WAN/LAN MACは通常の`link`・`interface`・`address`を使う。
[設定例](../examples/vrrp-additional-identity.yaml)では`service.ha.elected`で固定MACと
link-local addressを準備し、`service.ha.active`でRAを公開する。
linkへの依存はinterface/address/consumerへ推論され、依存先の`ready_when`も期限を制限する。
resource自身の`ready_when`はそのresourceの起動を妨げない。

writerは期限切れlinkをDOWNにして、そこで管理するaddressとlocal routeを撤去する。
address自身の期限が短ければそちらを保持し、静的なaddressもlinkの所有期限を越えない。
設定の再生成・worker再起動でこの期限を延長しない。実行中のnetwork workerを必要とし、
writer停止中のlink fencingや物理suspendを保証するものではない。

address／route自身の`when`と依存先の準備期限もwriterへ渡す。addressのpreferred/valid
lifetimeを条件の元の期限で制限し、UTC指定は投影時の対応する時計でBOOTTIMEへ写す。
自身のvalid期限も依存先へ渡し、期限切れは`lifetime_expired`として非activeにする。
条件が現在もtrueでも、期限切れのaddressが後続routeやworkerを起動することはない。
IPv6 routeの明示`valid_until`はkernel timerとしても残し、条件のBOOTTIME期限と両方を満たす。
controlとwriterは同じlifetime投影を使い、意図した期限切れを適用待ちとして残さない。
Ready/confirmed commitは実際にACKされたlink・address・routeの期限で判定し、
新しい準備観測だけで古い実適用を成功扱いにしない。所有期限の更新もprojection generationを
進めるが、operator revisionは変更しない。

## VRRPの選出とVIP公開のreadiness

VRRP固有の`service.vrrp.graceful_activation.ready_when`はVIP公開の入場条件を
指定する。resource自体の`ready_when`による依存先の準備完了とは別の役割を持つ。

```yaml
service:
  name: ha
  vrrp:
    interface: lan0
    family: ipv4
    virtual_router_id: 42
    addresses: [192.0.2.254/24]
    graceful_activation:
      ready_when: {fact: service.uplink.active, for: 1s}
      timeout: 45s
```

選出がMASTERでも条件がfalse/indeterminateならVIPを公開しない。VRRPの選出広告は
継続するが、VIPのGARP/NAは公開完了後だけ送る。timeoutは状態報告だけを変え、
後でreadinessが回復したら同じ有効な選出から再試行できる。
公開済みの選出ではreadiness悪化だけでVIPを取り下げない。role喪失、worker世代変更、
条件によるresource除外、元所有期限の失効は引き続き撤去理由になる。

`service.NAME.elected`は有限の選出fact、`service.NAME.active`はVIP公開側のfactである。
初回公開をnetworkがACKするまではactiveを真として扱わない。
自身の未公開activeを必須にする循環した入場条件はcompile時に拒否する。
一般の旧status referenceや全cluster/track契約の移植完了を意味しない。

固定DS-Lite tunnelも`link.kind: ip6tnl`として`parent`・`local`・`remote`を指定する。
MTU・B4 addressはinterface/address、経路はrouteで表す。動的AFTR serviceも同じowned linkを生成し、
元のAFTR期限でwriterがDOWN・管理address/route撤去を担当する。内部`link.authority`はoperator宣言できない。

IPv6 routeの`valid_until`は運用者の絶対期限なら`{utc: "2030-01-01T00:00:00Z"}`とする。
学習したRAは同じ型で元のboot ID/nanosecondsを渡すが、operator resourceはboot期限を指定できない。
writerは元のauthorityとkernelの相対timerを区別し、保存したCLOCK_MONOTONICのtimerで所有権を確認する。
長い期限のkernel grant更新はwriterが担当し、元のprotocol期限は変更しない。

## IPv4-in-IPv6 tunnelのMTU

owned `ip6tnl`（DS-Liteを含む）は、bridge/VRFへの所属設定と下位interfaceのMTUを適用した後に
`min(parent MTU, 65535) - 40`を上限とする。tunnel interfaceの明示`mtu`は、さらに小さい
上限として扱う。省略時も現在のparentから導出し、下位MTUの変更とrollbackへ追従する。
preexisting/unmanaged tunnelのMTUをこの規則で変更しない。writerはlink所有権を確認してtunnelのMTUを適用し、変更後にMSS/DF処理を計算する。
tunnelは撤去時にlink全体を削除するため、MTUだけの復元記録を重ねない。下位interfaceの明示MTUは
既存のsetting journalで復元する。network ownership journalは9とし、旧試作形式のreader/migrationは作らない。
新しいMTU cacheやmanagerはない。
AFTRのsource読み取りでwriter RPCが失敗した場合、新しいsource消失inputには変換しない。
元のsource/candidate/DNS期限は延長せず、実際に観測したsource消失/DAD失敗と期限切れを適用する。
IPv4-in-IPv6なので、下位IPv6 MTU1280に対する内側MTUは1240となる。

Linuxはtunnel作成時にMTUを導出するが、この試験では下位MTU変更時に1460のまま残った。
独立Linux AFTRとの小packet/Healthは成功しても、1MiB downloadが停止した。
修正後の実daemonは1500→1280→1500の設定切替でtunnel1460→1240→1460へ追従し、
同じtunnel index・同じAFTR workerでIPv4/IPv6の1MiB双方向TCPとIPv4 UDP1450が通った。
IPv6 UDPは実Packet Too Big/MTU1280を受信し、その後の1200-byte UDPが通った。
`make check-dual-wan`で実行する。`make check-dual-wan-bridge`はMTU1400のmemberを持つ
owned bridgeをWANにし、同じ通信と1280への変更・1360へのrollbackを確認する。
中間routerが作る、NIC MTUよりも小さいroute PMTUや
WireGuard/VXLAN等の全encapsulationへの適用は未受入である。

## TCP MSSとpath MTU

`firewall.forward`と、入力・出力interfaceを両方指定した`forward_chain`のaccept ruleは、
そのpairの両方向でTCP SYN/SYN-ACKの大きいMSSだけを下げる。後者はprotocol未指定/TCPを対象とし、
指定したfamilyを維持する。writerはlink・interface設定後のkernel MTUを一度読み、両端の小さい方から
IPv4は40、IPv6は60を引く。小さいMSSは増やさない。転送を許可する範囲は元のfilterが決める。
PPPの交渉済みMTUやDS-Liteの現在のMTUを、別のMSS設定へ重複記載する必要はない。

```yaml
id: firewall
firewall:
  forward: [{from: lan0, to: ppp-wan}]
```

別のpath上限が必要な場合や、interface pairを限定しないaccept policyを使う場合は、
`firewall.mss_clamp`で`from`・`to`（一方向）と`family`（4/6）を指定する。
任意の`mtu`は実interface MTUよりも小さい上限として使う。省略するとkernel MTUから導出する。
明示ruleは一方向であり、両方向へ必要ならそれぞれ指定する。自動ruleと重なる場合は小さい上限を使う。
まだない動的linkには自動ruleを作らず、link生成後の通常適用で導出する。
導出値はnft batchだけで使い、operator intentやjournalへ書き戻さない。
IPv4 MTU576未満・IPv6 MTU1280未満の自動clamp、bridge-family、route固有のPMTU、
VRF/iif条件付きDF lookup、物理NICと性能の受入は未完了である。

owned inet tableのforward/mangle hookを、他のfirewall変更と同じnft batchで交換する。
interface MTU、UDPのDF、通常のPMTU探索はMSS処理では変更しない。
`disable_forward`はforward filter hookを省略する指定で、明示したMSS/DF/NAT hookは有効である。
固定版routerdでは`PathMTUPolicy`自体が廃止済みであり、その旧resourceのreaderは追加しない。

`make check-dual-wan`は独立RP-PPPoE AC/Linux AFTRと、その先のMTU-1500端末を使う。
製品MTU1454に対してACがMRU1400を交渉し、IPv4のMSSとDF閾値を記載せず、
PPPのMSS1360・DS-Lite1420、IPv6の明示path上限による1400、元の1200維持をcaptureで確認した。
上流がIPv4 PMTU通知を遮断する構成でも、両WANの1MiB download/upload、切替・復旧が通った。
ACのpppd起動引数末尾へMTU/MRU1400を指定する。RP-PPPoE 3.15自身がoptions fileの後に
1492を追加するため、file内だけの指定では実際のLCPが1492だったことをcaptureで確認した。
使用する独立protocol engineは変更していない。

## IPv4 DF解除とkernelのfragment

`firewall.ipv4_clear_df`は`from`（入力interface）と`to`（FIBが選ぶ出力interface）で
DF解除を明示的に選ぶ。閾値は出力のkernel MTUから導出し、任意の`mtu`でさらに小さくできる。
global RPDBの宛先lookupへpacketの送信元と現在のmarkを渡し、そのpathで閾値を超えた
未fragmentのDF付きIPv4だけを変更する。DNAT後・forwardのMTU判定前に適用し、
IPv4 header checksumとfragmentはkernelが処理する。

```yaml
ipv4_clear_df:
  - {from: lan0, to: ppp-wan}
  - {from: lan0, to: dslite0}
```

これはinterface MTUを変更せず、自前でpacketを分割しない。実際の出力MTUを超えたpacketは
kernelがfragmentする。受信済みfragment、閾値以下、別の入力／FIB出力、IPv6は変更しない。
既定ではDFを解除しない。VRF・iif条件付きRPDBは未受入であり、global source/markの試験と分ける。

`make check-dual-wan`は接続UDPと`IP_PMTUDISC_PROBE`を使って1450-byte payloadを送る。
PROBEは学習済みPMTUを無視して実際にDF付きpacketを送出するためのLinux socket設定である。
PPP1400では1396+102、DS-Lite1460では1460+38-byteへ分割され、再構成した全payloadが往復した。
source policyがDS-Liteを選ぶ間も、mark41のPPPを選ぶ1458-byte DF packetが通った。
独立mark tableはowned table交換後も保持され、AFTR worker再起動後のUDPも復旧した。
Alpine kernel 6.18.58-0-virtでも1454→1400の再適用後にTCP MSSと1450-byte UDPを確認した。
