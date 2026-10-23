# routerd2

Linuxカーネルの上で動く、モジュール化されたルーターの実装です。
`routerd` 自身を PID 1 とし、ネットワーク設定・制御・SSH管理を別プロセスに分けます。
Alpineのパッケージをビルドとイメージ組み立てに利用します。

**開発中です。現行 [routerd](https://github.com/imksoo/routerd) の全機能互換は移植の目標であり、現時点の完成状態ではありません。**
移植元の基準は `1329ef54f296507eda79bbbc3b853783dd27251e` です。
既存のhomert02/03を置き換える前に、WAN取得・HA・既存設定との互換試験が必要です。

## 実装されている基盤

| 層 | 実装 |
| --- | --- |
| init / supervisor | 明示的なappliance・container・hostモード、PID 1、子プロセス回収、終了処理、再起動backoff、heartbeat、同一releaseの検証 |
| worker runtime | 一つの `routerd-modules` 実行ファイルを役割別のプロセスとして起動。親との専用FD、世代ごとの状態 |
| network | NetlinkによるIPv4/IPv6アドレス・静的経路・MTU・リンク状態、forwarding sysctl、nftablesによるforward filterとmasquerade |
| DS-Lite | 固定IPv6 endpointを使うカーネルIPv4-in-IPv6トンネル。AFTR探索・DHCPv6取得は別途移植 |
| configuration | YAML / JSON、厳密な検証、楽観的世代管理、冪等リクエスト、永続journal、失敗時の旧desiredへのreconcile |
| confirmed commit | 時間内に確認しない変更を戻す。未確認のままcontrolが再起動した場合も戻す |
| management | 内蔵SSH、公開鍵認証、永続host key、OSシェルを呼ばないtyped CLI、Unix socket API |
| images | static amd64/arm64バイナリ、scratch OCI image、Alpine APK、Linux kernel + initramfs、QEMU検証スクリプト |

アプライアンスの通常起動には、systemd、journald、sshd、OpenRC、`ip`、`nft`、シェルを使いません。
パケット転送はLinuxカーネルが行います。ユーザーランドでIPv4/IPv6スタックを独自再実装する設計ではありません。

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
現行のQEMUイメージはdirect boot用initramfsです。A/B bootloader、永続state partitionのセットアップ、AMIや各クラウドのGolden Imageはまだ含みません。
initramfsだけで動かす場合、設定journalとSSH host keyは再起動で消えます。
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

実行中設定の変更はcontrol APIのトランザクションを通します。
`schema_version` はrouterd2の初期モデルであり、元のrouterdのresource YAMLをそのまま受理する意味ではありません。
全resourceモデルの移植と互換性は明示的に追跡します。

## Architecture constraints

- `routerd` はプロセスの生存を監督し、networkだけが管理対象のカーネルオブジェクトを書き換えます。
- `/run/routerd/*.sock` はprivate Unix socketです。内部APIをTCPへ公開しません。
- 管理対象外のアドレス・経路・nftables tableは採用・削除しません。ownership journalの喪失は自動採用の根拠になりません。
- nftablesの一括更新以外は、複数のカーネル操作を一つのatomic transactionにはできません。復元失敗はdegradedとして残します。
- `commit confirmed` のrollbackも通信の無停止を保証しません。接続・conntrackの状態は設定のrollbackとは別です。
- 同じrelease IDの2バイナリを一組として配布します。現段階にlive image switchや署名済みA/B更新はありません。
- 初期実装のworkerは同じUIDで動きます。プロセス分離は最小権限化や完全なセキュリティ境界の完成を意味しません。

## License

新規コードは [BSD-3-Clause](LICENSE) です。
Linux、Alpineパッケージ、Goライブラリ、今後移植する既存routerdコードは、それぞれのライセンスと表示義務を維持します。
独立プロセスであることだけを理由に、結合・配布に関する義務がなくなるとは扱いません。
