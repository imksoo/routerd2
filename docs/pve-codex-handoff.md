# routerd2 開発・PVE VM検証 引き継ぎ

更新日: 2026-10-24（Asia/Tokyo）
対象: `https://github.com/imksoo/routerd2`

**継続ゴール:** 全残実装・必要試験・対象実機での利用可能性の受入まで進める。
部分checkpointでは完了しない。最新コードcheckpointは `e46ce22`。
fact leafの `max_age` と元の観測時刻の保持を実装し、producer期限と独立した失効を
実daemon二WAN TCP/UDP/PCAPとQEMUで確認した。
[追加証跡](evidence/2026-10-24-condition-max-age.md) を参照。
次は `for` の安定時間、一般status reference等のCORE-03残作業を進める。

**PVE検証後の追記:** コードcheckpointは `7046fd5` へ進んだ。
`make check`、権限付きHealth実ソケット（raw ICMP含む）、二WANの実daemon/TCP/UDP/PCAP、
worker再起動・古い通知・fact expiry・confirmed rollbackを検証した。
`make check-health-pbr` とresource条件・保留理由のAPI/CLIを追加し、
最小kernelのNetlink lookup、generation再利用、nsfs mountinfo、VRRP停止を修正した。
詳細・未検証・残作業は [PVE検証証跡](evidence/2026-10-24-pve-health-pbr.md) と実装計画を参照。
以下は前環境からの引き継ぎ時点の履歴である。

実装コードのチェックポイント: **`834038edc7a9bf50e36f4f06fcf125f0aa2d88ff`**（mainへ反映済み）

## 1. 依頼と進め方

ユーザーの依頼は、`docs/implementation-plan.md` と `docs/implementation-plan.json` に従い、全機能の実装・検証を進めること。
開発とGitHub操作（mainへの反映を含む）は許可されている。通常の実装、テスト、commit/pushについて確認を繰り返さない。

**GitHub Actionsを待たず、ローカルで実装→関連テスト→修正を連続して進める。**
機能単位でRace Detector・vet・ビルドを確認してcommit/pushし、CIは別系統の統合検証として使う。
計画確認や定期タスクの設定だけで作業を終えない。

作業状態の正本はGitHubのコード、実装計画、検証証跡とする。チャットをまたいで別の計画を作り直さない。
既存の定期タスクが同時に動く可能性があるため、push前にremoteの進行を確認し、他の変更を上書きしない。

## 2. なぜPVE VMへ移すか

今回のクラウド実行環境では、次の操作が実際に拒否された。

- Unixソケットの作成・接続: `socket: operation not permitted`
- ICMP raw socketの作成: `listen ip4:icmp 127.0.0.1: socket: operation not permitted`

そのため、supervisor/control/networkの実プロセス連携、実daemon試験、raw ICMPを完走できない。
一方、通常のGo単体テスト、Race Detector、ビルド、loopbackのTCP/HTTP/DNSは実行できた。

**PVEホスト自体ではなく、使い捨て可能なLinux VMを開発・検証先にする。**
VMの管理経路を試験用ネットワークから分離し、netnsやQEMU内でWAN/LANを構成する。
本番WAN/HAの切替、本番ディスク初期化、有料クラウドの作成は今回の隔離検証と別扱いであり、勝手に対象を広げない。

## 3. 最新の実装内容

今回の開始点は `e4be8804fe70a3769523d188a8af1e86b26b5e77`。
以下の変更を `834038edc7a9bf50e36f4f06fcf125f0aa2d88ff` にまとめて反映した。

### CORE-03 / CORE-04: 条件とruntimeの接続

- ResourceProgramの `when` / `ready_when` を受け付け、三値評価を実際の投影へ接続した。
- 一つの投影内で、一貫したfact snapshotを使用する。
- `when` がfalseまたはindeterminateなら、そのresourceと依存・owner subtreeを保留・撤去する。
- `ready_when` は自身の起動を妨げず、依存resourceへの準備完了を制御する。
- supervisorの `service.NAME.ready` と、受理済みprotocol snapshotの `service.NAME.active` を評価する。
- boot ID、source digest、operator revision、worker generation、producer epoch・sequence、CLOCK_BOOTTIME期限で古い観測を排除する。
- 条件付き設定のprotocol callbackは観測を受理し、serialized reconcile loopがworker停止→network ACK→worker起動の順で適用する。
- 停止中にfactが変化した場合は再評価し、同一設定のworkerでも既に停止済みなら再起動する。
- confirmed commitの確認では、ready_whenと適用済み投影への収束も確認する。
- operator revisionとは別に、成功したnetwork projection generationをstatus/healthへ出す。永続復元や全worker世代管理の完成を意味しない。

