# DHCPv4・実CLOCK_REALTIME補正の隔離QEMU検証

2026-10-25 UTC。product `a136ae8e9af273f443d22b3fb1f7d045d56be674`、
fixture `c051be212521e7c40df0bf0072b5ec1940442a9e`、
Makefile／実行手順 `00c0d8ea106cfd15ba989196ca178b4e66fe962a`。
CORE-03／NET-05の個別受入を追加した。製品コードの追加変更はなく、全計画・対象実機の完了ではない。

## 実行境界

時計変更のsyscallは `images/kernel-test-init/clock_linux.go` のテスト専用PID 1だけにある。
通常のテストやproductionのrouterd／routerd-modulesは時刻を設定しない。
production両archの4 ELFは前回checked pairとSHAが一致し、production依存に `internal/clocktest` は含まれない。

- 実PID 1、固定suite `daemon-dhcp4-clock`、initramfs内の32桁nonce、同じbootのkernel引数が一致しなければclock IO前に拒否する。
- kernel引数の欠落・不一致・重複も拒否する。PID namespace内のPID 1や環境変数だけでは許可しない。
- PID 1が作ったprivate socketpairのFD 3をnetns試験へ渡す。clientはSO_PEERCREDのPID 1を要求し、fdを消費／close-on-execにしてdaemonへ渡さない。
- 要求は前進24時間、後退24時間、復元だけ。任意timestamp・未知field・過大frameを拒否する。
- 補正値は初期UTC＋実際のBOOT経過＋固定offset。既に補正されたUTCへ加算して累積させない。
- test終了・失敗・timeout後もPID 1がserverを止めて復元する。復元失敗はgate失敗にする。

QEMUは既存の512MiB／2 CPU、外部NICなし、private scratch64MiB。
専用fixtureはchild180秒／PID 1側210秒／QEMU250秒で、既存gateの期限は変更していない。
nativeの正のネットワーク試験はroot所有0700の一時stageとfresh user/network namespaceを使う。
ホーム／repositoryのmodeやホストのnetwork／clockを変更していない。

## 実daemon・kernel・packetの受入

実supervisor/control/network/DHCP workerと、独立したUDP DHCP server・TCP/UDP peerを起動する。

1. 20秒のleaseを取得し、kernel address/default routeと実TCP/UDPを確認する。
2. ACKを止め、実UTCを24時間前進させる。元のUTC expiresAtが過去でも、元BOOT leaseを変更せず通信が続くことを要求する。
3. 通常のconfig commitでWAN MTUを1400へ変更し、network writerの再適用後も同じgrant・kernel address/default route・TCP/UDPを要求する。
4. UTCを復元し、新しいACKで3秒leaseへ更新する。先行fixtureの3秒leaseは変更していない。
5. ACK停止＋元workerのSIGSTOP後にUTCを24時間後退させる。元BOOT期限でaddress/default routeを撤去する。
   expiry時にsupervisorが同じproducerをReadyと見ていることと、raw snapshotがまだActiveなことを確認し、heartbeat喪失による撤去と区別する。
6. 同一通知の再送で復活しないこと、UTC復元・worker再開後のfresh ACKによる復旧を確認する。
7. DHCP workerとcontrolをSIGKILLし、それぞれ復旧する。古い世代の通知は409、control復旧後のcacheは非active hintに留まり、fresh ACKだけで復旧する。
8. 外部所有のstatic address保持と、実DHCP／lease sourceからのTCP/UDP packet captureを要求する。

QEMU serialには前進 `24h0m0.087945346s`、後退 `-23h59m59.991034078s` と、
対応するBOOT経過88.46ms／8.97ms、復元2回とPID 1の最終復元を記録した。
ホストのwall-minus-monotonic差の変化は-570nsで、24時間stepはなかった。
これはゲスト内の実clock syscallの受入であり、NTP client/serverやslewの試験ではない。

## 結果と再現

| 検証 | 結果 |
| --- | --- |
| controller／transport／PID 1 cleanupのRace単体試験 | PASS（clock syscallはfake） |
| native・必須clock gateをbrokerなしで直接実行 | 0.07秒で期待どおり拒否（test exit 1） |
| native・通常の実DHCP authority、Race harness | 10.15秒PASS |
| QEMU・新しい実UTC補正gate | 37.74秒PASS（buildを含むhost51.58秒） |
| QEMU・通常DHCP gate、clock broker無効 | 34.14秒PASS（host46.11秒） |
| clean fixtureの全make check／Race／vet／Python24／両arch static build | 9.39秒PASS |
| Makefile／README追加後のclean再check/build | 7.36秒PASS（cache含む） |

新旧2つのQEMU試験はinitializer/test/kernel/production pairのSHAが一致する。
QEMU pairも前回の受入pairと一致し、native／arm64の4 ELFも前回と一致する。
全suiteを今回まとめて再実行したという意味ではない。

先行テストのmissing API／runner REDを保存した。
最初のtransport試験は1秒FAILだった。`net.FileConn`がduplicateした後もfixtureが元descriptorを保持し、
EOFを妨げていた。元descriptorを閉じる局所修正後にPASSした。期限延長や実clock操作で回避していない。

```sh
umask 022
make check build-linux RELEASE_ID=a136ae8e9af2
make qemu-dhcp4-clock IMAGE_DIR=dist/image
python3 scripts/qemu-network.py --suite daemon-dhcp4 --image-dir dist/image
```

QEMU用kernel/modulesを含むappliance image、QEMUとGoが必要。
ホストでpositive clock testを直接走らせない。brokerがなければ通常実行はSKIP、必須gate実行はFAILする。
unit/Race試験はホスト時刻を変更しない。

## 継続する残作業

実NTP discipline/slew、物理suspend、arm64実行、対象実機/NIC、長時間・性能は未検証。
IPv4 default routeの撤去はlive coordinatorによる。全controller/writer停止中の自律route失効を保証しない。
他protocolの期限/current provider、DHCP resource全体、抽出runtime/更新復旧、storage/effects/federation等を含む全残計画を続ける。
work packageとparity flagsは変更せず、継続ゴールをactiveのまま保持する。

[provenance](2026-10-25-dhcp4-clockstep/provenance.json) にsource checkpoint、元logのSHA、環境、変換、private PCAPのSHAを記録した。
guest captureはin-memory assertions、nativeのPCAP本体はprivate保存。ELF/initramfs/PCAP本体はcommitしない。
公開artifactのSHAは `2026-10-25-dhcp4-clockstep/SHA256SUMS` にある。
