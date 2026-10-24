# PVE Linux VM: Health → PBR 実daemon・実packet検証

日付: 2026-10-24 UTC。開始時の最新main: `8009262`。
実装・最終ローカルpacket gateのコードcheckpoint:
`7046fd5ab23d8860e0a8d70b3d8f47c52ae6da4f`。
着手時に未コミット変更はなく、mainをfast-forward確認した。
GitHub Actionsを待たず、ローカルで実装・修正・検証した。

## 実装した変更

1. 実supervisor/control/network/health worker、独立した二WAN peerとLAN clientを使う
   `TestDaemonHealthPBRFailoverAndRecovery` と必須target `make check-health-pbr`。
   skip、別名test、空run、偽PASS marker、空captureを合格にしない。
2. `GET /v1/services/resources` と運用modeの `show resources`。
   条件・readiness・依存待ちの理由、operator revisionとprojection generationを表示する。
   最大128件のページを観測digestで固定し、同じnetwork generation内のreadiness変更も409で検出する。
   candidate modeの `show resources` は引き続きsource宣言を表示する。
3. worker削除・再追加後のgeneration再利用を修正。
   以前はcontrol再起動後にgenerationが1へ戻り、古いgeneration 2の通知を受理した。
   supervisorの生存中は退役世代の単一watermarkを保持し、その上から採番する。
   nameごとの無制限なtombstone mapは作らない。
4. scoped Netlink lookupは `NETLINK_ROUTE` だけを開く。
   最小Alpine kernelで不要なXFRM socketが `protocol not supported` になる問題を修正。
5. storageは `nsfs` の `net:[inode]` 等の正当なmount rootを受理する。
   targetの絶対path・ext4 state volume・子mount競合の検査は維持する。
6. VRRPはaction途中の親context cancellationも正常停止として扱う。
   新しいcontextで撤去し、独立したaction errorや撤去失敗を隠さない。

## 環境と再現

- Host VM: Linux amd64、kernel `6.8.0-136-generic`、Go `1.27.3`。
- 実daemonと全test interfaceを新しいuser/network namespaceに置き、
  WAN-A、WAN-B、LANもそれぞれ独立させた。管理namespaceを変更していない。
- QEMU: 外部NICなし、Alpine kernel `6.18.58-0-virt`、TCG。
- VMのumask `0002` はGoのtest用秘密鍵directoryをgroup writableにするため、
  commandでは `umask 022` を指定した。製品の秘密鍵検査を緩和していない。
- raw ICMPはroot権限のtest executableで実行。通常userのraw socket失敗と区別する。

```sh
umask 022
make check
go test -c -race -tags integration -o dist/pve-validation/health.test ./internal/protocol/health
sudo -n dist/pve-validation/health.test -test.run '^TestScoped' -test.v
make build-linux
make check-health-pbr
sudo -n make image IMAGE_CONFIG=qemu
make qemu-network
make qemu-protocols
```

## 結果と証跡

| 検証 | 結果 |
| --- | --- |
| `make check` | PASS。全Go unit、依存expr、Python 13件、Race Detector、vet。[log](2026-10-24-pve-health-pbr/make-check.log) |
| scoped TCP / HTTP / DNS / raw ICMP | 全PASS。[log](2026-10-24-pve-health-pbr/health-sockets.log) |
| `make build-linux` | PASS。CGO無効のlinux/amd64とlinux/arm64。arm64実bootの合格とはしない |
| clean checkpointのHealth/PBR gate | PASS、`dirty: false`、14.87秒。[manifest](2026-10-24-pve-health-pbr/result.json)、[log](2026-10-24-pve-health-pbr/test.log) |
| packet capture | scoped probeとLANの実TCP/UDP。[WAN-A](2026-10-24-pve-health-pbr/wan0.pcap)、[WAN-B](2026-10-24-pve-health-pbr/wan1.pcap) |
| probeのmark | WAN別の独立nft OUTPUT counterでSYN全件がmark 42。markはpacket自体には載らない |
| 状態とkernel | 各転送検証でresource APIのactiveとtable 100の実gatewayを照合。[milestones](2026-10-24-pve-health-pbr/milestones.json) |
| QEMU Health/PBR | PASS、50.57秒。generation修正とresource APIを含む。[serial](2026-10-24-pve-health-pbr/qemu-health-pbr.log) |
| QEMU network gateと13 protocol suite | 個別PASS。required test、kernel/initializer/test/daemon hashを保存。[14 manifests](2026-10-24-pve-health-pbr/qemu-manifests.json) |