主なファイル:

- `internal/resourceprogram/projection.go`
- `internal/runtimeplan/resource_conditions.go`
- `internal/runtimeplan/runtime.go`
- `internal/runtimeplan/resource_conditions_test.go`

### NET-01: Health workerの最初の実装

- `service.health` をserviceconfig、supervisor起動設定、worker dispatcher、runtime snapshot受理へ接続した。
- ICMP/TCP/DNS/HTTPに対応。interfaceを必須とし、source address・markを指定できる。
- targetは現段階ではIPv4/IPv6のunicast literal。動的targetSourceは未実装として扱う。
- TCP/DNS/HTTP probeと閾値判定は固定済みrouterd実装を再利用する。
- ICMPはinterface/markを設定したraw socketを使用し、送信先、echo ID、sequence、乱数payloadを照合する。
- Passing→Healthy、Failing→Unhealthyと復旧の閾値を扱い、historyは20件に制限する。
- `.active` は旧egress契約に合わせ、Healthyおよび閾値未到達のFailingでtrueとする。Passing/Unhealthyはfalse。未観測・失効はindeterminate。
- 有効期間は `max(2分, 3×interval + timeout)`。CLOCK_BOOTTIMEの期限とsupervisor世代の双方で失効させる。
- 停止時はwithdrawalを発行する。単なる再送で期限を延長しない。

主なファイル:

- `internal/protocol/health/health.go`
- `internal/protocol/health/probe_linux.go`
- `internal/protocol/health/README.md`
- `internal/serviceruntime/health.go`

### 経路確認と最小Health→PBR

- network workerにread-only `POST /v1/routes/lookup` を追加した。
- target/interface/source/markを指定したNetlink lookupを行う。外部の `ip` コマンドを呼ばない。
- 経路情報の取得はbest effort。取得失敗だけをprobe失敗に変換しない。
- `examples/health-pbr.yaml` を追加した。
- probe用host routeをapplication用tableから独立させ、Healthによるprimary経路の追加・撤去、常設backup経路、source PBR ruleを組み合わせた。
- **これは最小構成であり、EgressRoutePolicy全体のpriority/mark/hash/hysteresis互換を完成したものではない。**

### 併せて修正した不具合

- 再帰的な条件式の型がCLI補完候補を使い切る問題: 入力prefixに沿って候補を探索するよう修正。
- JST等では負のUnix時刻が1970年に見え、期限検証を通る問題: route/addressの期限をUnix時刻とUTCで検証するよう修正。

## 4. 実行済み検証と未検証を区別する

使用環境: Linux amd64、**Go 1.27.3**。実リポジトリをcloneし、実際の型・依存関係で実行した。代替型による独立テストではない。

| 検証 | 結果 |
| --- | --- |
| network / health / runtimeplan / resourceprogram / serviceconfig / serviceruntime / config / control / cli / cmd-routerdのテスト・Race Detector | PASS |
| `go vet ./...` | PASS |
| `CGO_ENABLED=0 go build ./cmd/routerd ./cmd/routerd-modules ./cmd/routerd-bundle` | PASS |
| `go test -run '^$' ./...` | 全packageのテストコードをcompile成功。テスト実行の成功とは区別 |
| loopbackの実TCP・HTTP・DNS probe | PASS |
| raw ICMP integration | socket作成が環境に拒否され失敗 |
| `go test ./...` 全体 | 実行したが未合格。Unixソケット依存の複数suiteが環境制限で失敗。DHCP server readinessもtimeout |
| 実supervisor/control/networkを使うHealth→PBR | 未検証 |
| 二WANの実TCP/UDP、packet capture、障害・復旧 | 未検証 |
| 今回の変更に対するGitHub Actions | 待機・合否確認をしていない |

runtimeのテストは本物のcoordinatorコードと、in-memoryのnetwork/supervisor境界を使う。
これを実kernelへの適用や実パケット転送の証拠として扱わない。

GitHubへ反映したtreeは `ee5d5f60d9d3479117c450e2f4dc81aec043a8b3`。
ローカルで検証したtreeとの完全一致を確認した。本引き継ぎ文書の追記は、そのコードチェックポイントに対するdocs-only更新である。

## 5. PVE VM側の最初の実行手順

### 5.1 作業環境

