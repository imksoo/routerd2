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

`interface.addresses` と `interface.address_options` には設定しない。
アドレスは独立した `address` resource とし、options は `address.options` に置く。
これにより同じ interface の設定と複数のアドレスを別の宣言へ帰属させられる。
明示的な `interface` resource がないアドレスは、必要最小限の UP 設定を生成する。
ほかの native field と protocol の制約は引き続き検証する。

## graph、owner と検証

`depends_on` は同じ program の ID を参照する。
compiler は明示された辺に加え、program 内で管理する link/interface への型付き参照から必要な辺を補う。
重複 ID、複数 payload、参照先の欠落、自己依存、循環、同一 kernel object の二重宣言を拒否する。
同じ route slot の二重所有は、デフォルト table/metric や destination を正規化して判定する。

`owner` を省略した resource は自身の ID を owner とする。
指定する場合は同じ program 内の ID が必要で、owner の循環も拒否する。
`provenance` は任意の `source`、`resource`、`revision`、`path` を持つ記録である。
これらをファイル読み出しや executable 指定として解釈しない。

現段階の graph は **コンパイルの依存グラフ**である。
一つの network transaction が成功した後に worker を起動する境界は維持するが、
任意の service の readiness を `depends_on` で待つ coordinator はまだない。
`when`、`ready_when`、三値条件、hold time/freshness、動的な owner cascade は CORE-03 / CORE-04 の残作業である。
resource の未実装 field は unknown field として拒否し、再利用した DNS 型の非空 `when` も明示的に拒否する。
owner を削除するときは、その owner を参照する resource も candidate から明示的に削除する。

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
boot entry による任意 startup store/revision の選択、独立した projection generation の公開、
多 binary runtime manifest の選択は別の残作業であり、この checkpoint では追加していない。

routerd2 の試作 `schema_version: 1` とその保存 record は読み込まない。
元製品 routerd の resource YAML を取り込む `config migrate` は別の入口で、今回から新 ResourceProgram を出力する。
その review mapping にある `network.*` / `services/*` は生成する engine intent の位置を示す。

## 試験対象

- `internal/resourceprogram`: graph、owner、正規化した二重所有、決定性、native intent 全種、8,192 routes、入力を変更しないこと。
- `internal/config` / `internal/control`: source 保存、pending、request ID、確認期限、再起動 rollback、大きな record、prototype 拒否。
- `internal/runtimeplan`: source からの address/route 更新・削除、別 owner の保持、network ACK 前に worker を開始しないこと、worker 復旧。
- `internal/cli` / `cmd/routerd`: ID selector、typed edit、候補の分離、確認付き commit、plan の出力と失敗時の無出力。
- isolated daemon/QEMU: 本物の network/control worker、SSH、kernel reboot 後の source と projection の復元。

元 routerd の条件・依存・削除契約の根拠は [feature-contracts.md](feature-contracts.md) を参照。
この checkpoint は ResourceProgram の初期実装で、全 resource 契約の同等性を完了したものではない。