13 protocol suiteはresource-program、health-pbr、management、dhcp-dns、legacy-dhcp-dns、
bgp、daemon-bfd、dhcp、dhcp-server、vrrp、ra-client、aftr、native-bfd。
`make qemu-protocols` 初回はHealth lookupで停止した。修正後は失敗suiteと残りを
個別実行し、全suiteのPASSを取得した。一回のaggregate実行全体が合格したという主張ではない。
BFD連携の初回QEMU runはstartup timeoutとなったが、同じ実daemonのローカル試験と
QEMU単独再試験で合格。timeoutや受入条件は緩和していない。
VRRPのIPv6停止時cancellationはunitで決定的に再現して修正し、IPv4/IPv6の
election/failover/withdrawalをQEMUで再確認した。

## Health/PBR受入

| 条件 | 実際に確認した動作 |
| --- | --- |
| 正常時 | 二Health workerがHealthy。LANのTCP/UDP応答はWAN-A peerから返る |
| WAN-A probe障害 | WAN-Bにも同じprobe addressのdecoyを置いた状態でAだけUnhealthy。TCP/UDPはBへ切替 |
| 復旧 | healthy threshold到達後にTCP/UDPがAへ戻る |
| 世代 | 障害・復旧でoperator revision 1を保持し、適用投影のgenerationが進む |
| worker再起動 | Health/control/networkを個別SIGKILL。自動復旧し、foreign address/route/rule/nft tableとprobe host routesを保持 |
| 古い通知 | 古いsequence、Health再起動前、およびcontrol再起動前の通知を409で拒否 |
| fact expiry | 実control socketでsample期限を1.5秒へ短縮しproducerをSIGSTOP。supervisor-readyを維持したまま単調時計期限でprimaryを撤去。TCP/UDPはBへ流れる |
| replay | 同一再送はACKするが延命しない。sequenceだけ増やした失効sampleは422。新producerで復旧 |
| confirmed rollback | trialでAのmetricを300にしてBへ転送。未確認timeoutでsourceとrevision 3を復元し、実TCP/UDPもAへ戻る |

kernelのreverse-path検査でreplyを落とさないよう、fixtureはmarked table 101とmain table
双方に独立host routeを置く。application default routeをprobeに流用しない。
captureはinterface有効化後に開始する。worker停止後にkernelが出すsocketless RSTを
SO_MARK検証と取り違えず、probe開始時のSYNを数える。

## 証跡の扱いと残作業

公開PCAPは隔離fixtureの文書用・benchmark用アドレスのみを含む。
QEMU serialは公開kernel証明書のfingerprintを省略した。Gitleaksのgeneric-key検出に
該当した公開値であり、秘密鍵ではない。以後の公開artifact走査は検出なし。
`milestones.json` は生statusから必要な世代・閾値・期限・経路を抽出し、boot IDは含めない。
`result.json` はclean runの原manifestで、公開subsetのほかVMの
`dist/health-pbr-test/20261024T104250Z/` に保存した生statusのhashも記録する。
公開artifactのhash一覧は [SHA256SUMS](2026-10-24-pve-health-pbr/SHA256SUMS)。
新commitと追加source/evidenceは公開前にGitleaksで走査し、内容を確認した。

今回の合格は最小の条件付きroute/PBR構成に対するもの。
NET-01 / NET-02全体や全34 work packageの完了、full semantic parityは宣言しない。

- CORE-03: hold/maxAge、一般status reference、全protocol個別readinessとresource単位のeffects/lifecycle。
- CORE-04: durable journal/outbox/cursor、再送・dedup。supervisor自身の再起動を跨ぐ通知authorityも別途受入する。
- NET-01: targetSource/dynamic target、旧HealthCheck import、自然な2分のsample silence経路。今回のexpiryは短縮した実leaseによるfault injection。
- NET-02: 完全なEgressRoutePolicy、priority/mark/hash/hysteresis、nft mark/connmarkの自動生成、IPv6/source取得経路。
- 以降: 実装計画のWAN/LAN、HA、routing、SAM/federation/cloud、engine、配布imageと対象環境qualification。
- 全aggregate QEMU targetの最終同一tree一括実行、release/storage gate、物理arm64と実cloud qualificationは今回の合格に含めない。

読んだ主なsource: `internal/runtimeplan/{runtime,resource_conditions,status}.go`、
`internal/resourceprogram/{program,compile,projection,facts,condition}.go`、
`internal/protocol/health/{health,probe_linux}.go`、`internal/serviceruntime/{worker,health,bgp_bfd}.go`、
`internal/network/{lookup_linux,rules,routes_linux}.go`、`internal/integration/*_test.go`、
`internal/supervisor/{supervisor_linux,api_linux}.go`、`internal/storage/{mountinfo,files_linux}.go`、
`internal/protocol/vrrp/{run_linux,run_test,kernel_integration_test}.go`、
`internal/cli/{session,interactive}.go`、`scripts/qemu-network.py`、`images/kernel-test-init/main.go`、
`Makefile`、`images/README.md`、引き継ぎ文書と二つの実装計画。