- 既存checkoutの未コミット変更を確認して保護する。自動resetや上書きをしない。
- checkout内にAGENTS.mdがあれば読む。
- GitHubの最新mainを取得し、上記コードチェックポイントを含むことを確認する。
- 検証したcommit SHA、OS/kernel、Go版、権限、実行ログを記録する。
- Go 1.27.3、C compiler、Python、Gitを準備する。
- Unixソケット、network namespace、CAP_NET_ADMIN、CAP_NET_RAWが利用可能であることを確認する。

```sh
git status --short
git fetch origin
git log -5 --oneline origin/main
git rev-parse HEAD
go version
```

cleanなcheckoutを用意したうえで、通常のfast-forward更新または専用worktreeを使う。
最新mainに後続変更があれば、今回のチェックポイントへ無条件に戻さない。

### 5.2 最初に閉じるローカル検証

```sh
make check
go test -race -tags integration ./internal/protocol/health -run '^TestScoped' -count=1 -v
make build-linux
```

`make check` はformat、全Goテスト、依存ライブラリのテスト、Python tool tests、Race Detector、vetを実行する。
失敗したら原因を修正する。skip追加や期待値の緩和で合格にしない。
特にクラウド側で実行できなかったUnixソケット・supervisor・ICMPのsuiteを確認する。

### 5.3 既存のkernel/QEMU gate

`images/README.md` とMakefileを読み、Docker・QEMU・image toolchainを準備する。

```sh
make image IMAGE_CONFIG=qemu
make qemu-network
make qemu-protocols
```

これらはローカルで実行する。GitHub Actions完了待ちへ切り替えない。
既存gateは今回追加した二WAN Health/PBRの受入試験そのものではない。

## 6. 最優先で追加する実daemon・実パケット試験

独立したWAN-A/WAN-B peerとLAN clientを持つnetnsまたはQEMU labを作り、実際のrouterd supervisor/control/network/health workerを起動する。
`examples/health-pbr.yaml` をlabのinterface/addressへ合わせる。

1. probeの送信interface/source/markがWAN-Aの指定と一致することをcaptureする。
2. WAN-Aを遮断しても、probeがWAN-Bへ回り込んで成功しないことを確認する。
3. LANからの実TCP/UDPが正常時はprimaryへ流れることを確認する。
4. unhealthy threshold到達後にbackupへ切り替わることを確認する。
5. 復旧後、healthy thresholdを満たしてprimaryへ戻ることを確認する。
6. probe用host routeとPBR rule、無関係な所有者のobjectが途中で失われないことを確認する。
7. health/control workerのkill/restart、遅延した旧世代通知、fact expiry、confirmed rollbackを試す。
8. operator revisionとprojection generation、表示状態と実経路が整合することを確認する。
9. テストをrequiredなlocal targetへ組み込み、PASS marker・capture・ログ・対象commitを残す。

失敗・未実行を成功として扱わず、問題を直してから対応する受入条件を閉じる。

## 7. その後の残作業

全34作業単位の完了には至っていない。計画書のwork packageとacceptance gateを正本として続ける。

- **CORE-03:** hold/maxAge、一般status reference、全protocolのreadiness/lifecycle、resourceごとの状態表示、実daemonでの受入。
- **CORE-04:** durable event journal、consumer cursor、outbox、再送・dedup。transient factとは別の保持契約で実装する。
- **NET-01:** targetSource/dynamic target、旧HealthCheck import、二経路probeと経路証跡の実パケット受入。
- **NET-02:** 完全なEgressRoutePolicy、priority/mark/hash、hysteresis、nft mark/connmark、table/ruleの所有権と実転送。
- 以降は計画のWAN/LAN、HA、BGP/BFD/WG、SAM/federation/cloud、運用surface、配布image、最終qualificationを順に進める。

`parity.json` のsemantic parityは今回の変更で上げていない。
実装の存在、mock試験、実daemon動作、実kernel/packet検証、対象環境qualificationを別々に記録する。

## 8. PVE側Codexへの開始指示

> imksoo/routerd2の最新mainを取得し、docs/pve-codex-handoff.mdと実装計画を読んで開発を継続してください。開発およびGitHub操作は許可済みです。GitHub Actionsを待たず、PVEの隔離Linux VMでローカルビルド・単体テスト・実daemon・実パケット試験を進めてください。最初にクラウド環境で阻まれたUnixソケットとraw ICMPを含む検証を閉じ、次に二WANのHealth→PBR障害切替・復旧を実証してください。その後も全実装計画を継続し、計画確認だけで停止しないでください。
