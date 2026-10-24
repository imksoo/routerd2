# routerd2

Linuxカーネルの上で動く、モジュール化されたルーターの実装です。
`routerd` 自身を PID 1 とし、ネットワーク設定・制御・SSH管理を別プロセスに分けます。
Alpineのパッケージをビルドとイメージ組み立てに利用します。

**開発中です。現行 [routerd](https://github.com/imksoo/routerd) の全機能互換は移植の目標であり、現時点の完成状態ではありません。**
移植元の基準は `1329ef54f296507eda79bbbc3b853783dd27251e` です。
既存のhomert02/03を置き換える前に、WAN取得・HA・既存設定との互換試験が必要です。

## 実装されている機能

| 層 | 実装 |
| --- | --- |
| init / supervisor | 明示的な appliance・container・host モード、PID 1、子プロセス回収、終了処理、backoff、heartbeat、同一 release 検証、設定に応じたプロトコルプロセスの追加・置換・停止 |
| worker runtime | `routerd-modules` を役割とインスタンスごとの別プロセスで起動。専用 lifecycle FD、世代・設定 SHA-256・producer epoch による古い通知の拒否 |
| logging | journald を使わない独立ログプロセス、構造化ログ、回転・復旧、bounded queue、console fallback、appliance の `/dev/kmsg` 収集。ログ障害を必須ルーティングプロセスの停止条件にしない |
| network | Netlink による IPv4/IPv6、有限アドレス寿命、静的経路・ECMP、policy routing、bridge/VLAN/VRF/VXLAN/macvlan、MTU・リンク・forwarding。nftables の INPUT/OUTPUT/FORWARD ACL、DNAT/SNAT/masquerade とポート範囲 |
| WAN | native DHCPv4、DHCPv6 IA_PD / Information-request、DUID/IAID とリースの永続化、renew/rebind/失効。WAN RA クライアント、SLAAC、default/RIO/on-link routes、RDNSS/DNSSL。PD から LAN /64 を割り当て、旧 prefix を deprecated で維持 |
| DHCP server | native DHCPv4、scope・予約・sticky lease、ARP conflict probe、信頼する relay の限定、永続 journal を保存してから ACK。worker 再起動後の INIT-REBOOT / renew に対応 |
| RA | native ICMPv6、RS 応答、RDNSS/DNSSL、絶対寿命、PD 更新時に同じプロセス・socket で prefix を変更。削除した prefix だけを withdraw |
| DS-Lite | kernel IPv4-in-IPv6、B4 アドレス、IPv4 route。明示した RA/DHCPv6 依存から AFTR を IPv6 DNS で解決し、source・情報・DNS TTL の期限に従って接続・更新・撤去 |
| VRRPv3 | native IPv4/IPv6 election、VMAC、GARP/NA、preempt、優先度、有限 ownership lease、NoDAD の明示 VIP、Accept_Mode の入力制御 |
| DNS | 現行 routerd の resolver engine を再利用。UDP/TCP サーバー、cache、zones、query log、UDP/TCP/TLS/HTTPS/QUIC upstream。型付き DHCP リースによる A/PTR を、listener 再起動なしで更新・失効。虚偽の AD bit を防ぐため未実装の local DNSSEC signing は拒否 |
| BGP | GoBGP v4 を独立 worker に統合。IPv4/IPv6 unicast、policy、選択経路の kernel 投影、recursive next hop、ECMP、期限付き FIB ownership。未解決 next hop は状態に表示し、未解決のまま FIB に入れない |
| BFD | native IPv4/IPv6 single-hop asynchronous BFD。BGP の明示した隣接 peer を管理し、受信期限・producer 世代・Down counter で実セッションと学習経路を制御。旧 BFD profile、multihop、認証・echo は別の未実装範囲 |
| WireGuard | kernel WireGuard を generic netlink で設定。完全な peer set、IPv4/IPv6 endpoint、roaming、公開 handshake/counter 状態。秘密鍵は versioned file と immutable snapshot で管理し、失敗時は所有リンクを DOWN にして復旧 |
| configuration | YAML / JSON の厳密な検証、candidate、revision・冪等 request ID、永続 journal、プロセス構成と kernel projection の適用、失敗時の旧設定への reconcile |
| confirmed commit | 確認期限で rollback。未確認のまま control が再起動した場合も復元。復元失敗を成功として扱わない |
| management | 内蔵 SSH、公開鍵認証、永続 host key、Tab / history 付き候補編集、YAML 一括編集、diff、validate、commit confirmed、ログ参照。OS シェルを呼ばない |
| large observations | 大きな設定の revision 固定読取・確認応答、軽量 health、network summary と詳細ページ。経路・WireGuard の大きな観測を省略するときは件数・省略フラグを明示し、CLI から詳細を取得 |
| migration | `routerd config migrate [--policy FILE] FILE` で旧 resource YAML を変換。静的 DHCPv4 server/reservation、DHCP-derived DNS、静的 WireGuard も対応範囲を検証して変換。WAN と DHCP DNS の動作変更は resource ごとの明示 policy と review notes で扱う。未対応入力は理由を列挙し、部分的な設定を出力しない |
| persistent state | appliance PID 1 が明示された既存 ext4 データ領域を mount・検証し、設定・SSH host key・署名付きリリース履歴を保存。起動時にディスクを検出・初期化する処理は含めない |
| distribution | static amd64/arm64 の 2 バイナリ、依存ライセンス manifest、scratch OCI、Alpine APK、kernel + initramfs、QEMU と namespace 統合試験 |
| release | Ed25519 署名・SHA-256・ELF/architecture/release identity の検証、immutable な二つの実行ファイルの世代保存、next-boot selection、試行回数・health confirmation・rollback journal。kernel/rootfs の A/B 更新は別の未実装範囲 |

アプライアンスの通常起動には systemd、journald、sshd、OpenRC、`ip`、`nft`、シェルを使いません。
パケット転送と IPv4/IPv6、conntrack、NAT、トンネルは Linux カーネルが処理します。
routerd は設定、プロトコル、監視、ログ、管理とその整合性を受け持ちます。

詳細は [アーキテクチャ](docs/architecture.md)、[現行 routerd の機能棚卸し](docs/parity.md)、
[WAN と AFTR の契約](docs/wan.md)、[WireGuard](docs/wireguard.md)、[BFD と BGP](docs/bfd.md)、[署名付きユーザーランド更新](docs/releases.md)、[設定例](examples/) を参照してください。機能棚卸しの `semantic_parity` は個々の旧 resource 契約の互換性を示し、
プロトコル実装やビルドの成功だけでは true にしません。

## Build and test

Go 1.27.3、Linux、makeを使用します。race testにはCコンパイラも必要です。

```sh
make check
make build
make build-linux
./dist/bin/routerd version
./dist/bin/routerd config validate images/config-empty.json
```

通常のテストはホストのネットワークを変更しません。
Unix socket・localhostのSSH・テスト用の子プロセスを利用するテストはあります。
カーネルとの統合試験は明示的なbuild tagと隔離network namespaceを使います。

空の設定で開発モードを起動する例です。

```sh
mkdir -p /tmp/routerd2-dev/run /tmp/routerd2-dev/state
./dist/bin/routerd run \
  --platform host \
  --module-path "$PWD/dist/bin/routerd-modules" \
  --config "$PWD/images/config-empty.json" \
  --runtime-dir /tmp/routerd2-dev/run \
  --state-dir /tmp/routerd2-dev/state
```

別ターミナルで確認できます。

```sh
./dist/bin/routerd status --runtime-dir /tmp/routerd2-dev/run
./dist/bin/routerd config show --runtime-dir /tmp/routerd2-dev/run
./dist/bin/routerd modules restart network --runtime-dir /tmp/routerd2-dev/run
```

ネットワークの変更には対象namespaceの権限が必要です。空の設定はインターフェースを設定しません。
既存設定があるホストのnetwork namespaceで、ルーター用のサンプルを不用意に適用しないでください。

## Appliance and container

```sh
make container-smoke
make apk
make image
make qemu-smoke
```

詳細は [images/README.md](images/README.md) と
[packaging/alpine/README.md](packaging/alpine/README.md) を参照してください。
現行のQEMUイメージはdirect boot用initramfsです。事前に用意した ext4 データ領域を
[`boot.state_volume`](internal/runtimeconfig/README.md) で選び、設定 journal・SSH host key・リリース履歴を永続化できます。
データ領域を指定せず initramfs だけで動かす場合、設定と SSH host key は再起動で消えます。
A/B bootloader、データ領域の作成、AMIや各クラウドのGolden Imageはまだ含みません。
コンテナでは `/var/lib/routerd` を永続volumeに置いてください。

Alpineのmuslがカーネル転送経路に挟まるわけではありません。両Goバイナリは `CGO_ENABLED=0` でビルドします。
ただし、Ubuntuと同じ性能を測定済みという意味ではありません。NIC、カーネル設定、offload、IRQ、conntrack等を揃えた実機比較は別の検証項目です。

## SSH

`images/boot.json` の `ssh.authorized_keys` に公開鍵を指定します。
鍵がない場合、SSH listenerは起動しません。パスワード認証はありません。

```sh
ssh admin@ROUTER show status
ssh admin@ROUTER show network
ssh admin@ROUTER show config
ssh admin@ROUTER show version
ssh admin@ROUTER restart network
```

対話セッションでは候補設定を編集します。実行中設定は commit まで変わりません。

```text
routerd> configure
routerd(config)# edit network.interfaces[eth0]
routerd(config:network.interfaces[eth0])# set mtu 1500
routerd(config:network.interfaces[eth0])# top
routerd(config)# diff
routerd(config)# validate
routerd(config)# commit confirmed 60
routerd(config)# confirm
routerd(config)# exit
routerd> show services
routerd> show services wan-ra
routerd> show route-issues
routerd> show network routes table 254 limit 64
routerd> show network wireguard-peers interface wg0
routerd> show logs module service.wan limit 50
```

`set` の値は JSON、パスのリスト要素は名前または番号で指定します。
`edit running-config.yaml` は候補全体の YAML を表示し、置換文書を貼り付けて `.end` で検証します。
`.abort` で編集を取り消せます。Vim や OS shell の起動ではありません。
Tab 補完、矢印キーによる履歴、`delete`、`up`、`top`、`abort` も使用できます。
YAML のコメント・元の整形は保持しません。ローカルの TTY では `routerd cli` が同じ編集機能を使います。
大きな設定のファイル適用、network の詳細・ページ継続、読取中の revision 競合の扱いは
[CLI の契約](internal/cli/README.md) に記載しています。ページごとの観測はカーネル全体の atomic snapshot ではありません。

```sh
routerd config export > running-config.yaml
routerd config migrate old-routerd.yaml > candidate-routerd2.yaml
routerd config validate candidate-routerd2.yaml
```

`schema_version: 1` は routerd2 のモデルです。旧 resource YAML の無条件な互換ではありません。
移行器は未実装の暗黙依存や権限・入力制御の意味変更を拒否します。

## Architecture constraints

- `routerd` はプロセスの生存を監督し、networkだけが管理対象のカーネルオブジェクトを書き換えます。
- `/run/routerd/*.sock` はprivate Unix socketです。内部APIをTCPへ公開しません。
- 管理対象外のアドレス・経路・nftables tableは採用・削除しません。ownership journalの喪失は自動採用の根拠になりません。
- nftablesの一括更新以外は、複数のカーネル操作を一つのatomic transactionにはできません。復元失敗はdegradedとして残します。
- `commit confirmed` のrollbackも通信の無停止を保証しません。接続・conntrackの状態は設定のrollbackとは別です。
- 同じ release ID の 2 バイナリを署名した一組として保存・選択します。`routerd release help` がローカル管理入口です。boot の release 設定がある appliance は検証済み FD から PID 1 を引き継ぎ、連続した module/config readiness で確定します。live process tree の無停止置換、kernel/rootfs/設定 schema の移行は含みません。
- 初期実装のworkerは同じUIDで動きます。プロセス分離は最小権限化や完全なセキュリティ境界の完成を意味しません。

## License

新規コードは [BSD-3-Clause](LICENSE) です。
Linux、Alpineパッケージ、Goライブラリ、移植した既存 routerd コードは、それぞれのライセンスと表示義務を維持します。
独立プロセスであることだけを理由に、結合・配布に関する義務がなくなるとは扱いません。

`make licenses` は実際に使用する import graph と module graph を分け、各依存の
version/checksum、LICENSE/COPYING/COPYRIGHT/NOTICE/PATENTS とファイル hash を保存します。
選択コピーした元 routerd の DNS/BGP 実装にはその NOTICE を残します。
カーネルや追加 APK を配布するときの source 提供義務は、この manifest だけで完結しません。
